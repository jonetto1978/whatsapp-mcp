package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// The request gate for an API "older" walk (RequestOlderHistory) is what a
// caller hits on every request_history call. These tests drive the REAL gate —
// anchor lookup, alias resolution, stale expiry, conflict, registration — with
// the network send replaced by a counter, so no WhatsApp traffic is generated.
//
// Regression (Fable review 2026-09-10, B1): a walk whose request WhatsApp never
// answered stayed registered forever, because nothing on the request path ever
// expired it; every later request for that contact got errWalkActive (409).

const (
	gatePhone = "5491100000000@s.whatsapp.net"
	gateLID   = "123456789@lid"
)

// gateBridge builds a Bridge that passes IsConnected, holds one contact under
// its phone JID with a LID alias, one message to anchor on, and a history send
// that only counts.
func gateBridge(t *testing.T) (*Bridge, *int32) {
	t.Helper()
	db, err := sql.Open("sqlite3", t.TempDir()+"/gate.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	insertChat(t, db, gatePhone, "direct", "")
	insertChat(t, db, gateLID, "direct", "")
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, timestamp, type, content_text) VALUES ('OLDEST', ?, 1000, 'text', 'hi')`, gatePhone); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO jid_aliases (jid_a, jid_b, discovered_at, source) VALUES (?, ?, 0, 'test'), (?, ?, 0, 'test')`,
		gatePhone, gateLID, gateLID, gatePhone); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	b := &Bridge{db: db, walker: newBackfillWalker(), connected: true, authenticated: true}

	var sends int32
	prev := sendHistoryBefore
	sendHistoryBefore = func(_ *Bridge, _ context.Context, _, _ string, _ int64, _ bool, _ int) (whatsmeow.SendResponse, error) {
		atomic.AddInt32(&sends, 1)
		return whatsmeow.SendResponse{ID: "REQ"}, nil
	}
	t.Cleanup(func() { sendHistoryBefore = prev })
	return b, &sends
}

// ageWalk moves a registration's last request into the past.
func ageWalk(t *testing.T, w *backfillWalker, key string, by time.Duration) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.chats[key]
	if !ok {
		t.Fatalf("no walk registered under %s", key)
	}
	st.lastReqAt = time.Now().Add(-by)
}

func TestOlderRequestRefusesFreshWalkAcrossAliases(t *testing.T) {
	b, sends := gateBridge(t)
	ctx := context.Background()

	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if got := atomic.LoadInt32(sends); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	// Same contact, same form.
	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); !errors.Is(err, errWalkActive) {
		t.Fatalf("second request (phone): err = %v, want errWalkActive", err)
	}
	// Same contact, alias form: still one human, still refused.
	if _, _, err := b.RequestOlderHistory(ctx, gateLID, 50, true, 3); !errors.Is(err, errWalkActive) {
		t.Fatalf("second request (lid): err = %v, want errWalkActive", err)
	}
	if got := atomic.LoadInt32(sends); got != 1 {
		t.Fatalf("sends = %d after refused requests, want 1 (no double-spend)", got)
	}
	if active, _, _, _ := b.walker.stats(); active != 1 {
		t.Fatalf("active = %d, want 1", active)
	}
}

func TestOlderRequestExpiresStaleWalk(t *testing.T) {
	b, sends := gateBridge(t)
	ctx := context.Background()

	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); err != nil {
		t.Fatalf("first request: %v", err)
	}
	// WhatsApp never answered: no chunk arrived, so nothing swept the walk.
	ageWalk(t, b.walker, gatePhone, walkStaleAfter+time.Minute)

	// A later explicit request through the ALIAS form must expire the stale
	// registration and proceed, rather than 409 forever.
	a, _, err := b.RequestOlderHistory(ctx, gateLID, 50, true, 3)
	if err != nil {
		t.Fatalf("request after stale walk: err = %v, want nil", err)
	}
	if a.ChatJID != gatePhone || a.ID != "OLDEST" {
		t.Fatalf("anchor = %+v, want the oldest row under the phone JID", a)
	}
	if got := atomic.LoadInt32(sends); got != 2 {
		t.Fatalf("sends = %d, want 2 (one per accepted request)", got)
	}
	active, spent, _, stopped := b.walker.stats()
	if active != 1 {
		t.Fatalf("active = %d, want 1 (the new walk only)", active)
	}
	if stopped["unanswered"] != 1 {
		t.Fatalf("stopped[unanswered] = %d, want 1", stopped["unanswered"])
	}
	if spent != 2 {
		t.Fatalf("spent = %d, want 2", spent)
	}
	b.walker.mu.Lock()
	st := b.walker.chats[gatePhone]
	b.walker.mu.Unlock()
	if st == nil || st.rounds != 1 || time.Since(st.lastReqAt) > time.Minute || st.mode != "older" || st.maxRounds != 3 {
		t.Fatalf("new registration = %+v, want a fresh older-mode walk at round 1", st)
	}
}

func TestOlderRequestKeepsAlmostStaleWalk(t *testing.T) {
	b, sends := gateBridge(t)
	ctx := context.Background()

	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); err != nil {
		t.Fatalf("first request: %v", err)
	}
	ageWalk(t, b.walker, gatePhone, walkStaleAfter-time.Minute)

	if _, _, err := b.RequestOlderHistory(ctx, gateLID, 50, true, 3); !errors.Is(err, errWalkActive) {
		t.Fatalf("request one minute before staleness: err = %v, want errWalkActive", err)
	}
	if got := atomic.LoadInt32(sends); got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}
	if _, _, _, stopped := b.walker.stats(); stopped["unanswered"] != 0 {
		t.Fatalf("stopped[unanswered] = %d, want 0 (nothing expired early)", stopped["unanswered"])
	}
}

// Expiry is scoped to the contact being requested. A stale walk on an
// unrelated chat is left for the next chunk-arrival sweep, exactly as before,
// so one API call cannot reshuffle another chat's state.
func TestOlderRequestLeavesOtherChatsStaleWalksAlone(t *testing.T) {
	b, _ := gateBridge(t)
	ctx := context.Background()

	if !b.walker.begin("other@g.us", 9000, 100) {
		t.Fatal("begin other")
	}
	ageWalk(t, b.walker, "other@g.us", walkStaleAfter+time.Hour)

	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); err != nil {
		t.Fatalf("request: %v", err)
	}
	if !b.walker.isActive("other@g.us") {
		t.Fatal("an unrelated chat's stale walk was expired by this request")
	}
	if active, _, _, stopped := b.walker.stats(); active != 2 || stopped["unanswered"] != 0 {
		t.Fatalf("active=%d unanswered=%d, want 2 and 0", active, stopped["unanswered"])
	}
}

// The send failing must still release the claim, so the next request is not
// refused for a walk whose request never left the bridge.
func TestOlderRequestSendFailureReleasesClaim(t *testing.T) {
	b, _ := gateBridge(t)
	ctx := context.Background()

	fail := true
	prev := sendHistoryBefore
	sendHistoryBefore = func(_ *Bridge, _ context.Context, _, _ string, _ int64, _ bool, _ int) (whatsmeow.SendResponse, error) {
		if fail {
			return whatsmeow.SendResponse{}, errors.New("socket closed")
		}
		return whatsmeow.SendResponse{ID: "REQ"}, nil
	}
	t.Cleanup(func() { sendHistoryBefore = prev })

	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); err == nil {
		t.Fatal("want the send error to surface")
	}
	if active, _, _, stopped := b.walker.stats(); active != 0 || stopped["send_failed"] != 1 {
		t.Fatalf("active=%d send_failed=%d, want 0 and 1", active, stopped["send_failed"])
	}
	fail = false
	if _, _, err := b.RequestOlderHistory(ctx, gatePhone, 50, true, 3); err != nil {
		t.Fatalf("request after a failed send: %v", err)
	}
}

// The gate is one critical section: many concurrent requests for the same
// human (mixed JID forms) yield exactly one registration and one send. Run
// with -race.
func TestOlderRequestGateIsAtomicUnderConcurrency(t *testing.T) {
	b, sends := gateBridge(t)
	ctx := context.Background()

	const n = 24
	var wg sync.WaitGroup
	var ok, refused int32
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			jid := gatePhone
			if i%2 == 1 {
				jid = gateLID
			}
			<-start
			_, _, err := b.RequestOlderHistory(ctx, jid, 50, true, 3)
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, errWalkActive):
				atomic.AddInt32(&refused, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if ok != 1 || refused != n-1 {
		t.Fatalf("ok=%d refused=%d, want 1 and %d", ok, refused, n-1)
	}
	if got := atomic.LoadInt32(sends); got != 1 {
		t.Fatalf("sends = %d, want exactly 1", got)
	}
	if active, spent, _, _ := b.walker.stats(); active != 1 || spent != 1 {
		t.Fatalf("active=%d spent=%d, want 1 and 1", active, spent)
	}
}

// Walker-level view of the same gate, without a Bridge: stale keys expire,
// fresh keys refuse, the budget is raised only when the ask would not fit.
func TestClaimOlderWalkExpiresOnlyStaleKeys(t *testing.T) {
	w := newWalkerForTest(5, 10)
	w.beginMode("stale@lid", 5000, 100, "repair", 0)
	w.beginMode("fresh@lid", 5000, 100, "older", 3)
	w.mu.Lock()
	w.chats["stale@lid"].lastReqAt = time.Now().Add(-walkStaleAfter - time.Second)
	w.mu.Unlock()

	if err := w.claimOlderWalk([]string{"fresh@lid", "x@lid"}, "fresh@lid", 4000, "A", 100, 3); !errors.Is(err, errWalkActive) {
		t.Fatalf("claim over a fresh walk: err = %v, want errWalkActive", err)
	}
	if err := w.claimOlderWalk([]string{"stale-phone@s.whatsapp.net", "stale@lid"}, "stale-phone@s.whatsapp.net", 4000, "A", 100, 3); err != nil {
		t.Fatalf("claim over a stale walk: err = %v, want nil", err)
	}
	if w.isActive("stale@lid") {
		t.Fatal("stale registration survived the claim")
	}
	if !w.isActive("stale-phone@s.whatsapp.net") || !w.isActive("fresh@lid") {
		t.Fatal("expected the new claim and the untouched fresh walk to be registered")
	}
	active, spent, budget, stopped := w.stats()
	if active != 2 || spent != 3 || stopped["unanswered"] != 1 {
		t.Fatalf("active=%d spent=%d unanswered=%d, want 2, 3, 1", active, spent, stopped["unanswered"])
	}
	// budget 5, spent 2 before the claim, ask 3: 2+3 <= 5 fits, unchanged.
	if budget != 5 {
		t.Fatalf("budget = %d, want 5 (ask fit; no raise)", budget)
	}
	// Now the ask does not fit: spent 3, ask 3 -> raised to 6.
	if err := w.claimOlderWalk([]string{"y@lid"}, "y@lid", 4000, "B", 100, 3); err != nil {
		t.Fatalf("claim with raise: %v", err)
	}
	if _, spent, budget, _ := w.stats(); spent != 4 || budget != 6 {
		t.Fatalf("spent=%d budget=%d, want 4 and 6", spent, budget)
	}
}
