package main

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// The walk is self-propelling: each delivered history chunk decides whether to
// issue the next request. That shape can run away, so what is tested here is
// the STOPPING, not the stepping. Every brake gets its own case, plus a
// property test asserting no input sequence can exceed the budget.

func newWalkerForTest(budget, maxRounds int) *backfillWalker {
	w := newBackfillWalker()
	w.reset(budget, maxRounds, 100)
	return w
}

// TestWalkStepsBackwards is the happy path: a chunk older than the anchor, with
// empties still further back, continues and re-anchors.
func TestWalkStepsBackwards(t *testing.T) {
	w := newWalkerForTest(100, 10)
	if !w.begin("c@g.us", 5000, 100) {
		t.Fatal("begin returned false with budget available")
	}

	d := w.next("c@g.us", 4000, true)
	if !d.Continue {
		t.Fatalf("walk stopped (%s), want continue", d.Reason)
	}
	if d.PerChat != 100 {
		t.Errorf("PerChat = %d, want 100", d.PerChat)
	}

	// The anchor must have advanced, or the next round would re-request the
	// same window — the exact bug the walk exists to fix.
	w.mu.Lock()
	got := w.chats["c@g.us"].anchorTS
	w.mu.Unlock()
	if got != 4000 {
		t.Errorf("anchorTS = %d, want 4000 (anchor did not advance)", got)
	}
}

// TestWalkStopsOnNoProgress is the brake that catches WhatsApp replaying the
// same window. Without it the walk requests forever and never advances.
func TestWalkStopsOnNoProgress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		chunkOldest int64
	}{
		{"same as anchor", 5000},
		{"newer than anchor", 5500},
		{"zero (no timestamps in chunk)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWalkerForTest(100, 10)
			w.begin("c@g.us", 5000, 100)

			d := w.next("c@g.us", tc.chunkOldest, true)
			if d.Continue {
				t.Fatal("walk continued without advancing — it would replay the same window forever")
			}
			if d.Reason != "no_progress" {
				t.Errorf("Reason = %q, want \"no_progress\"", d.Reason)
			}
			// State must be released, not left pinned.
			w.mu.Lock()
			_, still := w.chats["c@g.us"]
			w.mu.Unlock()
			if still {
				t.Error("chat left registered after stopping")
			}
		})
	}
}

func TestWalkStopsWhenNothingOlderRemains(t *testing.T) {
	w := newWalkerForTest(100, 10)
	w.begin("c@g.us", 5000, 100)

	d := w.next("c@g.us", 4000, false) // progressed, but nothing left to repair
	if d.Continue {
		t.Fatal("walk continued with no empty rows older — it would spend requests for no repair")
	}
	if d.Reason != "nothing_older" {
		t.Errorf("Reason = %q, want \"nothing_older\"", d.Reason)
	}
}

func TestWalkStopsAtMaxRounds(t *testing.T) {
	const maxRounds = 5
	w := newWalkerForTest(1000, maxRounds)
	w.begin("c@g.us", 100000, 100)

	ts := int64(100000)
	rounds := 1 // begin() counts as round 1
	for i := 0; i < maxRounds+5; i++ {
		ts -= 1000
		d := w.next("c@g.us", ts, true)
		if !d.Continue {
			if d.Reason != "max_rounds" {
				t.Fatalf("stopped for %q, want \"max_rounds\"", d.Reason)
			}
			break
		}
		rounds++
	}
	if rounds != maxRounds {
		t.Errorf("ran %d rounds, want the cap of %d", rounds, maxRounds)
	}
}

// TestWalkRespectsGlobalBudget covers the brake that matters most under
// concurrency: many chats each within their per-chat cap can still multiply
// into a request storm, so the budget is enforced across all of them.
func TestWalkRespectsGlobalBudget(t *testing.T) {
	const budget = 10
	w := newWalkerForTest(budget, 1000)

	started := 0
	for i := 0; i < 50; i++ {
		if w.begin(chatID(i), 100000, 100) {
			started++
		}
	}
	if started != budget {
		t.Errorf("begin succeeded %d times, want exactly the budget %d", started, budget)
	}

	// Every further step must also refuse.
	d := w.next(chatID(0), 90000, true)
	if d.Continue {
		t.Error("walk continued past the global budget")
	}

	_, spent, gotBudget, _ := w.stats()
	if spent > gotBudget {
		t.Errorf("spent %d exceeds budget %d", spent, gotBudget)
	}
}

// TestWalkNeverExceedsBudgetUnderConcurrency is the property that has to hold
// no matter how deliveries interleave: total requests issued can never exceed
// the budget. Chunks arrive from whatsmeow's event goroutines, so `next` is
// genuinely called concurrently.
func TestWalkNeverExceedsBudgetUnderConcurrency(t *testing.T) {
	const budget = 25
	w := newWalkerForTest(budget, 1000)

	const chats = 20
	for i := 0; i < chats; i++ {
		w.begin(chatID(i), 1_000_000, 100)
	}

	var wg sync.WaitGroup
	for i := 0; i < chats; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ts := int64(1_000_000)
			for r := 0; r < 50; r++ {
				ts -= 100
				if !w.next(chatID(idx), ts, true).Continue {
					return
				}
			}
		}(i)
	}
	wg.Wait()

	_, spent, gotBudget, _ := w.stats()
	if spent > gotBudget {
		t.Fatalf("spent %d requests, budget was %d — the walk can run away under concurrency", spent, gotBudget)
	}
}

// TestWalkIgnoresUnsolicitedChunks: WhatsApp also delivers history we did not
// ask for (the initial pairing sync). Those must not start a walk.
func TestWalkIgnoresUnsolicitedChunks(t *testing.T) {
	w := newWalkerForTest(100, 10)
	d := w.next("never-registered@g.us", 4000, true)
	if d.Continue {
		t.Error("an unsolicited chunk started a walk")
	}
	if d.Reason != "not_walking" {
		t.Errorf("Reason = %q, want \"not_walking\"", d.Reason)
	}
	if _, spent, _, _ := w.stats(); spent != 0 {
		t.Errorf("spent %d requests on an unsolicited chunk, want 0", spent)
	}
}

// TestWalkReleasesUnansweredChats: if WhatsApp never answers, the chat must not
// stay pinned as "walking" and block a later sweep from retrying it.
func TestWalkReleasesUnansweredChats(t *testing.T) {
	w := newWalkerForTest(100, 10)
	w.begin("c@g.us", 5000, 100)

	w.mu.Lock()
	w.chats["c@g.us"].lastReqAt = time.Now().Add(-walkStaleAfter - time.Minute)
	w.mu.Unlock()

	w.sweepStale()

	if active, _, _, stopped := w.stats(); active != 0 || stopped["unanswered"] != 1 {
		t.Errorf("active=%d unanswered=%d, want 0 and 1", active, stopped["unanswered"])
	}
}

// TestWalkResetRearmsBudget: a new sweep must start with a fresh budget, or the
// second sweep of a session would be silently starved.
func TestWalkResetRearmsBudget(t *testing.T) {
	w := newWalkerForTest(5, 10)
	for i := 0; i < 10; i++ {
		w.begin(chatID(i), 1000, 100)
	}
	if _, spent, _, _ := w.stats(); spent != 5 {
		t.Fatalf("spent = %d, want 5", spent)
	}

	w.reset(5, 10, 100)
	if active, spent, _, _ := w.stats(); active != 0 || spent != 0 {
		t.Errorf("after reset active=%d spent=%d, want 0 and 0", active, spent)
	}
	if !w.begin("fresh@g.us", 1000, 100) {
		t.Error("begin refused after reset — a second sweep would be starved")
	}
}

// TestHasEmptyRowsOlderThan backs brake 3. If this answered wrongly the walk
// would either stop early (leaving rows unrecovered) or page on forever.
func TestHasEmptyRowsOlderThan(t *testing.T) {
	db, err := sql.Open("sqlite3", t.TempDir()+"/w.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chats (jid, chat_type, created_at, updated_at) VALUES ('c@g.us','group',0,0)`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	ins := func(id string, ts int64, typ, content string) {
		if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, timestamp, type, content_text) VALUES (?,?,?,?,?)`,
			id, "c@g.us", ts, typ, content); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	ins("old-empty", 1000, "system", "")
	ins("new-text", 9000, "text", "hello")
	ins("old-marker", 900, "system", "[unsupported: pollUpdateMessage]")

	b := &Bridge{db: db}
	ctx := context.Background()

	if got, _ := b.hasEmptyRowsOlderThan(ctx, "c@g.us", 5000); !got {
		t.Error("want true: an empty row exists older than 5000")
	}
	if got, _ := b.hasEmptyRowsOlderThan(ctx, "c@g.us", 500); got {
		t.Error("want false: nothing empty older than 500")
	}
	// A marker row is already handled and must not keep the walk paging.
	if got, _ := b.hasEmptyRowsOlderThan(ctx, "c@g.us", 950); got {
		t.Error("want false: a marker row is not an empty row")
	}
}

func chatID(i int) string {
	return string(rune('a'+i%26)) + "-chat@g.us"
}

// TestOlderModeIgnoresEmptiesBrake: a plain history fetch has nothing to
// repair, so "no empty rows further back" must not stop it — that brake is
// exactly why the MCP request_history tool could never go past one window.
func TestOlderModeIgnoresEmptiesBrake(t *testing.T) {
	w := newWalkerForTest(100, 10)
	if !w.beginMode("c@lid", 5000, 100, "older", 0) {
		t.Fatal("beginMode returned false")
	}
	d := w.next("c@lid", 4000, false)
	if !d.Continue {
		t.Fatalf("older-mode walk stopped (%s) on hasOlderEmpties=false; want continue", d.Reason)
	}
	// Repair mode must still stop on the same input.
	w2 := newWalkerForTest(100, 10)
	w2.begin("r@lid", 5000, 100)
	if d2 := w2.next("r@lid", 4000, false); d2.Continue || d2.Reason != "nothing_older" {
		t.Fatalf("repair-mode: got continue=%v reason=%q, want stop nothing_older", d2.Continue, d2.Reason)
	}
}

// TestOlderModeStillStopsOnNoProgress: the replay brake must apply to older
// mode too, or a server that keeps returning the same window loops forever.
func TestOlderModeStillStopsOnNoProgress(t *testing.T) {
	w := newWalkerForTest(100, 10)
	w.beginMode("c@lid", 5000, 100, "older", 0)
	if d := w.next("c@lid", 5000, false); d.Continue || d.Reason != "no_progress" {
		t.Fatalf("got continue=%v reason=%q, want stop no_progress", d.Continue, d.Reason)
	}
}

// TestPerRequestMaxRoundsCapsBelowWalkerDefault: an API caller's max_rounds
// bounds its own walk even when the walker-wide ceiling is higher.
func TestPerRequestMaxRoundsCapsBelowWalkerDefault(t *testing.T) {
	w := newWalkerForTest(100, 20)
	w.beginMode("c@lid", 9000, 100, "older", 3)         // round 1
	if d := w.next("c@lid", 8000, false); !d.Continue { // round 2
		t.Fatalf("round 2 stopped early: %s", d.Reason)
	}
	if d := w.next("c@lid", 7000, false); !d.Continue { // round 3
		t.Fatalf("round 3 stopped early: %s", d.Reason)
	}
	if d := w.next("c@lid", 6000, false); d.Continue || d.Reason != "max_rounds" {
		t.Fatalf("round 4: got continue=%v reason=%q, want stop max_rounds", d.Continue, d.Reason)
	}
}

// TestResolveKeyFindsWalkUnderAlias: chunks arrive under whichever JID form
// WhatsApp picked; the walk registered under the other form must be found.
func TestResolveKeyFindsWalkUnderAlias(t *testing.T) {
	w := newWalkerForTest(100, 10)
	w.beginMode("5491100000000@s.whatsapp.net", 5000, 100, "older", 0)
	if got := w.resolveKey([]string{"123@lid", "5491100000000@s.whatsapp.net"}); got != "5491100000000@s.whatsapp.net" {
		t.Fatalf("resolveKey = %q, want the registered alias", got)
	}
	if got := w.resolveKey([]string{"nobody@lid"}); got != "" {
		t.Fatalf("resolveKey = %q for an unregistered chat, want empty", got)
	}
}

// TestExtendBudgetOnlyRaises: extending never lowers, and only raises when
// the ask would not fit.
func TestExtendBudgetOnlyRaises(t *testing.T) {
	w := newWalkerForTest(5, 10)
	w.extendBudget(3) // spent 0, budget 5: fits, unchanged
	if w.budget != 5 {
		t.Fatalf("budget = %d, want 5 (unchanged)", w.budget)
	}
	for i := 0; i < 5; i++ {
		w.begin("c"+string(rune('a'+i))+"@lid", 1000, 10)
	}
	w.extendBudget(2) // spent 5 == budget 5: raise to 7
	if w.budget != 7 {
		t.Fatalf("budget = %d, want 7", w.budget)
	}
}
