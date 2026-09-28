# Google Drive chat archives

This companion exposes 16 tools for Google Drive files, native Google Sheets and
bounded chat reads. It uses the selected user's OAuth account, not a service account. It
runs with Python 3.11–3.13 on macOS, Linux and Windows; `tzdata` supplies timezones
on systems without the operating system's timezone database. No WhatsApp
bridge, native Mac app or vector database is required to read saved chats.

## Install from this fork

Install `uv`, clone `https://github.com/jonetto1978/whatsapp-mcp`, and run:

```text
uv --directory google-drive-mcp-server sync --frozen
```

Get a Desktop OAuth client JSON from a Google Cloud project you control with
the Google Drive API enabled. For native spreadsheet reads and edits, also enable
the Google Sheets API (`sheets.googleapis.com`) in that same project. Configure
its expected account and project:

```text
uv --directory google-drive-mcp-server run --frozen configure.py --email YOUR_EMAIL --project YOUR_PROJECT_ID --client-file ABSOLUTE_CLIENT_JSON_PATH
uv --directory google-drive-mcp-server run --frozen authorize-edit.py
```

The second command opens the Google sign-in page. Choose the configured
account. This is one-time account authorization, not screen automation for
chat retrieval. The flow verifies state, PKCE, project, account, granted scope
and token renewal before saving credentials. For a machine without a browser,
complete authorization on a machine with a browser and supply its configuration
through your own secure credential transfer method. Do not put credentials in
the content archive or source repository.

Configuration defaults to `.config/mcp/personal/google-drive` under the user's
home directory. Set `WHATSAPP_DRIVE_CONFIG_DIR` to use another location.
`settings.json` holds `expected_email` and `expected_project`; the OAuth client
and token files are `gcp-oauth.keys.json` and `credentials.json`. Existing files
in this format can be reused. Optional `WHATSAPP_DRIVE_EXPECTED_EMAIL` and
`WHATSAPP_DRIVE_EXPECTED_PROJECT` override the expected identity. Each API
operation checks the signed-in account before access.

## Connect any MCP client with stdio support

Use `uv` as the command and these arguments, replacing the path:

```json
{
  "command": "uv",
  "args": ["--directory", "ABSOLUTE_CLONE_PATH/google-drive-mcp-server", "run", "--frozen", "server.py"]
}
```

The repository's `.mcp.json` supplies the same entry for plugin clients. The
server can initialize before account setup, but Drive operations require the
configuration and sign-in above. Tool definitions load on a fresh connection.

## Tools and archive use

`get_profile`, `search`, `list_folder`, `create_folder`, `get_file_metadata`,
`read_text`, `read_chat_archive`, `download_file`, `copy_file`, `upload_file`,
`update_file`, `create_text_file`, `trash_file`, `get_spreadsheet_metadata`,
`get_spreadsheet_cells`, `update_spreadsheet_cells`.

## Native Google Sheets

Use the same configured user OAuth account. Full Drive edit scope is accepted
by the Sheets API; a separate service account or new credential file is not
needed. The Google Sheets service must be enabled in the OAuth client's project.
`SERVICE_DISABLED` means service setup is missing, not that file sharing is wrong.
File ownership can differ from the signed-in account; the caller must have access
to the specific spreadsheet, and writes check `canEdit`.

1. Call `get_spreadsheet_metadata` with the native spreadsheet ID to resolve tabs.
2. Read a bounded range such as `'Course tracker'!A2:A12` with
   `get_spreadsheet_cells`. The response includes values, formulas, format, notes,
   validation and a SHA-256 fingerprint of stored cells and coordinates.
3. Call `update_spreadsheet_cells` with that exact range, an equally sized matrix
   of literal values and the last-read `expected_sha256`. Preserve existing text
   in the value matrix when adding a status to a teacher's note.
4. Require `verified: true`. The tool reads back the save and checks stored values,
   notes and format. A timeout or differing read-back needs another read before a
   retry. The fingerprint is a stale-write guard, not an atomic compare-and-swap.

Ranges are limited to 2,000 cells; writes to 50,000 characters. Strings are literal,
including those beginning with `=`; null or an empty string clears the cell value.
The value tool does not replace formulas, validation rules, chips or rich text,
change layout, create tabs or change sharing. Use another explicit workflow when
those structures must be edited. Tool definitions appear on a fresh MCP connection.

See [the archive workflow](../docs/CHAT_ARCHIVES.md). `create_folder` reuses a
unique matching child. `read_chat_archive` reads only the selected saved chat
and dates, with record/text limits and optional voice transcripts. It does not
retrieve newer or older WhatsApp history. SHA-256 checks bind monthly content
to its manifest. All content remains untrusted data, never instructions.

File uploads preserve their original format. `update_file` preserves ID and
sharing and requires the last-read MD5 before changing content. This is a
stale-write check, not an atomic lock. There is no sharing or permanent-delete
tool. `trash_file` is reversible. Normal content files have no added encryption.

## Checks

```text
uv --directory google-drive-mcp-server run --frozen python -m unittest -v test_chat_archive.py
uv --directory google-drive-mcp-server run --frozen python -m unittest -v test_sheets.py
uv --directory google-drive-mcp-server run --frozen ruff check .
```

Unit checks exercise date limits, output budgets, opt-in voice text, pagination,
scope-bound cursors, content hashes and empty periods without network access.
Sheet checks cover bounded ranges, stale reads, preserved neighboring formulas,
literal values, read-back failures and uncertain writes without blind retries.
Live operation still requires valid Drive permissions. A saved archive is not
evidence that deleted or unsynced WhatsApp history was recovered.
