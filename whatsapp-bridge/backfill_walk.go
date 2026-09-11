// backfill_walk.go — walking backwards through a chat's history so the
// MYC-3284 content backfill can reach past the most recent window.
//
// Why this exists. RequestChatHistory anchors on the NEWEST message in a chat
// and asks WhatsApp for the `count` (max 200) messages before it, so calling it
// repeatedly returns the same window forever. Measured on the live bridge
// 2026-08-01: the first sweep recovered 251 rows, then a further sweep of 40
// chats × 200 delivered 8 more chunks and recovered 0 additional rows. Every
// empty row older than ~200 messages was simply unreachable.
//
// The walk fixes that by feeding the OLDEST message of each delivered chunk
// back in as the next anchor, stepping backwards one window at a time.
//
// The hard part is not the stepping, it is the STOPPING. History arrives
// asynchronously over the events.HistorySync stream, so the walk cannot be a
// loop with a condition at the top — each delivery has to decide whether to
// issue the next request. That shape is self-propelling, and a self-propelling
// process that talks to someone else's server must be bounded by construction,
// not by expecting the right reply. Four independent brakes, any one of which
// halts a chat:
//
//  1. maxRounds        — a hard per-chat ceiling on requests.
//  2. no-progress      — the new chunk's oldest message is not older than the
//                        anchor we asked about, so we are not advancing. This
//                        is the brake that catches "WhatsApp keeps replaying
//                        the same window", the exact failure that motivated
//                        the walk.
//  3. nothing-left     — no empty rows remain older than where we have reached,
//                        so continuing would burn requests for no repair.
//  4. global budget    — a cap across ALL chats, so N concurrent walks cannot
//                        multiply into an unbounded request storm.
//
// State is in-memory on purpose. It is a progress cursor, not a fact about the
// world: losing it on restart costs one redundant window, and the backfill's
// UPDATE is idempotent, so a replayed window is a no-op rather than damage.

package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const (
	// defaultMaxWalkRounds bounds requests per chat per sweep. At 200 messages
	// a window this reaches ~4,000 messages back, which covers the deepest
	// chat in the live store (6,245 empty rows, most of them protocol
	// carriers) without unbounded paging.
	defaultMaxWalkRounds = 20

	// defaultWalkBudget bounds requests across ALL chats for one sweep, so a
	// wide sweep cannot multiply per-chat rounds into a request storm.
	defaultWalkBudget = 400

	// walkStaleAfter releases per-chat state when WhatsApp never answers, so
	// an unanswered request cannot pin a chat as "walking" forever and block
	// a later sweep from retrying it.
	walkStaleAfter = 15 * time.Minute
)

// walkState is one chat's position in its backwards walk.
type walkState struct {
	rounds     int
	anchorTS   int64  // timestamp of the anchor we last asked about
	anchorID   string // id of that anchor — see next(): same second ≠ same message
	lastReqAt  time.Time
	perChat    int
	stopReason string

	// mode selects which brakes apply. "repair" is the MYC-3284 content
	// backfill: stop when no empty rows remain further back (brake 3).
	// "older" is a plain history fetch requested through the API: there is
	// nothing to repair, so brake 3 must not apply or the walk would stop
	// after one window every time. Brakes 1, 2 and 4 apply to both.
	mode string

	// maxRounds, when > 0, caps this chat's rounds independently of the
	// walker-wide ceiling. Set per request so an ad-hoc "fetch older" walk
	// can be bounded by its caller.
	maxRounds int
}

// backfillWalker coordinates the walks. One instance per Bridge.
type backfillWalker struct {
	mu    sync.Mutex
	chats map[string]*walkState

	spent     int // requests issued this sweep, against budget
	budget    int
	maxRounds int

	// Totals for the sweep, reported back so an operator sees where it stopped
	// rather than inferring it from silence.
	stopped map[string]int // stopReason -> count
}

func newBackfillWalker() *backfillWalker {
	return &backfillWalker{
		chats:     map[string]*walkState{},
		stopped:   map[string]int{},
		budget:    defaultWalkBudget,
		maxRounds: defaultMaxWalkRounds,
	}
}

// reset clears state and re-arms the budget for a new sweep.
func (w *backfillWalker) reset(budget, maxRounds, perChat int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	dropped := 0
	for _, st := range w.chats {
		if st.mode == "older" {
			dropped++
		}
	}
	if dropped > 0 {
		log.Printf("backfill walk: reset dropped %d in-flight API history walk(s)", dropped)
	}
	w.chats = map[string]*walkState{}
	w.stopped = map[string]int{}
	w.spent = 0
	// Fall back to the defaults rather than keeping whatever an API walk
	// extended the budget to (review: inflated budget survived into sweeps).
	if budget > 0 {
		w.budget = budget
	} else {
		w.budget = defaultWalkBudget
	}
	if maxRounds > 0 {
		w.maxRounds = maxRounds
	} else {
		w.maxRounds = defaultMaxWalkRounds
	}
	_ = perChat
}

// isActive reports whether a walk is registered for key.
func (w *backfillWalker) isActive(key string) bool {
	return w.anyActive([]string{key})
}

// anyActive reports whether a walk is registered under ANY of the keys —
// callers pass a JID plus its aliases, so a repair walk under the LID form
// and an API walk under the phone form cannot run side by side and steal
// each other's chunks (Codex review, 2026-09-02).
func (w *backfillWalker) anyActive(keys []string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range keys {
		if _, ok := w.chats[k]; ok {
			return true
		}
	}
	return false
}

// release drops a walk without it having reached a brake — used when the
// request that started it never left the bridge.
func (w *backfillWalker) release(key, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.chats[key]; ok {
		delete(w.chats, key)
		w.stopped[reason]++
	}
}

// begin registers the first request for a chat. Returns false when the global
// budget is exhausted, so the caller does not issue the request at all.
func (w *backfillWalker) begin(chatJID string, anchorTS int64, perChat int) bool {
	return w.beginMode(chatJID, anchorTS, perChat, "repair", 0)
}

// beginMode is begin with an explicit mode and an optional per-chat round cap
// (0 = walker default). See walkState.mode for what the modes mean.
func (w *backfillWalker) beginMode(chatJID string, anchorTS int64, perChat int, mode string, maxRounds int) bool {
	return w.beginModeID(chatJID, anchorTS, "", perChat, mode, maxRounds)
}

// beginModeID is beginMode with the anchor's message id recorded, so a chunk
// that lands on the same second but a different (older) message still counts
// as progress. WhatsApp timestamps are whole seconds; several messages in
// one second is ordinary (Codex review, 2026-09-02).
func (w *backfillWalker) beginModeID(chatJID string, anchorTS int64, anchorID string, perChat int, mode string, maxRounds int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.spent >= w.budget {
		w.stopped["global_budget"]++
		return false
	}
	if mode == "" {
		mode = "repair"
	}
	w.spent++
	w.chats[chatJID] = &walkState{
		rounds:    1,
		anchorTS:  anchorTS,
		anchorID:  anchorID,
		lastReqAt: time.Now(),
		perChat:   perChat,
		mode:      mode,
		maxRounds: maxRounds,
	}
	return true
}

// claimOlderWalk is the request gate for an API-initiated "older" walk. Under
// ONE lock it (1) expires any registration among keys whose last request went
// unanswered for longer than walkStaleAfter, (2) refuses with errWalkActive if
// a fresh registration remains under any key, (3) raises the global budget by
// maxRounds so the walk is not starved by earlier sweeps, and (4) registers
// the walk under chatJID.
//
// Why one lock. RequestOlderHistory used to do these as separate calls
// (anyActive, extendBudget, beginModeID), so two concurrent API requests for
// the same contact could both pass the active check and both register, the
// second silently overwriting the first's cursor. And nothing on the request
// path ever called sweepStale, which only ran when some history chunk arrived
// or a repair sweep reset everything — so a walk whose request WhatsApp never
// answered stayed registered indefinitely and every later request got 409
// (Fable review, 2026-09-10, finding B1).
//
// Expiry is bounded to keys: only registrations for THIS contact (its JID and
// aliases) are released, so a stale repair walk on another chat is left for the
// next chunk-arrival sweep exactly as before. Expiry is driven by the request,
// not by a timer.
func (w *backfillWalker) claimOlderWalk(keys []string, chatJID string, anchorTS int64, anchorID string, perChat, maxRounds int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for _, k := range keys {
		st, ok := w.chats[k]
		if !ok {
			continue
		}
		if now.Sub(st.lastReqAt) > walkStaleAfter {
			w.stopped["unanswered"]++
			delete(w.chats, k)
			log.Printf("backfill walk: %s expired an unanswered walk (last request %s ago)", k, now.Sub(st.lastReqAt).Round(time.Second))
			continue
		}
		return errWalkActive
	}
	if maxRounds > 0 && w.spent+maxRounds > w.budget {
		w.budget = w.spent + maxRounds
	}
	if w.spent >= w.budget {
		w.stopped["global_budget"]++
		return errors.New("walk budget exhausted")
	}
	w.spent++
	w.chats[chatJID] = &walkState{
		rounds:    1,
		anchorTS:  anchorTS,
		anchorID:  anchorID,
		lastReqAt: now,
		perChat:   perChat,
		mode:      "older",
		maxRounds: maxRounds,
	}
	return nil
}

// extendBudget raises the global budget by n so an ad-hoc walk started from
// the API is not refused just because earlier sweeps spent the sweep budget.
// The budget is a brake against runaway self-propelled walks, not a quota on
// operator-initiated ones; each API walk is separately capped by maxRounds.
func (w *backfillWalker) extendBudget(n int) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.spent+n > w.budget {
		w.budget = w.spent + n
	}
}

// resolveKey returns the first candidate JID that has an active walk, or "".
// History chunks come back under whichever JID form WhatsApp chose for the
// conversation (LID or phone), which need not be the form the walk was
// registered under; callers pass every alias so the walk is found either way.
func (w *backfillWalker) resolveKey(candidates []string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range candidates {
		if _, ok := w.chats[c]; ok {
			return c
		}
	}
	return ""
}

// walkDecision is what a delivered chunk should cause next.
type walkDecision struct {
	Continue bool
	Reason   string // why it stopped, when Continue is false
	PerChat  int
}

// next decides whether a chat keeps walking after a chunk arrived whose oldest
// message is chunkOldestTS. hasOlderEmpties reports whether any empty row
// remains strictly older than that.
//
// Every brake lives here, in one place, so the stop conditions can be read and
// tested together rather than being scattered through the delivery path.
func (w *backfillWalker) next(chatJID string, chunkOldestTS int64, hasOlderEmpties bool) walkDecision {
	return w.nextID(chatJID, chunkOldestTS, "", hasOlderEmpties)
}

// nextID is next() with the delivered chunk's oldest message id. Progress is
// "strictly older timestamp" OR "same timestamp but a different message than
// the anchor". Same timestamp AND same id (or no id) is the replay case.
func (w *backfillWalker) nextID(chatJID string, chunkOldestTS int64, chunkOldestID string, hasOlderEmpties bool) walkDecision {
	w.mu.Lock()
	defer w.mu.Unlock()

	st, ok := w.chats[chatJID]
	if !ok {
		// Not a chat we are walking (e.g. an unsolicited history chunk, or a
		// chunk arriving after the walk was released). Do nothing.
		return walkDecision{Continue: false, Reason: "not_walking"}
	}

	stop := func(reason string) walkDecision {
		st.stopReason = reason
		w.stopped[reason]++
		delete(w.chats, chatJID)
		return walkDecision{Continue: false, Reason: reason}
	}

	// Brake 2: we asked for messages before anchorTS and got back a window
	// whose oldest message is not older than that. We are not advancing, so
	// stepping again would replay the same window indefinitely.
	sameSecondNewMsg := chunkOldestTS == st.anchorTS && chunkOldestID != "" && st.anchorID != "" && chunkOldestID != st.anchorID
	if chunkOldestTS <= 0 || chunkOldestTS > st.anchorTS || (chunkOldestTS == st.anchorTS && !sameSecondNewMsg) {
		return stop("no_progress")
	}
	// Brake 3: nothing left worth fetching further back. Repair mode only —
	// a plain "older" fetch has nothing to repair, so this would always fire.
	if st.mode != "older" && !hasOlderEmpties {
		return stop("nothing_older")
	}
	// Brake 1: per-chat ceiling (per-request cap wins when set).
	limit := w.maxRounds
	if st.maxRounds > 0 {
		limit = st.maxRounds
	}
	if st.rounds >= limit {
		return stop("max_rounds")
	}
	// Brake 4: global budget across all chats.
	if w.spent >= w.budget {
		return stop("global_budget")
	}

	w.spent++
	st.rounds++
	st.anchorTS = chunkOldestTS
	if chunkOldestID != "" {
		st.anchorID = chunkOldestID
	}
	st.lastReqAt = time.Now()
	return walkDecision{Continue: true, PerChat: st.perChat}
}

// sweepStale releases chats whose last request was never answered, so they do
// not stay pinned as "walking" and block a later sweep.
func (w *backfillWalker) sweepStale() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for jid, st := range w.chats {
		if time.Since(st.lastReqAt) > walkStaleAfter {
			w.stopped["unanswered"]++
			delete(w.chats, jid)
		}
	}
}

// stats snapshots progress for the API response.
func (w *backfillWalker) stats() (active, spent, budget int, stopped map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.stopped))
	for k, v := range w.stopped {
		out[k] = v
	}
	return len(w.chats), w.spent, w.budget, out
}

// --- bridge integration ----------------------------------------------------

// hasEmptyRowsOlderThan reports whether the chat still holds pre-MYC-3284
// empty rows strictly older than ts. This is the "is another window worth
// fetching" question, and answering it from the store is what keeps the walk
// from paging through history that has nothing left to repair.
func (b *Bridge) hasEmptyRowsOlderThan(ctx context.Context, chatJID string, ts int64) (bool, error) {
	jids, err := resolveAliases(ctx, b.db, chatJID)
	if err != nil || len(jids) == 0 {
		jids = []string{chatJID}
	}
	args := append(jidsToArgs(jids), ts)
	var n int
	err = b.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM messages
			 WHERE chat_jid IN (`+inClausePlaceholders(len(jids))+`)
			   AND timestamp < ?
			   AND type = 'system'
			   AND (content_text IS NULL OR content_text = '')
		)
	`, args...).Scan(&n)
	return n == 1, err
}

// continueWalk is called once per conversation in a delivered history chunk.
// It decides whether to step further back and, if so, issues the next request
// anchored on that chunk's oldest message.
func (b *Bridge) continueWalk(ctx context.Context, chatJID string, oldest chunkAnchor) {
	if b.walker == nil {
		return
	}
	// Opportunistic stale sweep: walks whose requests were never answered
	// must not stay pinned until the next repair sweep happens to run.
	b.walker.sweepStale()

	// The walk may have been registered under an alias of the JID WhatsApp
	// delivered the chunk under (LID vs phone form). Look it up under every
	// known form; if none is walking, this chunk was unsolicited.
	candidates := []string{chatJID}
	if aliases, aerr := resolveAliases(ctx, b.db, chatJID); aerr == nil {
		for _, a := range aliases {
			if a != chatJID {
				candidates = append(candidates, a)
			}
		}
	}
	key := b.walker.resolveKey(candidates)
	if key == "" {
		return
	}
	if oldest.ID == "" {
		// A chunk with no usable timestamps cannot re-anchor; stop the walk
		// explicitly instead of leaving it registered forever.
		d := b.walker.next(key, 0, false)
		log.Printf("backfill walk: %s stopped (%s: chunk carried no anchor)", chatJID, d.Reason)
		return
	}

	hasOlder, err := b.hasEmptyRowsOlderThan(ctx, chatJID, oldest.TS)
	if err != nil {
		log.Printf("backfill walk: older-empties check for %s failed: %v", chatJID, err)
		return
	}

	d := b.walker.nextID(key, oldest.TS, oldest.ID, hasOlder)
	if !d.Continue {
		if d.Reason != "not_walking" {
			log.Printf("backfill walk: %s stopped (%s)", chatJID, d.Reason)
		}
		return
	}

	// Send off the event goroutine. continueWalk runs inside whatsmeow's
	// synchronous event dispatch, and RequestHistoryBefore does a network send
	// that can block for seconds — holding the dispatch there would stall
	// delivery of every other event, including the very history chunks this
	// walk is waiting on.
	//
	// Spawning here is bounded, not open-ended: the walker already decremented
	// the global budget before returning Continue, so the number of goroutines
	// this can create over a sweep is capped by that same budget.
	go func() {
		reqCtx, cancel := context.WithTimeout(b.rootCtx, 30*time.Second)
		defer cancel()
		if _, err := b.RequestHistoryBefore(reqCtx, chatJID, oldest.ID, oldest.TS, oldest.FromMe, d.PerChat); err != nil {
			log.Printf("backfill walk: next request for %s failed: %v", chatJID, err)
			return
		}
		log.Printf("backfill walk: %s stepping back before %s (ts=%d)", chatJID, oldest.ID, oldest.TS)
	}()
}

// chunkAnchor identifies the oldest message in a delivered history chunk,
// which becomes the anchor for the next step backwards.
type chunkAnchor struct {
	ID     string
	TS     int64
	FromMe bool
}

// WalkStats exposes walk progress for the API.
func (b *Bridge) WalkStats() (active, spent, budget int, stopped map[string]int) {
	if b.walker == nil {
		return 0, 0, 0, map[string]int{}
	}
	return b.walker.stats()
}

// releaseWalkFor drops any walk registered for chatJID or one of its aliases.
// Called on delivery-path errors that skip continueWalk, so a transient DB
// failure cannot leave a chat pinned as "walking" and answering 409 until a
// stale sweep happens to reset it (Codex review, 2026-09-02).
func (b *Bridge) releaseWalkFor(ctx context.Context, chatJID, reason string) {
	if b.walker == nil {
		return
	}
	keys := []string{chatJID}
	if al, err := resolveAliases(ctx, b.db, chatJID); err == nil && len(al) > 0 {
		keys = al
	}
	for _, k := range keys {
		b.walker.release(k, reason)
	}
}
