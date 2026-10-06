package main

import (
	"os"
	"runtime"
	"testing"
)

// The writer still requests 0600 on every platform. POSIX must enforce that
// exact privacy mode; Windows FileMode exposes only its read-only attribute,
// not ACL privacy. Verify the supported writable bit there rather than
// asserting POSIX mode bits that Windows never reports.
func assertMediaFilePermissions(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if st.Mode().Perm()&0o200 == 0 {
			t.Fatalf("media file unexpectedly read-only: mode %o", st.Mode().Perm())
		}
		return
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", st.Mode().Perm())
	}
}
