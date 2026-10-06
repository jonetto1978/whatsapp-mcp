package main

import (
	"net/url"
	"testing"
)

func TestNativeStoreDSNAbsolutePathsAndReadOnlyOptions(t *testing.T) {
	for _, tc := range []struct{ name, path, wantPath string }{
		{"POSIX", "/tmp/ChatStorage.sqlite", "/tmp/ChatStorage.sqlite"},
		{"Windows drive", "C:/Users/Test User/ChatStorage.sqlite", "/C:/Users/Test User/ChatStorage.sqlite"},
		{"Windows UNC", "//server/share/ChatStorage.sqlite", "//server/share/ChatStorage.sqlite"},
		{"escaped filename", "/tmp/Chat %# ? José.sqlite", "/tmp/Chat %# ? José.sqlite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := url.Parse(nativeStoreDSN(tc.path))
			if err != nil {
				t.Fatal(err)
			}
			if got.Scheme != "file" || got.Host != "" || got.Path != tc.wantPath {
				t.Fatalf("native URI = %q, want file with empty authority and path %q", got.String(), tc.wantPath)
			}
			if got.Fragment != "" || got.Query().Get("mode") != "ro" || got.Query().Get("_query_only") != "1" || got.Query().Get("_busy_timeout") != "3000" {
				t.Fatalf("read-only URI options lost: %s", got)
			}
		})
	}
}
