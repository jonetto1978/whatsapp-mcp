package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestEscapeLikeNeutralisesWildcards(t *testing.T) {
	cases := map[string]string{
		`abc`:        `abc`,
		`100%`:       `100\%`,
		`a_b`:        `a\_b`,
		`back\slash`: `back\\slash`,
		`%_%`:        `\%\_\%`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

// The real handler, the real SQL. The earlier search test mirrored the WHERE
// clause by hand and so passed while the shipped ESCAPE literal was two
// characters long and every search on the live bridge returned nothing.
func TestSearchContactsHandlerEscapesWildcards(t *testing.T) {
	db := newContactTestDB(t)
	seed := func(jid, name string) {
		t.Helper()
		insertChat(t, db, jid, "direct", "")
		if _, err := db.Exec(`
			INSERT INTO contacts (jid, push_name, normalized_name, is_business, created_at, updated_at)
			VALUES (?, ?, ?, 0, 0, 0)
		`, jid, name, Normalize(name)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	seed("15555550101@s.whatsapp.net", "Manuel Diana")
	seed("15555550102@s.whatsapp.net", "100% Juan")
	seed("15555550103@s.whatsapp.net", "Agustín Ruiz")

	s := &Server{db: db}
	search := func(q string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/contacts/search?q="+url.QueryEscape(q), nil)
		rr := httptest.NewRecorder()
		s.handleSearchContacts(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("q=%q: HTTP %d: %s", q, rr.Code, rr.Body.String())
		}
		var out contactListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("q=%q: decode: %v", q, err)
		}
		return out.Count
	}
	if n := search("Manuel"); n != 1 {
		t.Errorf("plain name search returned %d, want 1", n)
	}
	if n := search("agustin"); n != 1 {
		t.Errorf("accent-insensitive search returned %d, want 1", n)
	}
	if n := search("%"); n != 1 {
		t.Errorf("q=%% must match only the literal-percent contact, got %d", n)
	}
	if n := search("_"); n != 0 {
		t.Errorf("q=_ must not act as a single-char wildcard, got %d", n)
	}
	if n := search("100% J"); n != 1 {
		t.Errorf("literal percent inside a name returned %d, want 1", n)
	}
}
