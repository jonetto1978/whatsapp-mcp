package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type nativeVoiceResult struct {
	MessageID            string  `json:"message_id"`
	ChatJID              string  `json:"chat_jid"`
	Source               string  `json:"source"`
	State                string  `json:"state"`
	Error                string  `json:"error,omitempty"`
	Path                 string  `json:"path,omitempty"`
	Mime                 string  `json:"mime,omitempty"`
	Size                 int64   `json:"size,omitempty"`
	SHA256               string  `json:"sha256,omitempty"`
	Duration             float64 `json:"duration_seconds,omitempty"`
	CachedHit            bool    `json:"cached_hit"`
	PhoneRetrySent       bool    `json:"phone_retry_sent"`
	Transcript           string  `json:"voice_note_transcript,omitempty"`
	TranscriptPath       string  `json:"transcript_path,omitempty"`
	TranscriptCharacters int     `json:"transcript_characters,omitempty"`
	Backend              string  `json:"transcript_backend,omitempty"`
	Model                string  `json:"transcript_model,omitempty"`
	Language             string  `json:"transcript_language,omitempty"`
}

type nativeVoiceJob struct {
	mu     sync.Mutex
	done   chan struct{}
	result nativeVoiceResult
}

func (j *nativeVoiceJob) snapshot() nativeVoiceResult {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result
}
func (j *nativeVoiceJob) update(f func(*nativeVoiceResult)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f(&j.result)
}
func (j *nativeVoiceJob) fail(state, detail string) {
	j.update(func(r *nativeVoiceResult) { r.State = state; r.Error = detail })
}

func nativeVoiceKey(a *nativeAudio) string {
	h := sha256.Sum256([]byte(a.chat + "\x00" + a.id))
	return hex.EncodeToString(h[:])
}

func (b *Bridge) recoverNativeVoice(ctx context.Context, a *nativeAudio, transcribe, retry bool) nativeVoiceResult {
	key := nativeVoiceKey(a)
	b.nativeMu.Lock()
	if b.nativeJobs == nil {
		b.nativeJobs = make(map[string]*nativeVoiceJob)
	}
	j := b.nativeJobs[key]
	if j != nil {
		select {
		case <-j.done:
			r := j.snapshot()
			// Failed retries remain terminal until explicitly retried. Successful
			// calls recheck file/hash/sidecar, so deleted files aren't cache hits.
			if !retry && r.State != "complete" && r.State != "audio_saved" {
				b.nativeMu.Unlock()
				return r
			}
			j = nil
		default:
		}
	}
	if j == nil {
		active := 0
		for k, v := range b.nativeJobs {
			select {
			case <-v.done:
				if len(b.nativeJobs) > 512 {
					delete(b.nativeJobs, k)
				}
			default:
				active++
			}
		}
		if active >= 4 {
			b.nativeMu.Unlock()
			return nativeVoiceResult{MessageID: a.id, ChatJID: a.chat, Source: "whatsapp_macos", State: "busy", Error: "Four voice recovery jobs are running; retry after one finishes."}
		}
		j = &nativeVoiceJob{done: make(chan struct{}), result: nativeVoiceResult{MessageID: a.id, ChatJID: a.chat, Source: "whatsapp_macos", State: "recovering"}}
		b.nativeJobs[key] = j
		go func() {
			defer close(j.done)
			root := b.rootCtx
			if root == nil {
				root = context.Background()
			}
			work, cancel := context.WithTimeout(root, 5*time.Minute)
			defer cancel()
			b.runNativeVoice(work, a, j, transcribe)
		}()
	}
	b.nativeMu.Unlock()
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-ctx.Done():
	case <-timer.C:
	}
	return j.snapshot()
}

func nativeReadVerified(p string, expected []byte) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxNativeAudioBytes {
		return nil, errors.New("invalid audio file size")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxNativeAudioBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("audio file is still changing")
	}
	h := sha256.Sum256(data)
	if len(data) > maxNativeAudioBytes || !bytes.Equal(h[:], expected) {
		return nil, errors.New("audio file hash mismatch")
	}
	return data, nil
}

func nativeAtomicWrite(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".native-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Cap writes as well as declared size. whatsmeow streams ciphertext and plaintext
// through this File interface, without buffering a server response in memory.
type boundedNativeFile struct{ *os.File }

func (f boundedNativeFile) Write(p []byte) (int, error) {
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if off+int64(len(p)) > maxNativeAudioBytes+1024 {
		return 0, errors.New("audio limit exceeded")
	}
	return f.File.Write(p)
}
func (f boundedNativeFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > maxNativeAudioBytes+1024 {
		return 0, errors.New("audio limit exceeded")
	}
	return f.File.WriteAt(p, off)
}
func (f boundedNativeFile) Truncate(n int64) error {
	if n < 0 || n > maxNativeAudioBytes+1024 {
		return errors.New("audio limit exceeded")
	}
	return f.File.Truncate(n)
}

var nativeDownload = func(ctx context.Context, b *Bridge, a *nativeAudio, p string) error {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	msg := &waE2E.AudioMessage{DirectPath: proto.String(a.directPath), MediaKey: a.key, FileEncSHA256: a.encSHA, FileSHA256: a.sha, FileLength: proto.Uint64(uint64(a.size)), Mimetype: proto.String(a.mime)}
	dl, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	return b.client.DownloadToFile(dl, msg, boundedNativeFile{f})
}

// A retry is protocol traffic to the user's linked phone, not a chat message.
// Register BEFORE sending, match chat/id/direction, then decrypt the receipt.
// This handler lives with the background job, including after a tool wait ends.
var nativePhoneRetry = func(ctx context.Context, b *Bridge, a *nativeAudio, onSent func()) (string, string) {
	if b.client == nil {
		return "", "bridge_not_connected"
	}
	jids, err := b.nativeAliases(ctx, a.chat)
	if err != nil {
		return "", "alias_lookup_failed"
	}
	chat, err := types.ParseJID(a.chat)
	if err != nil {
		return "", "invalid_chat"
	}
	sender, _ := types.ParseJID(a.sender)
	ch := make(chan *events.MediaRetry, 1)
	handler := b.client.AddEventHandler(func(raw any) {
		e, ok := raw.(*events.MediaRetry)
		if !ok || !matchesNativeRetry(e, a, jids) {
			return
		}
		select {
		case ch <- e:
		default:
		}
	})
	defer b.client.RemoveEventHandler(handler)
	info := &types.MessageInfo{ID: a.id, MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: a.fromMe, IsGroup: chat.Server == types.GroupServer}}
	if err := b.client.SendMediaRetryReceipt(ctx, info, a.key); err != nil {
		return "", "phone_retry_send_failed"
	}
	onSent()
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", "recovery_cancelled"
	case <-timer.C:
		return "", "phone_did_not_reply"
	case evt := <-ch:
		data, err := whatsmeow.DecryptMediaRetryNotification(evt, a.key)
		if errors.Is(err, whatsmeow.ErrMediaNotAvailableOnPhone) {
			return "", "unavailable_on_phone"
		}
		if err != nil {
			return "", "phone_response_decrypt_failed"
		}
		if data.GetStanzaID() != "" && data.GetStanzaID() != a.id {
			return "", "phone_response_message_mismatch"
		}
		if data.GetResult() == waMmsRetry.MediaRetryNotification_NOT_FOUND {
			return "", "unavailable_on_phone"
		}
		if data.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS {
			return "", "phone_could_not_upload"
		}
		p := data.GetDirectPath()
		if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\r\n") {
			return "", "invalid_phone_media_path"
		}
		return p, ""
	}
}

func matchesNativeRetry(e *events.MediaRetry, a *nativeAudio, jids []string) bool {
	if e.MessageID != a.id || e.FromMe != a.fromMe {
		return false
	}
	matched := false
	for _, jid := range jids {
		if e.ChatID.ToNonAD().String() == jid {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if e.ChatID.Server == types.GroupServer {
		sender, err := types.ParseJID(a.sender)
		return err == nil && e.SenderID.ToNonAD() == sender.ToNonAD()
	}
	return true
}

var nativeDecode = func(ctx context.Context, t *Transcriber, p, work string) (string, float64, error) {
	ffmpeg := t.ffmpegBin()
	wav := filepath.Join(work, "audio.wav")
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-xerror", "-y", "-i", p, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", wav)
	if err := cmd.Run(); err != nil {
		return "", 0, errors.New("audio could not be fully decoded by ffmpeg")
	}
	probe := "ffprobe"
	if filepath.IsAbs(ffmpeg) {
		probe = filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	}
	out, err := exec.CommandContext(ctx, probe, "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", wav).Output()
	if err != nil {
		return "", 0, errors.New("audio duration could not be read by ffprobe")
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration <= 0 {
		return "", 0, errors.New("decoded audio is empty")
	}
	return wav, duration, nil
}

var nativeTranscribe = func(ctx context.Context, t *Transcriber, wav string) (string, error) {
	switch t.cfg.WhisperBackend {
	case "local-cpp":
		return t.transcribeLocalCpp(ctx, wav)
	case "openai-api":
		return t.transcribeOpenAI(ctx, wav)
	}
	return "", errors.New("transcription backend is off")
}

func (b *Bridge) runNativeVoice(ctx context.Context, a *nativeAudio, j *nativeVoiceJob, transcribe bool) {
	dir := filepath.Join(b.cfg.MediaPath, "native")
	if err := os.MkdirAll(dir, 0o700); err != nil || !insideMediaDir(b.cfg.MediaPath, dir) {
		j.fail("storage_failed", "Native audio storage is unavailable.")
		return
	}
	path := filepath.Join(dir, nativeVoiceKey(a)+".ogg")
	var data []byte
	err := errors.New("no verified cache")
	if insideMediaDir(dir, path) {
		data, err = nativeReadVerified(path, a.sha)
	}
	cached := err == nil
	if err != nil && a.localPath != "" {
		p := filepath.Join(a.mediaRoot, a.localPath)
		if insideMediaDir(a.mediaRoot, p) {
			data, err = nativeReadVerified(p, a.sha)
			cached = err == nil
		}
	}
	if err != nil {
		if len(a.key) != 32 || len(a.encSHA) != 32 {
			j.fail("metadata_unavailable", "Native media keys are absent or use an unsupported format.")
			return
		}
		f, e := os.CreateTemp(dir, ".download-")
		if e != nil {
			j.fail("storage_failed", "Cannot create audio file.")
			return
		}
		tmp := f.Name()
		f.Close()
		defer os.Remove(tmp)
		if a.directPath != "" {
			err = nativeDownload(ctx, b, a, tmp)
		} else {
			err = whatsmeow.ErrNoURLPresent
		}
		if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) || errors.Is(err, whatsmeow.ErrNoURLPresent) {
			p, reason := nativePhoneRetry(ctx, b, a, func() {
				j.update(func(r *nativeVoiceResult) { r.State = "waiting_for_phone"; r.PhoneRetrySent = true })
			})
			if reason != "" {
				j.fail(reason, "The voice file has not been recovered. A sent retry is not a recovered file.")
				return
			}
			a.directPath = p
			j.update(func(r *nativeVoiceResult) { r.State = "downloading" })
			err = nativeDownload(ctx, b, a, tmp)
		}
		if err != nil {
			j.fail("download_failed", "WhatsApp media download failed; no audio was counted as recovered.")
			return
		}
		data, err = nativeReadVerified(tmp, a.sha)
		if err != nil {
			j.fail("audio_invalid", "Downloaded audio did not match the native message's file hash.")
			return
		}
	}
	// Always atomically replace, never follow a stale destination-file symlink.
	if err := nativeAtomicWrite(path, data); err != nil {
		j.fail("storage_failed", "Cannot retain recovered audio.")
		return
	}
	t := &Transcriber{cfg: b.cfg, db: b.db}
	work, err := os.MkdirTemp(dir, ".decode-")
	if err != nil {
		j.fail("storage_failed", "Cannot create decode directory.")
		return
	}
	defer os.RemoveAll(work)
	wav, duration, err := nativeDecode(ctx, t, path, work)
	if err != nil {
		j.fail("audio_invalid", err.Error())
		return
	}
	j.update(func(r *nativeVoiceResult) {
		r.State = "audio_saved"
		r.Path = path
		r.Mime = a.mime
		r.Size = int64(len(data))
		r.SHA256 = hex.EncodeToString(a.sha)
		r.Duration = duration
		r.CachedHit = cached
	})
	if !transcribe {
		return
	}
	if b.cfg.WhisperBackend == "off" {
		j.update(func(r *nativeVoiceResult) {
			r.Error = "Audio saved. Transcription is disabled in the bridge configuration."
		})
		return
	}
	// Respect the configured exclusions using the native name and every alias.
	jids, err := b.nativeAliases(ctx, a.chat)
	if err != nil {
		j.fail("transcription_failed", "Cannot check transcription exclusions.")
		return
	}
	identity := Normalize(a.name + " " + strings.Join(jids, " "))
	for _, excluded := range b.cfg.WhisperExcludeChats {
		if strings.Contains(identity, excluded) {
			j.update(func(r *nativeVoiceResult) { r.Error = "Audio saved. This chat is excluded from transcription." })
			return
		}
	}
	model := b.cfg.WhisperModel
	if b.cfg.WhisperBackend == "local-cpp" {
		model = b.cfg.WhisperModelPath
	}
	metaPath := strings.TrimSuffix(path, ".ogg") + ".json"
	txtPath := strings.TrimSuffix(path, ".ogg") + ".txt"
	var previous nativeVoiceResult
	if raw, e := nativeReadSidecar(dir, metaPath); e == nil && json.Unmarshal(raw, &previous) == nil && previous.MessageID == a.id && previous.ChatJID == a.chat && previous.SHA256 == hex.EncodeToString(a.sha) && previous.Backend == b.cfg.WhisperBackend && previous.Model == model && previous.Language == b.cfg.WhisperLanguage && strings.TrimSpace(previous.Transcript) != "" {
		if text, e := nativeReadSidecar(dir, txtPath); e == nil && string(text) == previous.Transcript+"\n" {
			j.update(func(r *nativeVoiceResult) {
				r.State = "complete"
				r.Transcript = previous.Transcript
				r.TranscriptPath = txtPath
				r.TranscriptCharacters = len([]rune(previous.Transcript))
				r.Backend = previous.Backend
				r.Model = previous.Model
				r.Language = previous.Language
			})
			return
		}
	}
	j.update(func(r *nativeVoiceResult) { r.State = "transcribing" })
	text, err := nativeTranscribe(ctx, t, wav)
	text = strings.TrimSpace(text)
	if err != nil || text == "" {
		j.fail("transcription_failed", "Audio saved, but the configured speech engine did not return a nonempty transcript.")
		return
	}
	if err := nativeAtomicWrite(txtPath, []byte(text+"\n")); err != nil {
		j.fail("transcription_failed", "Audio saved, but the transcript file could not be written.")
		return
	}
	j.update(func(r *nativeVoiceResult) {
		r.State = "complete"
		r.Transcript = text
		r.TranscriptPath = txtPath
		r.TranscriptCharacters = len([]rune(text))
		r.Backend = b.cfg.WhisperBackend
		r.Model = model
		r.Language = b.cfg.WhisperLanguage
	})
	raw, _ := json.MarshalIndent(j.snapshot(), "", "  ")
	if err := nativeAtomicWrite(metaPath, raw); err != nil {
		j.fail("transcription_failed", "Audio and transcript saved, but the provenance file could not be written.")
	}
}

func (s *Server) handleRecoverNativeVoice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatJID    string `json:"chat_jid"`
		MessageID  string `json:"message_id"`
		Transcribe bool   `json:"transcribe"`
		Retry      bool   `json:"retry"`
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid voice recovery request"})
		return
	}
	a, err := s.bridge.loadNativeAudio(r.Context(), body.ChatJID, body.MessageID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.bridge.recoverNativeVoice(r.Context(), a, body.Transcribe, body.Retry))
}

func nativeReadSidecar(root, path string) ([]byte, error) {
	if !insideMediaDir(root, path) {
		return nil, errors.New("sidecar outside audio storage")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 8<<20 {
		return nil, errors.New("invalid sidecar")
	}
	data, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if len(data) > 8<<20 {
		return nil, errors.New("sidecar too large")
	}
	return data, err
}

var _ whatsmeow.File = boundedNativeFile{}
