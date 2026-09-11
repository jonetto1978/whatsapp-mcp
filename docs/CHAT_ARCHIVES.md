# Consult saved chats on demand

This is a content archive in Google Drive. It holds normal text, JSON and audio.
Do not add encryption when the user chose ordinary Drive files. Do not include
device sessions, credentials, raw media keys or unrelated chats.

## Scope comes first

Consult only the requested chat/group and date window. Typical uses are one
class, selected students, or a teacher discussion. Older history needs an
explicit user request, even when more context might help. A missing saved slice
is not permission to download all chats or start a history walk. If a class/event
has no known date, resolve its date before a broad read. No scheduled download
or vector database is part of this flow.

## Read a saved slice

1. Verify the selected account with the Drive MCP `get_profile` tool.
2. Use `list_folder` to find `Artifacts/WhatsApp/index.json`, or the user's chosen
   root. Reuse observed IDs. The small index lists only previously selected chats.
3. Resolve the chat by its stable ID and phone/LID aliases. Its index entry has
   a `manifest_id`; the manifest is read inside the tool, not pasted into context.
4. Call `read_chat_archive` with the manifest ID, inclusive `start_date` and
   `end_date` in `YYYY-MM-DD`, a small `limit`, and `max_characters`.
5. The tool reads relevant monthly files and filters before returning records.
   Default limits are 40 records and 12,000 text characters; callers can lower
   them. Maximum limits are 100 records and 30,000 text characters. Voice text
   requires `include_transcripts=true`. Optional `query` is literal text search,
   without case sensitivity. It is not semantic search.
6. Use `next_cursor` only with the same dates, query, transcript option and
   archive version. Stop once the requested question is answered. Report record
   and character counts, truncation and saved coverage. An empty slice does not
   prove no messages existed. This tool makes zero WhatsApp requests.

```json
{
  "manifest_id": "<observed-manifest-id>",
  "start_date": "2026-09-10",
  "end_date": "2026-09-10",
  "limit": 10,
  "max_characters": 4000,
  "include_transcripts": true
}
```

## Extend an archive without replacing earlier records

Retrieve a missing/newer/older slice only when the user requests it. Use the
WhatsApp MCP for that selected slice and preserve pending history receipts.
Save through the Drive MCP. The archive does not itself schedule later fetches.

Keep one folder per chat, monthly JSONL message files, individual UTF-8
transcripts and audio files named by original message ID. Retain phone/LID
aliases. The `whatsapp-chat-archive/v1` manifest records the chat identity,
timezone, counts, coverage, monthly file IDs and SHA-256 values, and voice-file
links. Each month's records include `id`, Unix `timestamp`, ISO `date`,
`sender_display`, `is_from_me`, `type`, `content_text`, and optional
`voice_note_transcript` and Drive file links. A small `whatsapp-chat-index/v1`
root index maps chats to manifests. A combined conversation is optional, for
an explicitly requested full reading.

Read an existing month before writing it. Merge by message ID and retain all
records outside the current query. A partial result is not a deletion signal.
Keep source wording and distinguish the sender from the actual voice speaker.
Use `update_file` with the last-read `expected_md5`; re-read and merge if it is
stale. That checksum check is not an atomic lock against simultaneous writers,
so avoid concurrent writers for the same chat. Reuse unchanged files and IDs.

Verify provider checksums and content readback after upload. Update the monthly
hashes in the manifest and its entry in the root index. The archive reader
rejects changed monthly bytes until the manifest is refreshed. Record gaps;
earliest/latest dates are bounds, not proof of complete server history. Split
large month files before the reader's 8 MiB per-file limit; a manifest may list
multiple files for the same month. The manifest limit is 2 MiB.

The Drive companion lives in `google-drive-mcp-server/`. It is separate from
the live WhatsApp bridge, so saved content can be read without that bridge or
the native Mac database. See its README for cross-platform setup. Folder,
upload, update and bounded-read operations use MCP without screen control.

## Search choice

For occasional narrow class/group/student questions, use the chat/date index
and literal text filter. Consider a semantic index only when repeated requests
need topic searches across many saved chats and a measured comparison shows
better retrieval. Drive remains the source, and an index never expands consent
to collect other chats. Do not claim token/cost savings without measurement.
