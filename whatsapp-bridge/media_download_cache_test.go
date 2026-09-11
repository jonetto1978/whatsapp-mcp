package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
)

// DownloadMedia has three outcomes for a row that already carries a
// media_path: serve the file (cache), fetch again (file gone / empty / outside
// the media folder), or fail. The REST cached_hit flag used to be inferred
// after the fact from "stored path == returned path && size > 0", which is also
// true right after a deleted file was re-downloaded to its deterministic name
// (Fable review 2026-09-10, B3). These tests pin the flag to the branch that
// actually ran, with the network step replaced by synthetic bytes.

const mediaMsgID = "3A0F1B2C3D4E5F60"

func mediaBridge(t *testing.T) (*Bridge, *sql.DB, string) {
	t.Helper()
	db, err := sql.Open("sqlite3", t.TempDir()+"/media.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	insertChat(t, db, "c@s.whatsapp.net", "direct", "")
	if _, err := db.Exec(`
		INSERT INTO messages (id, chat_jid, timestamp, type, media_key, media_mime, media_direct_path, media_enc_sha256, media_sha256)
		VALUES (?, 'c@s.whatsapp.net', 1000, 'voice', X'0102', 'audio/ogg', '/v/t62.1/x', X'aa', X'bb')`, mediaMsgID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	mediaDir := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{db: db, cfg: &Config{MediaPath: mediaDir}}
	return b, db, mediaDir
}

// stubDownload replaces the network step with fixed bytes and counts calls.
func stubDownload(t *testing.T, payload []byte) *int {
	t.Helper()
	calls := 0
	prev := downloadMediaBytes
	downloadMediaBytes = func(_ context.Context, _ *Bridge, _ whatsmeow.DownloadableMessage) ([]byte, error) {
		calls++
		return payload, nil
	}
	t.Cleanup(func() { downloadMediaBytes = prev })
	return &calls
}

func setMediaPath(t *testing.T, db *sql.DB, p string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE messages SET media_path = ? WHERE id = ?`, p, mediaMsgID); err != nil {
		t.Fatal(err)
	}
}

func storedMediaPath(t *testing.T, db *sql.DB) sql.NullString {
	t.Helper()
	var p sql.NullString
	if err := db.QueryRow(`SELECT media_path FROM messages WHERE id = ?`, mediaMsgID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDownloadMediaCachedHitServesExistingFile(t *testing.T) {
	b, db, dir := mediaBridge(t)
	calls := stubDownload(t, []byte("must not be fetched"))
	existing := filepath.Join(dir, mediaMsgID+".ogg")
	if err := os.WriteFile(existing, []byte("cached-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	setMediaPath(t, db, existing)

	path, mt, size, cached, err := b.DownloadMedia(context.Background(), mediaMsgID)
	if err != nil {
		t.Fatalf("DownloadMedia: %v", err)
	}
	if !cached {
		t.Fatal("cached = false, want true for an existing nonempty file inside the media folder")
	}
	if path != existing || size != int64(len("cached-bytes")) || mt != "audio/ogg" {
		t.Fatalf("path=%q size=%d mime=%q", path, size, mt)
	}
	if *calls != 0 {
		t.Fatalf("network step called %d times on a cache hit, want 0", *calls)
	}
}

func TestDownloadMediaRedownloadAfterDeletionIsNotACacheHit(t *testing.T) {
	b, db, dir := mediaBridge(t)
	calls := stubDownload(t, []byte("fresh"))
	// An earlier download recorded this path; the bytes were later deleted.
	gone := filepath.Join(dir, mediaMsgID+".ogg")
	setMediaPath(t, db, gone)

	path, _, size, cached, err := b.DownloadMedia(context.Background(), mediaMsgID)
	if err != nil {
		t.Fatalf("DownloadMedia: %v", err)
	}
	if cached {
		t.Fatal("cached = true after the bytes were fetched again; want false")
	}
	if *calls != 1 {
		t.Fatalf("network step called %d times, want 1", *calls)
	}
	// Same deterministic name as before: exactly the case the old inference got wrong.
	if path != gone {
		t.Fatalf("path = %q, want the deterministic %q", path, gone)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "fresh" || size != 5 {
		t.Fatalf("file = %q (err %v) size = %d, want \"fresh\" / 5", got, err, size)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", st.Mode().Perm())
	}
}

func TestDownloadMediaHandlerReportsCacheEvidenceNotPathEquality(t *testing.T) {
	b, db, dir := mediaBridge(t)
	stubDownload(t, []byte("fresh"))
	gone := filepath.Join(dir, mediaMsgID+".ogg")
	setMediaPath(t, db, gone) // path recorded, file absent

	s := &Server{db: db, bridge: b}
	call := func() downloadMediaResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/media/download", strings.NewReader(`{"message_id":"`+mediaMsgID+`"}`))
		rr := httptest.NewRecorder()
		s.handleDownloadMedia(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
		}
		var out downloadMediaResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Public schema unchanged: these keys still exist.
		var raw map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &raw)
		for _, k := range []string{"message_id", "path", "mime", "size"} {
			if _, ok := raw[k]; !ok {
				t.Fatalf("response lost key %q: %s", k, rr.Body.String())
			}
		}
		return out
	}

	first := call()
	if first.CachedHit {
		t.Fatalf("first call re-downloaded to %s yet reported cached_hit=true", first.Path)
	}
	second := call()
	if !second.CachedHit {
		t.Fatal("second call served the file just written yet reported cached_hit=false")
	}
	if first.Path != second.Path || first.Size != second.Size {
		t.Fatalf("first=%+v second=%+v, want the same file", first, second)
	}
}

func TestDownloadMediaEmptyCachedFileIsRefetched(t *testing.T) {
	b, db, dir := mediaBridge(t)
	calls := stubDownload(t, []byte("real-audio"))
	empty := filepath.Join(dir, mediaMsgID+".ogg")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setMediaPath(t, db, empty)

	path, _, size, cached, err := b.DownloadMedia(context.Background(), mediaMsgID)
	if err != nil {
		t.Fatalf("DownloadMedia: %v", err)
	}
	if cached || *calls != 1 {
		t.Fatalf("cached=%v calls=%d, want false and 1 (a zero-byte file is not a cache hit)", cached, *calls)
	}
	if size != int64(len("real-audio")) {
		t.Fatalf("size = %d, want %d", size, len("real-audio"))
	}
	if got, _ := os.ReadFile(path); string(got) != "real-audio" {
		t.Fatalf("file = %q, want the refetched bytes", got)
	}
}

func TestDownloadMediaEmptyPayloadIsAnError(t *testing.T) {
	b, db, dir := mediaBridge(t)
	calls := stubDownload(t, []byte{})

	_, _, _, cached, err := b.DownloadMedia(context.Background(), mediaMsgID)
	if err == nil || !strings.Contains(err.Error(), "empty payload") {
		t.Fatalf("err = %v, want an empty-payload error", err)
	}
	if cached || *calls != 1 {
		t.Fatalf("cached=%v calls=%d, want false and 1", cached, *calls)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("an empty payload wrote %d file(s) into the media folder", len(entries))
	}
	if p := storedMediaPath(t, db); p.Valid {
		t.Fatalf("media_path = %q after a failed fetch, want NULL", p.String)
	}
}

func TestDownloadMediaStoredPathOutsideMediaDirIsRefetched(t *testing.T) {
	b, db, dir := mediaBridge(t)
	calls := stubDownload(t, []byte("inside"))
	outside := filepath.Join(t.TempDir(), "elsewhere.ogg")
	if err := os.WriteFile(outside, []byte("not-ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	setMediaPath(t, db, outside)

	path, _, _, cached, err := b.DownloadMedia(context.Background(), mediaMsgID)
	if err != nil {
		t.Fatalf("DownloadMedia: %v", err)
	}
	if cached || *calls != 1 {
		t.Fatalf("cached=%v calls=%d, want false and 1", cached, *calls)
	}
	if !insideMediaDir(dir, path) {
		t.Fatalf("path %q is not inside the media folder", path)
	}
	if p := storedMediaPath(t, db); p.String != path {
		t.Fatalf("media_path = %q, want %q", p.String, path)
	}
}

func TestDownloadMediaDeclaredSizeCapStillRefusesBeforeFetch(t *testing.T) {
	b, db, _ := mediaBridge(t)
	calls := stubDownload(t, []byte("x"))
	if _, err := db.Exec(`UPDATE messages SET media_file_length = ? WHERE id = ?`, maxInboundMediaBytes+1, mediaMsgID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := b.DownloadMedia(context.Background(), mediaMsgID); err == nil || !strings.Contains(err.Error(), "bridge cap") {
		t.Fatalf("err = %v, want the size-cap refusal", err)
	}
	if *calls != 0 {
		t.Fatalf("network step called %d times past the declared cap, want 0", *calls)
	}
}
