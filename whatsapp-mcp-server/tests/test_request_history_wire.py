"""request_history: wire mapping, caps and hint handling, driven behaviorally.

The bridge speaks anchor="oldest"|"newest"; the tool speaks direction=
"older"|"newest" (a bare pass-through once sent anchor="older", rejected with
400 — review finding 2026-09-02). These tests call the real tool with the
bridge POST mocked and assert on the body it would have sent and the result
it returns. The earlier source-string inspection tests were replaced.
"""

from __future__ import annotations

import pytest

import main

CHAT = "5491100000000@s.whatsapp.net"


def _capture(monkeypatch, response):
    """Mock the bridge POST; return the list that collects each call."""
    calls: list[tuple[str, dict, float | None]] = []

    async def fake_post(path, body, timeout=None):
        calls.append((path, dict(body), timeout))
        return response

    monkeypatch.setattr(main, "_bridge_post", fake_post)
    return calls


# --- wire body ---------------------------------------------------------------


@pytest.mark.asyncio
@pytest.mark.parametrize("direction,anchor", [("older", "oldest"), ("oldest", "oldest"), ("newest", "newest")])
async def test_direction_maps_to_bridge_anchor(monkeypatch, direction, anchor):
    calls = _capture(monkeypatch, {"hint": "x"})
    await main.request_history(CHAT, direction=direction)
    (path, body, _), = calls
    assert path == "/api/admin/request-history"
    assert body["anchor"] == anchor
    assert "direction" not in body, "the raw direction must never reach the wire"
    assert body["chat_jid"] == CHAT


@pytest.mark.asyncio
async def test_invalid_direction_is_rejected_before_any_bridge_call(monkeypatch):
    calls = _capture(monkeypatch, {"hint": "x"})
    with pytest.raises(ValueError, match='direction must be "older" or "newest"'):
        await main.request_history(CHAT, direction="older-please")
    assert calls == []


@pytest.mark.asyncio
@pytest.mark.parametrize("count,sent", [(0, 1), (-5, 1), (50, 50), (200, 200), (999, 200)])
async def test_count_is_clamped_to_bridge_window(monkeypatch, count, sent):
    calls = _capture(monkeypatch, {})
    await main.request_history(CHAT, count=count)
    assert calls[0][1]["count"] == sent


@pytest.mark.asyncio
@pytest.mark.parametrize("rounds,sent", [(0, 1), (3, 3), (20, 20), (50, 20)])
async def test_max_rounds_is_clamped(monkeypatch, rounds, sent):
    calls = _capture(monkeypatch, {})
    await main.request_history(CHAT, max_rounds=rounds)
    assert calls[0][1]["max_rounds"] == sent


@pytest.mark.asyncio
async def test_walk_flag_is_forwarded(monkeypatch):
    calls = _capture(monkeypatch, {})
    await main.request_history(CHAT, walk=False)
    assert calls[0][1]["walk"] is False


# --- hint handling -----------------------------------------------------------

BRIDGE_HINT = "Anchored on the OLDEST local message; WhatsApp delivers the window before it asynchronously."


@pytest.mark.asyncio
async def test_bridge_hint_is_preserved_and_guidance_appended(monkeypatch):
    _capture(monkeypatch, {"anchor": "oldest", "sent_message_id": "3EB0", "hint": BRIDGE_HINT})
    result = await main.request_history(CHAT)
    assert result["hint"].startswith(BRIDGE_HINT), "the bridge's mode-specific hint must stay first and intact"
    assert result["hint"].endswith(main._HISTORY_GUIDANCE)
    assert result["sent_message_id"] == "3EB0", "other receipt fields pass through untouched"


@pytest.mark.asyncio
async def test_newest_mode_hint_is_kept_verbatim(monkeypatch):
    newest = "Newest-anchor request: re-fetches the window already held (media-key recovery)."
    _capture(monkeypatch, {"anchor": "newest", "hint": newest})
    result = await main.request_history(CHAT, direction="newest")
    assert newest in result["hint"]


@pytest.mark.asyncio
@pytest.mark.parametrize("hint", [None, "", "   ", 42], ids=["null", "empty", "blank", "non-string"])
async def test_missing_or_unusable_hint_gets_guidance_alone(monkeypatch, hint):
    _capture(monkeypatch, {"anchor": "oldest", "hint": hint})
    result = await main.request_history(CHAT)
    assert result["hint"] == main._HISTORY_GUIDANCE


@pytest.mark.asyncio
async def test_absent_hint_key_gets_guidance(monkeypatch):
    _capture(monkeypatch, {"anchor": "oldest"})
    result = await main.request_history(CHAT)
    assert result["hint"] == main._HISTORY_GUIDANCE


def test_guidance_states_sent_not_received_and_no_duplicates():
    g = main._HISTORY_GUIDANCE
    assert "SENT" in g and "not that rows arrived" in g
    assert "duplicate" in g
    for field in ("sent_message_id", "sent_at_unix", "anchor_message", "anchor_ts", "anchor_chat_jid", "max_rounds"):
        assert field in g
    assert "request_id" not in g, "the bridge returns no request_id; do not invent one"
    assert "15 minute" not in g and "15-minute" not in g, "elapsed time is not evidence a walk has been released"


@pytest.mark.asyncio
async def test_bridge_error_propagates_unchanged(monkeypatch):
    async def failing_post(path, body, timeout=None):
        raise RuntimeError("bridge 409: history request failed — a history walk is already active for this chat; wait for it to stop")

    monkeypatch.setattr(main, "_bridge_post", failing_post)
    with pytest.raises(RuntimeError, match="already active"):
        await main.request_history(CHAT)
