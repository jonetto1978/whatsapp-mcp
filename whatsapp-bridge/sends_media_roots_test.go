package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSendFileRootsPlatformListAndConfinement(t *testing.T) {
	base := t.TempDir()
	roots := []string{filepath.Join(base, "allowed one"), filepath.Join(base, "allowed two")}
	for _, root := range roots {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WHATSAPP_SEND_FILE_ROOTS", strings.Join(roots, string(os.PathListSeparator)))
	if got := sendFileRoots(); !reflect.DeepEqual(got, roots) {
		t.Fatalf("roots = %q, want %q", got, roots)
	}
	for _, root := range roots {
		path := filepath.Join(root, "fixture.png")
		if err := os.WriteFile(path, tinyPNG, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := confineToSendRoots(path); err != nil {
			t.Fatalf("declared root rejected: %v", err)
		}
	}
	// A sibling with the same prefix must not become an allowed root.
	sibling := roots[0] + " outside"
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(sibling, "fixture.png")
	if err := os.WriteFile(outside, tinyPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := confineToSendRoots(outside); err == nil {
		t.Fatal("file outside the declared roots accepted")
	}
}
