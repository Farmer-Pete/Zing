package console_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
)

// wantMermaidSHA256 is the digest static/ASSETS.md records for the vendored
// mermaid 11.4.1 bundle (design section 0, dependency set: "An asset-verify
// test asserts the embedded bytes match the recorded digest").
const wantMermaidSHA256 = "cff34e82c8bded4711ae36bc9cf1df0f1d05fa0594cb6540eb8dc3d2aced9426"

// testContentTypeJS and testContentTypeJSON name the two Content-Type
// values the static assets serve with, reused across this file and
// console_test.go so goconst has one literal to point at, not several.
const (
	testContentTypeJS   = "text/javascript"
	testContentTypeJSON = "application/json"
)

// TestStaticAssetsServeWithContentType is the render-and-serve smoke test
// for the five public static assets the allowlist in
// internal/console/server.go serves (design section 5, 12).
func TestStaticAssetsServeWithContentType(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	tests := []struct {
		path        string
		contentType string
	}{
		{"/static/datastar.js", testContentTypeJS},
		{"/static/mermaid.js", testContentTypeJS},
		{"/static/console.js", testContentTypeJS},
		{"/static/keyboard.mjs", testContentTypeJS},
		{"/static/keys.json", testContentTypeJSON},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path) //nolint:noctx // a bare GET on a test server needs no deadline
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", tt.path, resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != tt.contentType {
				t.Errorf("GET %s Content-Type = %q, want %q", tt.path, ct, tt.contentType)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("GET %s: read body: %v", tt.path, err)
			}
			if len(body) == 0 {
				t.Errorf("GET %s returned an empty body", tt.path)
			}
		})
	}
}

// TestStaticAssetsRejectForbiddenPaths proves the static mux is an explicit
// allowlist, not a directory server: console.test.js, package.json, and
// ASSETS.md all live in internal/console/static/ for repo-side use (Node
// tooling, license bookkeeping) but must never be servable (design section
// 3, 5, 12).
func TestStaticAssetsRejectForbiddenPaths(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	forbidden := []string{
		"/static/console.test.js",
		"/static/package.json",
		"/static/ASSETS.md",
	}

	for _, path := range forbidden {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path) //nolint:noctx // a bare GET on a test server needs no deadline
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want 404", path, resp.StatusCode)
			}
		})
	}
}

// TestMermaidAssetDigestMatchesRecorded proves the mermaid.js bytes actually
// embedded and served match the SHA-256 digest recorded in
// internal/console/static/ASSETS.md, so a future accidental re-vendor (or a
// hand-edit of the vendored file) is caught rather than silently served
// (design section 0, dependency set).
func TestMermaidAssetDigestMatchesRecorded(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/mermaid.js") //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET /static/mermaid.js: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != wantMermaidSHA256 {
		t.Errorf("mermaid.js sha256 = %s, want %s (internal/console/static/ASSETS.md)", got, wantMermaidSHA256)
	}
}
