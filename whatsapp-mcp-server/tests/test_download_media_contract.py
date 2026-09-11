"""download_media: the error contract its docstring now promises.

The bridge answers 404 for three different reasons ("message not found",
"media key not available", "is not downloadable"); the tool must surface the
bridge's own text so a caller can tell a history gap from a key gap, and must
raise rather than return. Bridge POST is mocked; no network.
"""

from __future__ import annotations

import httpx
import pytest

import main


def _bridge_404(text: str) -> RuntimeError:
    req = httpx.Request("POST", f"{main.BRIDGE_BASE}/api/media/download")
    resp = httpx.Response(404, json={"error": text}, request=req)
    return main._bridge_error(httpx.HTTPStatusError("404", request=req, response=resp))


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "bridge_text",
    [
        "message not found: ABC123",
        "media key not available for message ABC123 (likely received before media-key persistence patch)",
        "message type text is not downloadable",
    ],
    ids=["history-gap", "key-gap", "not-media"],
)
async def test_404_causes_are_raised_with_bridge_text(monkeypatch, bridge_text):
    async def failing_post(path, body, timeout=None):
        raise _bridge_404(bridge_text)

    monkeypatch.setattr(main, "_bridge_post", failing_post)
    with pytest.raises(RuntimeError) as ei:
        await main.download_media("ABC123")
    assert str(ei.value).startswith("bridge 404: ")
    assert bridge_text in str(ei.value)


@pytest.mark.asyncio
async def test_success_passes_bridge_fields_through_with_long_timeout(monkeypatch):
    calls = []

    async def fake_post(path, body, timeout=None):
        calls.append((path, body, timeout))
        return {"message_id": "V1", "path": "/media/V1.ogg", "mime": "audio/ogg", "size": 4321, "cached_hit": True}

    monkeypatch.setattr(main, "_bridge_post", fake_post)
    result = await main.download_media("V1")
    assert result["path"] == "/media/V1.ogg" and result["cached_hit"] is True
    (path, body, timeout), = calls
    assert path == "/api/media/download" and body == {"message_id": "V1"}
    assert timeout == 90.0, "must outlast the bridge's own 60 s media fetch"
    assert "transcript" not in result, "downloading does not add a transcript"


def test_docstrings_name_the_transcript_field_correctly():
    for fn in (main.download_media, main.list_messages):
        doc = fn.__doc__ or ""
        assert "voice_note_transcript" in doc
        assert "never the transcript" in doc, f"{fn.__name__} must say content_text is not the transcript"
