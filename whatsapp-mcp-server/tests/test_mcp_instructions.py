"""The MCP initialize handshake must deliver workflow instructions.

A fresh real handshake returned instructions=null before 2026-09-11, so
that handshake did not carry the voice/history workflow. This drives
the real FastMCP server through the supported in-memory client. The lifespan
builds an httpx client and probes /healthcheck; both the token lookup and the
HTTP client are replaced so no file outside the repo is read and no socket is
opened.
"""

from __future__ import annotations

import httpx
import pytest
from fastmcp import Client

import main

REQUIRED_PHRASES = (
    "voice_note_transcript",
    "`content_text` is empty for audio",
    "never the transcript",
    "does not prove the audio file is retained",
    "without transcribing",
    "three separate counts",
    "keep that scope",
    "@lid",
    "merged_jids",
    "oldest stored message across aliases",
    "SENT",
    "do not send a duplicate request",
    "409",
    "no anchor",
    "screen control",
    "untrusted",
    "confirm_send",
    "Check the saved Drive index first",
    "read_chat_archive",
    "Older history requires an explicit user request",
    "Never download all chats",
)


@pytest.fixture
def offline_lifespan(monkeypatch):
    """Keep the server's startup fully offline and file-free."""
    constructed: list[dict] = []

    class FakeAsyncClient:
        def __init__(self, **kwargs):
            constructed.append(kwargs)

        async def get(self, path, **_):
            raise httpx.ConnectError(f"test double: no bridge for {path}")

        async def aclose(self):
            return None

    monkeypatch.setattr(main.httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(main, "_bridge_token", lambda: None)
    monkeypatch.setattr(main, "_bridge_token_path", lambda: "<unused in tests>")
    return constructed


@pytest.mark.asyncio
async def test_handshake_delivers_instructions(offline_lifespan):
    async with Client(main.mcp) as client:
        init = client.initialize_result
        tools = await client.list_tools()

    assert init is not None
    assert init.instructions, "initialize must not return instructions=null"
    assert init.instructions == main.INSTRUCTIONS
    for phrase in REQUIRED_PHRASES:
        assert phrase in init.instructions, f"instructions must cover: {phrase!r}"
    assert {t.name for t in tools} >= {"list_messages", "download_media", "request_history", "list_native_messages", "recover_voice_note"}
    assert "list_native_messages" in init.instructions and "recover_voice_note" in init.instructions
    # Every HTTP client the lifespan built was the offline double.
    assert all(kw.get("base_url") == main.BRIDGE_BASE for kw in offline_lifespan)


def test_instructions_are_portable():
    text = main.INSTRUCTIONS
    assert "/Users/" not in text and "C:\\" not in text and "~/" not in text, "no host paths"
    assert "@s.whatsapp.net" in text and "+54" not in text, "no real numbers"
    assert "request_id" not in text
    assert "15 minute" not in text and "15-minute" not in text, "elapsed time is not evidence a walk was released"
    assert len(text) < 4000, "keep the handshake payload small"
