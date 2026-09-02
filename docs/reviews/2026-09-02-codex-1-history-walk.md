# Codex review 1 — commit cc71041 (history walk) — gpt-5.5 xhigh, 2026-09-02 12:53 ART

I reviewed commit cc71041 and the requested files in read-only mode; current checkout is HEAD 9c26321 with later fixes, so the finding line refs below are for cc71041 from `git show cc71041:<path>`. I found one critical defect, two high defects, four medium defects, and two low defects in cc71041; I did not edit files, restart the bridge, or call any POST endpoint that sends WhatsApp traffic. Left: add the missing tests named below and keep the follow-up fixes that already exist in current HEAD.

Verification done:
- `git show cc71041` read.
- Requested files read: `whatsapp-bridge/backfill_walk.go`, `whatsapp-bridge/history_sync.go`, `whatsapp-bridge/server.go`, `whatsapp-mcp-server/main.py`, `whatsapp-bridge/backfill_walk_test.go`.
- Alias evidence read: `whatsapp-bridge/aliases.go`.
- Repo grep done for `request-history`, `request_history`, `SweepDecodeBackfill`, hooks, scripts, docs.
- Current HEAD checks passed: `go test ./...`, `go vet ./...`, `go build -o /tmp/wa-review-bin .`, `go test -race -run TestWalkNeverExceedsBudgetUnderConcurrency ./...`, `uv run pytest -o cache_dir=/tmp/wa-review-pytest-cache` with 109 passed and 10 skipped, and `curl -sS -i http://127.0.0.1:8080/healthcheck` returned HTTP 200.
- The test results are for current HEAD 9c26321, not a checkout of cc71041.

Findings, ranked:

CRITICAL 1 - `whatsapp-mcp-server/main.py:556-589` and `whatsapp-bridge/server.go:110-140`: the MCP tool defaults `direction="older"` and sends it as `anchor:"older"`, but the bridge accepts only `anchor:"oldest"` or `anchor:"newest"`, so the default `request_history()` path fails before any history walk starts.
Scenario: caller runs `request_history(chat_jid="123@lid")`; Python builds `{"anchor":"older"}` at `main.py:588-589`; Go reaches `server.go:138-140` and returns 400 `anchor must be "oldest" or "newest"`; no WhatsApp history request is sent.
Fix: map tool vocabulary to bridge vocabulary, `older|oldest -> oldest`, `newest -> newest`, reject anything else before the POST, and add a Python wire test for the default call.
Status: verified from source; not executed because POST would send WhatsApp traffic.

HIGH 1 - `whatsapp-bridge/history_sync.go:404-414`, `whatsapp-bridge/backfill_walk.go:157-165`, `whatsapp-bridge/backfill_walk.go:226-231`, `whatsapp-bridge/server.go:143`, and `whatsapp-mcp-server/main.py:561-589`: `max_rounds` has no upper bound and also raises the global budget, so an API caller can bypass the request-storm brake.
Scenario: caller sends `max_rounds=5000,count=200`; `RequestOlderHistory` only defaults values <=0, then `extendBudget(5000)` raises `budget` to `spent+5000`, and `next()` uses `st.maxRounds` as the per-chat cap; one tool call can drive about 5000 peer history-sync sends.
Fix: clamp `max_rounds` in the Go backend to a hard ceiling, ideally `defaultMaxWalkRounds`, clamp in Python too for client hygiene, and return the applied cap in the HTTP response.
Status: verified from source; not executed because it would send WhatsApp traffic.

HIGH 2 - `whatsapp-bridge/backfill_walk.go:131-149`, `whatsapp-bridge/history_sync.go:411-418`, `whatsapp-bridge/backfill_walk.go:217-240`, and `whatsapp-bridge/history_sync.go:258-260`: concurrent `request_history` calls for the same chat overwrite the active `walkState` and can make duplicate chunks stop a valid walk.
Scenario: two HTTP calls for the same chat both anchor on the same oldest row; both register and send; the first returned chunk advances `anchorTS` and sends step 2; the duplicate first chunk then has `chunkOldestTS == anchorTS`, so `next()` stops with `no_progress` and deletes the walk; the step-2 response later arrives with no active key and is ignored.
Fix: reject a new walk with 409 when the same chat or any alias is already active, or make same-chat starts idempotent against the same anchor.
Status: source behavior verified; duplicate delivery ordering is a reasoned hypothesis.

MEDIUM 1 - `whatsapp-bridge/backfill_walk.go:63-81`, `whatsapp-bridge/backfill_walk.go:217-240`, and `whatsapp-bridge/history_sync.go:205-209`: `no_progress` compares only timestamps, so real progress inside the same WhatsApp second can be misread as no progress.
Scenario: local oldest anchor is message `C` at timestamp 1000; WhatsApp returns older messages `A` and `B` also at timestamp 1000, and maybe more older messages exist before `A`; `oldest.ID` changed but `oldest.TS == anchorTS`, so the walker stops and does not request before `A`.
Fix: store `anchorID` as well as `anchorTS` in `walkState`, pass `oldest.ID` into `next()`, stop only when the same anchor ID comes back or the chunk is newer, and continue when the anchor ID moves even if the second-level timestamp is equal.
Status: source limitation verified; same-second history chunks are a realistic but unexecuted hypothesis.

MEDIUM 2 - `whatsapp-bridge/history_sync.go:411-419`, `whatsapp-bridge/backfill_walk.go:247-255`, `whatsapp-bridge/history_sync.go:591-595`, and `whatsapp-bridge/backfill_walk.go:292-294`: API walks can stay registered after an initial send failure, no-response, or empty-anchor chunk, and `walkStaleAfter` is not honored unless a decode sweep later runs.
Scenario: `beginMode` registers before `RequestHistoryBefore`; if the send returns an error, or WhatsApp returns a conversation with no usable anchor, cc71041 leaves the chat in `w.chats`; because `sweepStale()` is only called by `SweepDecodeBackfill`, a later unsolicited matching chunk after 15 minutes can still resume the stale walk.
Fix: release the walk on the first send error, call `next(key,0,false)` for anchorless chunks after resolving the key, and run stale cleanup from API begin/status/continue or a timer.
Status: source behavior verified; no-response and late-chunk effects are hypotheses.

MEDIUM 3 - `whatsapp-bridge/backfill_walk.go:275-285`, `whatsapp-bridge/aliases.go:49-53`, `whatsapp-bridge/backfill_walk.go:300-319`, and `whatsapp-bridge/history_sync.go:609-613`: the repair sweep's `hasEmptyRowsOlderThan` check ignores JID aliases, so split LID/phone histories can stop with `nothing_older` while older empty rows still exist under the alias.
Scenario: a human has recent LID rows and older phone-JID rows; the repair walk is active for the LID and a chunk arrives under the LID; `hasEmptyRowsOlderThan` checks only `chat_jid = LID`, returns false, and `next()` stops even though `phone@s.whatsapp.net` still has older empty rows.
Fix: resolve aliases inside `hasEmptyRowsOlderThan` and query `chat_jid IN (...)`, matching `OldestAnchor` and `list_messages` behavior.
Status: verified from source; the split-history shape is assumed by the commit comments and alias code.

MEDIUM 4 - `whatsapp-bridge/backfill_walk.go:141-149`, `whatsapp-bridge/backfill_walk.go:172-180`, `whatsapp-bridge/backfill_walk.go:300-308`, and `whatsapp-bridge/history_sync.go:609-614`: two active walks for aliases of the same conversation can be matched to the wrong state because `resolveKey()` returns the first active candidate, not a request-correlated or canonical conversation key.
Scenario: a decode repair sweep starts under the LID key while an API older-history walk starts under the phone key; a chunk delivered under the LID builds candidates `[LID, phone]`, so it advances the repair walk even if the chunk was the API response, applying the wrong mode, `perChat`, and `maxRounds`.
Fix: canonicalize walk keys to an alias component and reject a second active walk across the whole alias set; if request correlation is available from whatsmeow, include it in the state match.
Status: source behavior verified; exact WhatsApp response routing is a hypothesis.

LOW 1 - `whatsapp-bridge/server.go:143-156` and `whatsapp-bridge/history_sync.go:404-406`: the HTTP response reports the requested `max_rounds`, not the applied default.
Scenario: caller omits `max_rounds`; Go applies default 10, but the response says `"max_rounds":0`, so an operator or MCP caller sees the wrong cap.
Fix: include applied `MaxRounds` in the returned anchor/result and write that value in the response.
Status: verified from source.

LOW 2 - `whatsapp-bridge/backfill_walk.go:107-120` and `whatsapp-bridge/history_sync.go:642-650`: `reset(0,0,...)` keeps the current walker budget and maxRounds, so a direct zero-value `SweepDecodeBackfill` call after an API budget extension can inherit an inflated budget.
Scenario: code calls `request_history(max_rounds=5000)`, then later calls `SweepDecodeBackfill(ctx, DecodeBackfillOptions{})`; `applyDefaults()` leaves `WalkBudget` and `MaxRounds` at zero, and `reset()` keeps the inflated values instead of restoring defaults.
Fix: make zero reset arguments restore `defaultWalkBudget` and `defaultMaxWalkRounds`; the HTTP backfill-decode handler already passes positive defaults, so this mainly protects direct Go callers and tests.
Status: verified from source.

Question answers:

(a) Concurrency: for one active walk, I found no Go data race in the core state updates; `beginMode`, `resolveKey`, `next`, `sweepStale`, and `stats` all take `w.mu`, `next()` updates `spent`, `rounds`, and `anchorTS` before the goroutine send, and the targeted race test passed on current HEAD. The race-risk defects are not raw memory races; they are logical races from duplicate starts, alias-overlapping starts, and registered-before-send failures. `beginMode` before network send is right for normal responses, but it needs rollback on send failure and active-walk rejection.

(b) Alias resolution: if `jid_aliases` has the direct LID<->phone edge, a chunk under phone can find a walk under LID and vice versa because aliases are stored both ways in `aliases.go:49-53` and `continueWalk` passes all aliases to `resolveKey`. I did not find a normal wrong-chat match with a correct alias table, but I did find the overlapping-alias-walk wrong-state case above. The anchor row's own `chat_jid` is the right conversation to send to WhatsApp, because `OldestAnchor` selects the row's `chat_jid` at `history_sync.go:369-372` and `RequestHistoryBefore` builds `MessageInfo.Chat` from that value at `history_sync.go:453-467`; using the caller's other alias would pair an anchor ID with the wrong chat key.

(c) Budget and maxRounds: cc71041 lets callers bypass the global budget with a large `max_rounds`, and repeated API calls can grow the shared budget by design. `walkStaleAfter` is not fully honored for API walks because stale cleanup is not on the API path. Two concurrent calls on the same chat can overwrite state and waste or kill progress; two concurrent calls on two true different chats are serialized by the mutex and bounded by the current shared budget, except that the caller can inflate that budget.

(d) `no_progress`: the brake terminates, and it is correct when WhatsApp returns only the anchor itself because `chunkOldestTS == anchorTS` stops the walk. It is not correct in all cases because timestamp-only progress cannot distinguish same-second older messages from the same anchor. The live observation, last chunk had one message and stopped `no_progress`, is consistent with the correct anchor-return stop case, but it does not prove the same-second case safe.

(e) API default flip and repair sweep: grep found no repo caller of `POST /api/admin/request-history` except `whatsapp-mcp-server/main.py`; hooks and scripts had no callers, and docs only mention the endpoint. The default flip does not directly change `SweepDecodeBackfill`, because repair uses `POST /api/admin/backfill-decode`, `SweepDecodeBackfill`, and `RequestChatHistory` at `history_sync.go:613`. Repair still has the alias-empty and stale-registration risks listed above.

(f) Test sufficiency: the new tests in `backfill_walk_test.go:278-352` cover older mode skipping the empties brake, no-progress by timestamp, per-request maxRounds below the walker default, alias lookup, and budget extension. They are not sufficient for the defects above. Missing tests: Python default `request_history()` sends `anchor:"oldest"`; invalid direction is rejected; Go clamps huge `max_rounds` and returns the applied cap; same-chat and alias-same-chat concurrent starts return 409 or behave idempotently; send failure releases state; anchorless chunks stop and release state; stale API walks are cleaned without a decode sweep; `OldestAnchor` picks the oldest row across aliases and sends to that row's `chat_jid`; repair-mode `hasEmptyRowsOlderThan` resolves aliases; same-timestamp different-ID chunks continue while exact anchor return stops; and `SweepDecodeBackfill(DecodeBackfillOptions{})` resets budget/maxRounds after an API budget extension.