package console_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/response"
)

// assetVersionPattern is what console.AssetVersion() must always match:
// the first 12 hex characters of a sha256 (design section 5, 12; #59's
// Q2 decision).
var assetVersionPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// cacheControlImmutable and cacheControlNoCache are the two Cache-Control
// values the static mux and GET / choose between (#59, Q2 decision):
// immutable for a URL whose ?v= matches console.AssetVersion(), no-cache
// for every other static request and for GET /.
const (
	cacheControlImmutable = "public, max-age=31536000, immutable"
	cacheControlNoCache   = "no-cache"
)

// wantMermaidSHA256 is the digest static/ASSETS.md records for the vendored
// mermaid 11.4.1 UMD bundle (design section 0, dependency set: "An
// asset-verify test asserts the embedded bytes match the recorded digest";
// v10 change log: the self-contained UMD build replaces the chunked ESM
// entry Task 1 vendored).
const wantMermaidSHA256 = "a43bc1afd446f9c4cc66ac5dd45d02e8d65e26fc5344ec0ef787f88d6ddb6f9e"

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
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken, response.SeverityMinor, "", nil, "", nil))
	// t.Cleanup, not defer: this test's subtests below call t.Parallel(),
	// which pauses them until this function returns, so a deferred
	// srv.Close() would close the server before any subtest's GET runs.
	t.Cleanup(srv.Close)

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
			t.Parallel()
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
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken, response.SeverityMinor, "", nil, "", nil))
	// t.Cleanup, not defer: this test's subtests below call t.Parallel(),
	// which pauses them until this function returns, so a deferred
	// srv.Close() would close the server before any subtest's GET runs.
	t.Cleanup(srv.Close)

	forbidden := []string{
		"/static/console.test.js",
		"/static/package.json",
		"/static/ASSETS.md",
	}

	for _, path := range forbidden {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
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

// TestStaticCacheControl proves the reconnect plan's cache rule (#59): a
// static asset requested with ?v= equal to this build's AssetVersion
// answers immutable, since only then do the served bytes match the URL;
// every other static request -- no v at all, or another build's v --
// answers no-cache with the current bytes, so a stale tab's reload never
// gets a response the browser is allowed to keep past that reload.
func TestStaticCacheControl(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken, response.SeverityMinor, "", nil, "", nil))
	t.Cleanup(srv.Close)

	paths := []string{
		"/static/datastar.js",
		"/static/mermaid.js",
		"/static/console.js",
		"/static/keyboard.mjs",
		"/static/keys.json",
	}

	for _, path := range paths {
		t.Run(path+"/versioned", func(t *testing.T) {
			t.Parallel()
			checkStaticCacheControlVersioned(t, srv.URL, path)
		})
		t.Run(path+"/unversioned", func(t *testing.T) {
			t.Parallel()
			checkStaticCacheControlUnversioned(t, srv.URL, path)
		})
		t.Run(path+"/stale_version", func(t *testing.T) {
			t.Parallel()
			checkStaticCacheControlStaleVersion(t, srv.URL, path)
		})
	}
}

// checkStaticCacheControlVersioned asserts that path requested with this
// build's ?v= answers 200, a non-empty body and the immutable Cache-Control.
func checkStaticCacheControlVersioned(t *testing.T, base, path string) {
	t.Helper()
	resp, err := http.Get(base + path + "?v=" + console.AssetVersion()) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s?v=...: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?v=... status = %d, want 200", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s?v=...: read body: %v", path, err)
	}
	if len(body) == 0 {
		t.Errorf("GET %s?v=... returned an empty body", path)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != cacheControlImmutable {
		t.Errorf("GET %s?v=... Cache-Control = %q, want %q", path, cc, cacheControlImmutable)
	}
}

// checkStaticCacheControlUnversioned asserts that the bare path answers 200
// and the no-cache Cache-Control.
func checkStaticCacheControlUnversioned(t *testing.T, base, path string) {
	t.Helper()
	resp, err := http.Get(base + path) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != cacheControlNoCache {
		t.Errorf("GET %s Cache-Control = %q, want %q", path, cc, cacheControlNoCache)
	}
}

// checkStaticCacheControlStaleVersion asserts that path requested with
// another build's ?v= answers 200, the same body as the unversioned
// request, and the no-cache Cache-Control.
func checkStaticCacheControlStaleVersion(t *testing.T, base, path string) {
	t.Helper()
	unversioned, err := http.Get(base + path) //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = unversioned.Body.Close() }()
	wantBody, err := io.ReadAll(unversioned.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}

	resp, err := http.Get(base + path + "?v=000000000000") //nolint:noctx // a bare GET on a test server needs no deadline
	if err != nil {
		t.Fatalf("GET %s?v=000000000000: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?v=000000000000 status = %d, want 200", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s?v=000000000000: read body: %v", path, err)
	}
	if !bytes.Equal(body, wantBody) {
		t.Errorf("GET %s?v=000000000000 body differs from the unversioned body", path)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != cacheControlNoCache {
		t.Errorf("GET %s?v=000000000000 Cache-Control = %q, want %q", path, cc, cacheControlNoCache)
	}
}

// TestMermaidAssetDigestMatchesRecorded proves the mermaid.js bytes actually
// embedded and served match the SHA-256 digest recorded in
// internal/console/static/ASSETS.md, so a future accidental re-vendor (or a
// hand-edit of the vendored file) is caught rather than silently served
// (design section 0, dependency set).
func TestMermaidAssetDigestMatchesRecorded(t *testing.T) {
	t.Parallel()
	s := newConsoleTestStore(t)
	srv := httptest.NewServer(console.New(s, bus.New(), nil, testBindHosts, testConsolePort, newTestLogHandler(t), nil, testPushToken, response.SeverityMinor, "", nil, "", nil))
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
