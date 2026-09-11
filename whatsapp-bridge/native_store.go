package main

// The macOS app is an additional, read-only source. Never import its rows into
// the bridge's message DB: source provenance and history coverage stay distinct.
import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

const appleEpoch = 978307200
const maxNativeAudioBytes = 64 << 20

func nativeDatabasePath() (string, error) {
	if p := os.Getenv("WHATSAPP_NATIVE_DB_PATH"); p != "" {
		if !filepath.IsAbs(p) {
			return "", errors.New("WHATSAPP_NATIVE_DB_PATH must be absolute")
		}
		return p, nil
	}
	if runtime.GOOS != "darwin" {
		return "", errors.New("native chat source unavailable: macOS database not configured")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared", "ChatStorage.sqlite"), nil
}

func openNativeStore() (*sql.DB, string, error) {
	p, err := nativeDatabasePath()
	if err != nil {
		return nil, "", err
	}
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
		return nil, "", errors.New("native chat database is unavailable or unreadable")
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(p)}
	u.RawQuery = "mode=ro&_query_only=1&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, "", errors.New("cannot open native chat database read-only")
	}
	db.SetMaxOpenConns(1)
	return db, filepath.Join(filepath.Dir(p), "Message"), nil
}

type nativeMessageRow struct {
	messageRow
	NativeType  int  `json:"native_type"`
	AudioCached bool `json:"audio_cached"`
	Duration    int  `json:"declared_duration_seconds,omitempty"`
}

type nativeMessagesResponse struct {
	Source              string             `json:"source"`
	Messages            []nativeMessageRow `json:"messages"`
	Count               int                `json:"count"`
	TextCharacters      int                `json:"text_characters"`
	TotalRecords        int                `json:"total_records"`
	TotalTextCharacters int                `json:"total_text_characters"`
	TotalAudioRecords   int                `json:"total_audio_records"`
	OldestTimestamp     int64              `json:"oldest_timestamp"`
	NewestTimestamp     int64              `json:"newest_timestamp"`
	MergedJIDs          []string           `json:"merged_jids"`
	HasMore             bool               `json:"has_more"`
	Coverage            string             `json:"coverage"`
}

func (b *Bridge) nativeAliases(ctx context.Context, jid string) ([]string, error) {
	if jid == "" || len(jid) > 200 || !strings.Contains(jid, "@") {
		return nil, errors.New("valid chat_jid required")
	}
	return resolveAliases(ctx, b.db, jid)
}

func (b *Bridge) listNativeMessages(ctx context.Context, jid, before string, limit int) (*nativeMessagesResponse, error) {
	jids, err := b.nativeAliases(ctx, jid)
	if err != nil {
		return nil, err
	}
	db, mediaRoot, err := openNativeStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if limit < 1 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	where := "c.ZCONTACTJID IN (" + inClausePlaceholders(len(jids)) + ")"
	from := " FROM ZWAMESSAGE m JOIN ZWACHATSESSION c ON c.Z_PK=m.ZCHATSESSION "
	args := jidsToArgs(jids)
	out := &nativeMessagesResponse{Source: "whatsapp_macos", Messages: []nativeMessageRow{}, MergedJIDs: jids,
		Coverage: "Available native-app snapshot only; does not prove complete server history or recover deleted messages."}
	err = db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(COALESCE(m.ZTEXT,''))),0),
		COALESCE(SUM(m.ZMESSAGETYPE=3),0),CAST(COALESCE(MIN(m.ZMESSAGEDATE)+978307200,0) AS INTEGER),
		CAST(COALESCE(MAX(m.ZMESSAGEDATE)+978307200,0) AS INTEGER)`+from+" WHERE "+where, args...).Scan(
		&out.TotalRecords, &out.TotalTextCharacters, &out.TotalAudioRecords, &out.OldestTimestamp, &out.NewestTimestamp)
	if err != nil {
		return nil, errors.New("unsupported or unreadable native chat schema")
	}
	if before != "" {
		var ts float64
		var pk, count int64
		err = db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(m.ZMESSAGEDATE,0),COALESCE(m.Z_PK,0)"+from+" WHERE "+where+" AND m.ZSTANZAID=?", append(args, before)...).Scan(&count, &ts, &pk)
		if err != nil || count != 1 {
			return nil, errors.New("before must identify one message in this native chat")
		}
		where += " AND (m.ZMESSAGEDATE<? OR (m.ZMESSAGEDATE=? AND m.Z_PK<?))"
		args = append(args, ts, ts, pk)
	}
	query := `SELECT COALESCE(m.ZSTANZAID,''),c.ZCONTACTJID,COALESCE(m.ZFROMJID,''),
		CASE WHEN m.ZISFROMME=1 THEN 'You' WHEN c.ZSESSIONTYPE=0 THEN COALESCE(c.ZPARTNERNAME,'') ELSE COALESCE(m.ZPUSHNAME,m.ZFROMJID,'') END,
		CAST(m.ZMESSAGEDATE+978307200 AS INTEGER),m.ZMESSAGETYPE,COALESCE(m.ZTEXT,''),m.ZISFROMME,
		COALESCE(i.ZMEDIALOCALPATH,''),COALESCE(i.ZMOVIEDURATION,0)` + from +
		" LEFT JOIN ZWAMEDIAITEM i ON i.Z_PK=m.ZMEDIAITEM WHERE " + where + " ORDER BY m.ZMESSAGEDATE DESC,m.Z_PK DESC LIMIT ?"
	rows, err := db.QueryContext(ctx, query, append(args, limit+1)...)
	if err != nil {
		return nil, errors.New("unsupported or unreadable native chat schema")
	}
	defer rows.Close()
	for rows.Next() {
		var m nativeMessageRow
		var local string
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.SenderJID, &m.SenderDisplay, &m.Timestamp, &m.NativeType, &m.ContentText, &m.IsFromMe, &local, &m.Duration); err != nil {
			return nil, err
		}
		m.Type = fmt.Sprintf("native:%d", m.NativeType)
		if m.NativeType == 0 {
			m.Type = "text"
		}
		if m.NativeType == 3 {
			m.Type = "audio"
		}
		if local != "" && m.NativeType == 3 {
			p := filepath.Join(mediaRoot, local)
			if st, e := os.Stat(p); e == nil && st.Mode().IsRegular() && st.Size() > 0 && insideMediaDir(mediaRoot, p) {
				m.AudioCached = true
			}
		}
		out.Messages = append(out.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Messages) > limit {
		out.Messages = out.Messages[:limit]
		out.HasMore = true
	}
	out.Count = len(out.Messages)
	for _, m := range out.Messages {
		out.TextCharacters += len([]rune(m.ContentText))
	}
	return out, nil
}

// These fields must never be JSON-encoded, audited, or put in errors. Keys and
// signed paths pass only from the read-only native store to whatsmeow in memory.
type nativeAudio struct {
	id, chat, sender, name, localPath, mediaRoot, directPath, mime string
	fromMe                                                         bool
	size                                                           int64
	key, encSHA, sha                                               []byte
}

func protoBytesField(data []byte, wanted protowire.Number) ([]byte, error) {
	var found []byte
	for len(data) > 0 {
		n, t, k := protowire.ConsumeTag(data)
		if k < 0 {
			return nil, errors.New("unsupported native media metadata")
		}
		data = data[k:]
		if n == wanted && t == protowire.BytesType {
			v, k := protowire.ConsumeBytes(data)
			if k < 0 || found != nil {
				return nil, errors.New("unsupported native media metadata")
			}
			found = append([]byte(nil), v...)
			data = data[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, t, data)
			if k < 0 {
				return nil, errors.New("unsupported native media metadata")
			}
			data = data[k:]
		}
	}
	return found, nil
}

func (b *Bridge) loadNativeAudio(ctx context.Context, jid, id string) (*nativeAudio, error) {
	if id == "" || len(id) > 256 {
		return nil, errors.New("valid message_id required")
	}
	jids, err := b.nativeAliases(ctx, jid)
	if err != nil {
		return nil, err
	}
	db, root, err := openNativeStore()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	a := &nativeAudio{id: id, mediaRoot: root}
	var rawKey, metadata []byte
	var hash string
	var nativeType int
	err = db.QueryRowContext(ctx, `SELECT c.ZCONTACTJID,COALESCE(m.ZFROMJID,''),COALESCE(c.ZPARTNERNAME,''),m.ZISFROMME,m.ZMESSAGETYPE,
		COALESCE(i.ZMEDIALOCALPATH,''),COALESCE(i.ZFILESIZE,0),COALESCE(i.ZVCARDNAME,''),COALESCE(i.ZVCARDSTRING,''),i.ZMEDIAKEY,i.ZMETADATA
		FROM ZWAMESSAGE m JOIN ZWACHATSESSION c ON c.Z_PK=m.ZCHATSESSION LEFT JOIN ZWAMEDIAITEM i ON i.Z_PK=m.ZMEDIAITEM
		WHERE c.ZCONTACTJID IN (`+inClausePlaceholders(len(jids))+`) AND m.ZSTANZAID=?`, append(jidsToArgs(jids), id)...).Scan(
		&a.chat, &a.sender, &a.name, &a.fromMe, &nativeType, &a.localPath, &a.size, &hash, &a.mime, &rawKey, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("message not found in this native chat")
	}
	if err != nil {
		return nil, errors.New("unsupported or unreadable native media schema")
	}
	if nativeType != 3 {
		return nil, errors.New("native message is not audio")
	}
	if a.size < 0 || a.size > maxNativeAudioBytes {
		return nil, errors.New("native audio exceeds the 64 MiB limit")
	}
	a.sha, err = base64.StdEncoding.DecodeString(hash)
	if err != nil || len(a.sha) != 32 {
		return nil, errors.New("native audio is missing a valid file hash")
	}
	// The cache remains usable if this app version has unknown key metadata.
	a.key, _ = protoBytesField(rawKey, 1)
	a.encSHA, _ = protoBytesField(rawKey, 2)
	dp, _ := protoBytesField(metadata, 4)
	a.directPath = string(dp)
	if !strings.HasPrefix(a.directPath, "/") || strings.HasPrefix(a.directPath, "//") || strings.ContainsAny(a.directPath, "\r\n") {
		a.directPath = ""
	}
	return a, nil
}

func (s *Server) handleNativeMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := s.bridge.listNativeMessages(r.Context(), q.Get("chat_jid"), q.Get("before"), atoiDefaultPositive(q.Get("limit"), 20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}
