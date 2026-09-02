# Review 1 — commit cc71041 (history walk) — Claude/Opus agent, 2026-09-02 12:04 ART
Checks: go vet clean · go build clean · go test ok (15.7s) · healthcheck ok 0.4.0. Bridge untouched.
Core design sound: register-before-send, brakes in one place, `oldest` set outside RowsAffected guard.

HIGH — max_rounds has no upper clamp and raises the global budget (history_sync.go:404-406; server.go:143; main.py:561; backfill_walk.go:157-166). request_history(max_rounds=5000) → up to 5000 peer HistorySyncOnDemandRequest sends. Fix: clamp to defaultMaxWalkRounds in RequestOlderHistory and in main.py.
HIGH — inflated budget survives into the next repair sweep: reset() assigns budget only when >0; applyDefaults leaves WalkBudget=0. Fix: reset falls back to defaultWalkBudget when budget<=0, or track extension separately.
MEDIUM — a repair sweep's reset() wipes w.chats, silently killing an in-flight API walk (not_walking log suppressed). Fix: skip mode=="older" in reset, or log.
MEDIUM — failed send leaves the walk registered forever; sweepStale only called from SweepDecodeBackfill. A later unsolicited chunk resumes stepping (HYPOTHESIS). Fix: release on send error; sweepStale on a timer.
MEDIUM — zero-timestamp chunk: oldest never set (ts>0 guard) → continueWalk returns early → walk neither steps nor stops; the "zero" subtest asserts an unreachable state. Fix: call next() with TS 0 so it stops/releases.
MEDIUM — hasEmptyRowsOlderThan filters chat_jid = ? without aliases (repair mode stops early on alias-split chats). Pre-existing. Fix: IN(aliases).
LOW/MED — resolveKey alias borrowing: chat A (no walk) can consume alias B's round; follow-up sent under A. HYPOTHESIS. Fix: only borrow when mode=="older", or canonical key.
LOW — concurrent request_history on one chat silently overwrites walkState (bounded, no storm). Fix: refuse/409 when active.
CLEAN — concurrency (whatsmeow dispatches HistorySync from one goroutine; no nested locks; no DB under lock); aliases bidirectional (aliases.go:49-53,161-166,67); anchor row's chat_jid is the right conversation; termination provable (strictly decreasing anchorTS + rounds + spent); no other callers of the endpoint; SweepDecodeBackfill byte-identical.
TESTS MISSING — clamp; reset after extend; reset during older walk; send-failure release; continueWalk over alias end-to-end; zero-ts chunk; concurrent beginMode; handler defaults incl. "Oldest" case; alias borrowing; hasEmptyRowsOlderThan across aliases; beginMode("") defaults to repair.
