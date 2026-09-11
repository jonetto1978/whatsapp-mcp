"""Shared test guards for whatsapp-mcp-server.

Tool functions append to the user's real audit log (~/.claude/whatsapp-mcp/
audit.log) on every call. Behavioral tests call the tools directly with the
bridge mocked, so the audit sink is disabled for every test here; nothing in
this suite may touch a file outside the tempdir or the network.
"""

from __future__ import annotations

import pytest

import main


@pytest.fixture(autouse=True)
def _no_audit_log(monkeypatch):
    monkeypatch.setattr(main, "AUDIT_LOG_ENABLED", False)
