package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Bearer-token guard for the HTTP API (added 2026-09-02 after review).
//
// The API binds to loopback and is guarded against browser CSRF, but until
// now ANY local process could read every message and send as the user — the
// SQLCipher-at-rest control was bypassed by an unauthenticated API in front
// of it. The token is minted once into a 0600 file next to the database and
// read by the Python MCP server; nothing else needs to know it.

const bridgeTokenFileName = "bridge.token"

func bridgeTokenPath(cfg *Config) string {
	if p := os.Getenv("WHATSAPP_BRIDGE_TOKEN_FILE"); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(cfg.DBPath), bridgeTokenFileName)
}

// loadOrMintBridgeToken returns the token, minting a 32-byte random one into
// the token file (0600) if it does not exist yet. The value is never logged.
func loadOrMintBridgeToken(cfg *Config) (token, path string, err error) {
	path = bridgeTokenPath(cfg)
	if b, rerr := os.ReadFile(path); rerr == nil {
		t := strings.TrimSpace(string(b))
		if len(t) >= 32 {
			return t, path, nil
		}
		return "", path, fmt.Errorf("token file %s is too short; delete it to re-mint", path)
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return "", path, fmt.Errorf("read token file: %w", rerr)
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", path, fmt.Errorf("mint token: %w", err)
	}
	token = hex.EncodeToString(raw)
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", path, err
	}
	if err = os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", path, fmt.Errorf("write token file: %w", err)
	}
	return token, path, nil
}

// newTokenGuard requires `Authorization: Bearer <token>` on every route
// except /healthcheck, which stays open so supervisors can probe liveness.
func newTokenGuard(next http.Handler, token string) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		const p = "Bearer "
		if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) &&
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[len(p):])), want) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, errorResponse{
			Error:   "unauthorized",
			Details: "this bridge requires Authorization: Bearer <token>; the token is in the bridge token file next to the database",
		})
	})
}
