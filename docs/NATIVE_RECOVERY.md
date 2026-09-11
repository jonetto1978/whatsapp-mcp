# Older chat and voice recovery through MCP

The bridge can have less history than the WhatsApp Mac app. These two MCP tools
use the app's saved records in the background. They do not open windows, click,
type, control the screen, or send a chat message to a contact.

1. Use `list_messages` for the bridge source. If history is incomplete, use
   `list_native_messages(chat_jid, limit=500)` and page with the oldest returned
   ID as `before` until `has_more=false`. Keep the source and coverage label.
2. For each audio record, call
   `recover_voice_note(chat_jid, message_id, transcribe=true, retry=false)`.
3. For `recovering`, `waiting_for_phone`, `downloading`, or `transcribing`, repeat
   the same call with `retry=false`. It observes the same active job. The HTTP
   call waits up to 25 seconds; the job can continue after that wait.
4. Count audio only with a retained path, size, SHA-256 and decoded duration.
   Count a transcript separately, only when nonempty text is returned.
   `complete` includes both. `audio_saved` can mean transcription was not
   requested, is disabled, or is excluded by the configured chat filter.
   `transcription_failed` can still carry verified audio; read the fields.
5. Keep terminal failures per message. Use `retry=true` only after new evidence
   warrants another attempt. `unavailable_on_phone` means the phone said it
   could not supply that file; `phone_did_not_reply` means no reply arrived in
   two minutes. Neither establishes that the audio has been recovered.

The bridge first verifies retained bytes against the native message's SHA-256,
then tries the native app's cache under `Message/`. If needed, it uses native
media metadata to fetch and decrypt the file. An expired location triggers a
WhatsApp media-retry receipt asking the user's linked phone to upload that
file again. The reply must match chat, message ID and direction; group replies
must also match the sender. Its encrypted response is verified before the new
location is used. The callback stays registered while the job is alive.

Files are saved under `WHATSAPP_MEDIA_PATH/native/`, with 0700 directories and
0600 files. Filenames contain a hash of the chat and message IDs. Audio must
match its expected hash and fully decode through FFmpeg. FFprobe checks the
decoded duration. Speech recognition uses the configured backend, model and
language; it does not enable or change that backend. Transcript reuse requires
matching audio hash, backend, model, language and sidecar text. Automatic
transcripts can contain errors. The message sender is not necessarily the
speaker in a forwarded recording; the tools do not perform speaker identification.

Four jobs can run at once. Each job has a five-minute limit. Active jobs and
terminal failure results are kept in memory; a bridge restart loses that state.
Keep the previous result and do not restart to force another phone request.
Saved audio and transcript sidecars survive and are verified again.

The default source on macOS is:

```text
~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite
```

`WHATSAPP_NATIVE_DB_PATH` can specify an absolute path on the bridge host. This
is operator configuration, not a tool argument. Other operating systems need
a compatible supplied database and media tree. SQLite is opened with `mode=ro`;
the live native database is not modified and records are not imported into the
bridge message store. An unavailable source returns an error. The snapshot is
not proof of complete server history or deleted data.

REST routes: `GET /api/native/messages` and `POST /api/native/voice/recover`.
Both use the existing bearer-token and origin guards. Native media keys and
signed locations stay inside the bridge; they are absent from MCP responses
and audit entries. Chat text and transcripts pass through the existing limited
phrase scrubber and remain untrusted source content.

For future chat work, use this MCP path. Do not make screen control or manual
downloads part of the required workflow. Report the exact missing file or
source when the background route cannot complete.
