package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protowire"
)

func nativeFixture(t *testing.T) (*Bridge, []byte) {
	t.Helper()
	b, db, _ := mediaBridge(t)
	_, err := db.Exec(`INSERT INTO jid_aliases(jid_a,jid_b,discovered_at,source) VALUES ('111@lid','111@s.whatsapp.net',1,'test'),('111@s.whatsapp.net','111@lid',1,'test')`)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	n, err := sql.Open("sqlite3", p)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	_, err = n.Exec(`CREATE TABLE ZWACHATSESSION(Z_PK INTEGER PRIMARY KEY,ZCONTACTJID TEXT,ZPARTNERNAME TEXT,ZSESSIONTYPE INTEGER);
	CREATE TABLE ZWAMESSAGE(Z_PK INTEGER PRIMARY KEY,ZSTANZAID TEXT,ZCHATSESSION INTEGER,ZMEDIAITEM INTEGER,ZFROMJID TEXT,ZPUSHNAME TEXT,ZMESSAGEDATE REAL,ZMESSAGETYPE INTEGER,ZTEXT TEXT,ZISFROMME INTEGER);
	CREATE TABLE ZWAMEDIAITEM(Z_PK INTEGER PRIMARY KEY,ZMEDIALOCALPATH TEXT,ZMOVIEDURATION INTEGER,ZFILESIZE INTEGER,ZVCARDNAME TEXT,ZVCARDSTRING TEXT,ZMEDIAKEY BLOB,ZMETADATA BLOB);
	INSERT INTO ZWACHATSESSION VALUES(1,'111@s.whatsapp.net','Test Person',0),(2,'222@s.whatsapp.net','Other Person',0);
	INSERT INTO ZWAMESSAGE VALUES(1,'T1',1,NULL,'111@lid',NULL,100.5,0,'Hola á',0),(2,'A1',1,1,'111@lid',NULL,100.5,3,NULL,0),(3,'OTHER',2,NULL,'222@s.whatsapp.net',NULL,101,0,'private',0);`)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic audio fixture")
	hash := sha256.Sum256(data)
	key := protowire.AppendTag(nil, 1, protowire.BytesType)
	key = protowire.AppendBytes(key, bytes.Repeat([]byte{7}, 32))
	key = protowire.AppendTag(key, 2, protowire.BytesType)
	key = protowire.AppendBytes(key, bytes.Repeat([]byte{8}, 32))
	metadata := protowire.AppendTag(nil, 4, protowire.BytesType)
	metadata = protowire.AppendString(metadata, "/expired?credential=must-not-leak")
	_, err = n.Exec(`INSERT INTO ZWAMEDIAITEM VALUES(1,'Media/test.opus',1,? ,?,'audio/ogg; codecs=opus',?,?)`, len(data), base64.StdEncoding.EncodeToString(hash[:]), key, metadata)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(p), "Message", "Media")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "test.opus"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHATSAPP_NATIVE_DB_PATH", p)
	b.cfg.WhisperBackend = "local-cpp"
	b.cfg.WhisperLanguage = "es"
	b.cfg.WhisperModelPath = "fixture-model.bin"
	return b, data
}

func stubNativeSpeech(t *testing.T, fail bool) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	oldD, oldT := nativeDecode, nativeTranscribe
	nativeDecode = func(_ context.Context, _ *Transcriber, _ string, _ string) (string, float64, error) {
		return "fixture.wav", 1.25, nil
	}
	nativeTranscribe = func(_ context.Context, _ *Transcriber, _ string) (string, error) {
		calls.Add(1)
		if fail {
			return "", errors.New("fixture failure")
		}
		return "Mensaje de prueba.", nil
	}
	t.Cleanup(func() { nativeDecode, nativeTranscribe = oldD, oldT })
	return calls
}

func TestNativeReadOnlyAliasPaginationAndNoSecrets(t *testing.T) {
	b, _ := nativeFixture(t)
	ctx := context.Background()
	p, err := b.listNativeMessages(ctx, "111@lid", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Count != 1 || p.TotalRecords != 2 || p.TotalAudioRecords != 1 || p.TotalTextCharacters != 6 || !p.HasMore || p.Messages[0].ID != "A1" || !p.Messages[0].AudioCached {
		t.Fatalf("bad page: %+v", p)
	}
	q, err := b.listNativeMessages(ctx, "111@lid", "A1", 1)
	if err != nil || q.Messages[0].ID != "T1" || q.HasMore {
		t.Fatalf("tie pagination: %+v %v", q, err)
	}
	if _, err := b.listNativeMessages(ctx, "111@lid", "OTHER", 2); err == nil {
		t.Fatal("cross-chat cursor accepted")
	}
	if _, err := b.loadNativeAudio(ctx, "222@s.whatsapp.net", "A1"); err == nil {
		t.Fatal("cross-chat audio accepted")
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "credential") || strings.Contains(string(raw), "media_key") || strings.Contains(string(raw), "private") {
		t.Fatal("leaked metadata or other chat")
	}
	n, _, err := openNativeStore()
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err = n.Exec(`UPDATE ZWAMESSAGE SET ZTEXT='changed'`); err == nil {
		t.Fatal("native store was writable")
	}
}

func TestNativeCachedAudioTranscriptionAndProvenanceReuse(t *testing.T) {
	b, _ := nativeFixture(t)
	calls := stubNativeSpeech(t, false)
	a, err := b.loadNativeAudio(context.Background(), "111@lid", "A1")
	if err != nil {
		t.Fatal(err)
	}
	r := b.recoverNativeVoice(context.Background(), a, true, false)
	if r.State != "complete" || r.Size == 0 || r.Duration <= 0 || r.Transcript == "" || !r.CachedHit || calls.Load() != 1 {
		t.Fatalf("bad recovery: %+v", r)
	}
	st, _ := os.Stat(r.Path)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("audio permissions")
	}
	r = b.recoverNativeVoice(context.Background(), a, true, false)
	if r.State != "complete" || calls.Load() != 1 {
		t.Fatal("matching transcript not reused")
	}
	b.cfg.WhisperModelPath = "another-model.bin"
	r = b.recoverNativeVoice(context.Background(), a, true, false)
	if r.State != "complete" || calls.Load() != 2 {
		t.Fatal("changed model reused old transcript")
	}
}

func TestNativeAudioSavedWhenTranscriptionFails(t *testing.T) {
	b, _ := nativeFixture(t)
	stubNativeSpeech(t, true)
	a, _ := b.loadNativeAudio(context.Background(), "111@lid", "A1")
	r := b.recoverNativeVoice(context.Background(), a, true, false)
	if r.State != "transcription_failed" || r.Path == "" || r.Size == 0 || r.Transcript != "" {
		t.Fatalf("must retain audio separately: %+v", r)
	}
}

func TestNativeExpiredDownloadRetryAndConcurrentCallsCoalesce(t *testing.T) {
	b, data := nativeFixture(t)
	stubNativeSpeech(t, false)
	a, _ := b.loadNativeAudio(context.Background(), "111@lid", "A1")
	os.Remove(filepath.Join(a.mediaRoot, a.localPath))
	oldD, oldR := nativeDownload, nativePhoneRetry
	defer func() { nativeDownload, nativePhoneRetry = oldD, oldR }()
	var downloads, retries atomic.Int32
	sent := make(chan struct{})
	release := make(chan struct{})
	nativeDownload = func(_ context.Context, _ *Bridge, a *nativeAudio, p string) error {
		downloads.Add(1)
		if strings.HasPrefix(a.directPath, "/expired") {
			return whatsmeow.ErrMediaDownloadFailedWith403
		}
		return os.WriteFile(p, data, 0o600)
	}
	nativePhoneRetry = func(_ context.Context, _ *Bridge, _ *nativeAudio, onSent func()) (string, string) {
		retries.Add(1)
		onSent()
		close(sent)
		<-release
		return "/fresh?secret=hidden", ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.recoverNativeVoice(ctx, a, true, false)
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("retry not sent")
	}
	a2, _ := b.loadNativeAudio(context.Background(), "111@s.whatsapp.net", "A1")
	r := b.recoverNativeVoice(ctx, a2, true, false)
	if r.State != "waiting_for_phone" || retries.Load() != 1 {
		t.Fatalf("duplicate request or wrong pending state: %+v", r)
	}
	close(release)
	b.nativeMu.Lock()
	j := b.nativeJobs[nativeVoiceKey(a)]
	b.nativeMu.Unlock()
	select {
	case <-j.done:
	case <-time.After(time.Second):
		t.Fatal("job did not finish")
	}
	r = j.snapshot()
	if r.State != "complete" || !r.PhoneRetrySent || r.CachedHit || downloads.Load() != 2 {
		t.Fatalf("bad recovered result: %+v", r)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "credential") {
		t.Fatal("signed path leaked")
	}
}

func TestNativeUnavailablePhoneDoesNotPretendSuccessOrRetry(t *testing.T) {
	b, _ := nativeFixture(t)
	a, _ := b.loadNativeAudio(context.Background(), "111@lid", "A1")
	os.Remove(filepath.Join(a.mediaRoot, a.localPath))
	oldD, oldR := nativeDownload, nativePhoneRetry
	defer func() { nativeDownload, nativePhoneRetry = oldD, oldR }()
	count := 0
	nativeDownload = func(context.Context, *Bridge, *nativeAudio, string) error {
		return whatsmeow.ErrMediaDownloadFailedWith410
	}
	nativePhoneRetry = func(_ context.Context, _ *Bridge, _ *nativeAudio, onSent func()) (string, string) {
		count++
		onSent()
		return "", "unavailable_on_phone"
	}
	for range 2 {
		r := b.recoverNativeVoice(context.Background(), a, true, false)
		if r.State != "unavailable_on_phone" || r.Path != "" || r.Transcript != "" {
			t.Fatalf("bad failure: %+v", r)
		}
	}
	if count != 1 {
		t.Fatal("terminal failure retried without explicit retry")
	}
	b.recoverNativeVoice(context.Background(), a, true, true)
	if count != 2 {
		t.Fatal("explicit retry did not run")
	}
}

func TestNativeRetryMatchesChatMessageDirectionAndGroupSender(t *testing.T) {
	a := &nativeAudio{id: "A1", chat: "111@s.whatsapp.net", sender: "111@lid"}
	jids := []string{a.chat, "111@lid"}
	e := &events.MediaRetry{MessageID: "A1", ChatID: types.NewJID("111", "lid")}
	if !matchesNativeRetry(e, a, jids) {
		t.Fatal("known alias rejected")
	}
	e.MessageID = "OTHER"
	if matchesNativeRetry(e, a, jids) {
		t.Fatal("wrong id")
	}
	e.MessageID = "A1"
	e.FromMe = true
	if matchesNativeRetry(e, a, jids) {
		t.Fatal("wrong direction")
	}
	e.FromMe = false
	e.ChatID = types.NewJID("222", "s.whatsapp.net")
	if matchesNativeRetry(e, a, jids) {
		t.Fatal("wrong chat")
	}
	a.chat = "123@g.us"
	e.ChatID = types.NewJID("123", "g.us")
	e.SenderID = types.NewJID("222", "lid")
	if matchesNativeRetry(e, a, []string{a.chat}) {
		t.Fatal("wrong group sender")
	}
}

func TestNativeHashAndPathBoundaries(t *testing.T) {
	b, _ := nativeFixture(t)
	a, _ := b.loadNativeAudio(context.Background(), "111@lid", "A1")
	p := filepath.Join(a.mediaRoot, a.localPath)
	os.WriteFile(p, []byte("truncated"), 0o600)
	if _, err := nativeReadVerified(p, a.sha); err == nil {
		t.Fatal("bad hash accepted")
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	link := filepath.Join(b.cfg.MediaPath, "sidecar.json")
	os.Symlink(outside, link)
	if _, err := nativeReadSidecar(b.cfg.MediaPath, link); err == nil {
		t.Fatal("sidecar symlink escaped storage")
	}
	f, err := os.CreateTemp(t.TempDir(), "bounded")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := (boundedNativeFile{f}).WriteAt([]byte("x"), maxNativeAudioBytes+1024); err == nil {
		t.Fatal("write cap bypassed")
	}
	if err := (boundedNativeFile{f}).Truncate(maxNativeAudioBytes + 1025); err == nil {
		t.Fatal("truncate cap bypassed")
	}
}

func TestNativeProtoParserRejectsTruncationAndDuplicateFields(t *testing.T) {
	if _, err := protoBytesField([]byte{10, 32, 1}, 1); err == nil {
		t.Fatal("truncated metadata")
	}
	data := protowire.AppendTag(nil, 1, protowire.BytesType)
	data = protowire.AppendBytes(data, []byte{1})
	data = protowire.AppendTag(data, 1, protowire.BytesType)
	data = protowire.AppendBytes(data, []byte{2})
	if _, err := protoBytesField(data, 1); err == nil {
		t.Fatal("ambiguous metadata")
	}
}
