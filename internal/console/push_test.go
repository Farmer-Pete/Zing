package console_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/notify"
)

// testPushToken2 is the bearer token push_test.go's own server uses, kept
// distinct from testPushToken (console_test.go) so a copy-paste that used
// the wrong constant would still fail loudly rather than coincidentally pass.
const testPushToken2 = "task11-push-secret"

// newPushTestServer builds a console.New handler wired with a real
// notify.WebPush (design section 6.13's concrete PushKeys) over a fresh
// store, started on a reserved listener so its real port is known up front
// (mirroring mw_test.go's newMutationTestServer, which this file cannot
// reuse directly because it hardcodes push=nil for the tests that never
// touch push.go).
func newPushTestServer(t *testing.T) (srv *httptest.Server) {
	t.Helper()

	s := newConsoleTestStore(t)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", testBindHost+":0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	port := addr.Port

	push := notify.New(s)
	handler := console.New(s, bus.New(), nil, []string{testBindHost}, port, newTestLogHandler(t), push, testPushToken2)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// pushRequest builds a request to srv.URL+path carrying the given bearer
// token (empty means no Authorization header at all) and, for a POST, a
// Content-Type and Origin (fix 5: POST /push/subscribe dropped the
// mutation guard, so neither header is required any more, but sending them
// stays harmless and keeps this helper's shape close to mutationRequest's).
// TestPushSubscribe_SucceedsWithoutSameOriginHeaders below proves the guard
// is actually gone by omitting them entirely.
func pushRequest(t *testing.T, srv *httptest.Server, method, path, token, body string) *http.Request {
	t.Helper()
	var r *strings.Reader
	if body == "" {
		r = strings.NewReader("")
	} else {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, path, err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Datastar-Request", "true")
		req.Header.Set("Origin", srv.URL)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// newPushTestServerWithToken is newPushTestServer but with the console's own
// configured pushToken set explicitly, so a test can exercise an edge case
// (an empty configured token) newPushTestServer's fixed testPushToken2
// cannot.
func newPushTestServerWithToken(t *testing.T, pushToken string) (srv *httptest.Server) {
	t.Helper()

	s := newConsoleTestStore(t)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", testBindHost+":0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	port := addr.Port

	push := notify.New(s)
	handler := console.New(s, bus.New(), nil, []string{testBindHost}, port, newTestLogHandler(t), push, pushToken)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestPushKey_EmptyConfiguredTokenAlwaysRejects proves checkPushToken's
// constant-time rewrite (review fix, package 4 re-review) still rejects an
// empty presented token when the console's own configured pushToken is also
// empty, rather than a sha256("") == sha256("") digest collision letting it
// through. It builds the request by hand, not through pushRequest, so the
// Authorization header is present as "Bearer " with a genuinely empty token
// value rather than omitted outright -- the digest-collision case the
// prefix check alone would not exercise.
func TestPushKey_EmptyConfiguredTokenAlwaysRejects(t *testing.T) {
	srv := newPushTestServerWithToken(t, "")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/push/key", http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer ")

	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /push/key with an empty presented token against an empty configured token: status = %d, want 401", resp.StatusCode)
	}
}

// TestPushKey_RequiresBearerToken proves GET /push/key rejects a missing or
// wrong token with 401 and returns the VAPID public key with 200 once the
// right token is presented (design section 6.13, 7.1, 9).
func TestPushKey_RequiresBearerToken(t *testing.T) {
	srv := newPushTestServer(t)

	noAuth := doRequest(t, pushRequest(t, srv, http.MethodGet, "/push/key", "", ""))
	defer func() { _ = noAuth.Body.Close() }()
	if noAuth.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /push/key with no token: status = %d, want 401", noAuth.StatusCode)
	}

	wrong := doRequest(t, pushRequest(t, srv, http.MethodGet, "/push/key", "wrong-token", ""))
	defer func() { _ = wrong.Body.Close() }()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /push/key with a wrong token: status = %d, want 401", wrong.StatusCode)
	}

	ok := doRequest(t, pushRequest(t, srv, http.MethodGet, "/push/key", testPushToken2, ""))
	defer func() { _ = ok.Body.Close() }()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("GET /push/key with the right token: status = %d, want 200", ok.StatusCode)
	}
	var got struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET /push/key response: %v", err)
	}
	if got.Key == "" {
		t.Error("GET /push/key returned an empty key")
	}
}

// TestPushKey_IsStableAcrossCalls proves the VAPID keypair generates once:
// two GET /push/key calls against the same server return the same key.
func TestPushKey_IsStableAcrossCalls(t *testing.T) {
	srv := newPushTestServer(t)

	first := doRequest(t, pushRequest(t, srv, http.MethodGet, "/push/key", testPushToken2, ""))
	defer func() { _ = first.Body.Close() }()
	var firstKey struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(first.Body).Decode(&firstKey); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	second := doRequest(t, pushRequest(t, srv, http.MethodGet, "/push/key", testPushToken2, ""))
	defer func() { _ = second.Body.Close() }()
	var secondKey struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(second.Body).Decode(&secondKey); err != nil {
		t.Fatalf("decode second response: %v", err)
	}

	if firstKey.Key != secondKey.Key {
		t.Errorf("GET /push/key returned %q then %q, want the same key both times", firstKey.Key, secondKey.Key)
	}
}

// TestPushSubscribe_RequiresBearerToken proves POST /push/subscribe also
// rejects a missing token with 401, even though the request otherwise
// carries every header the mutation guard requires.
func TestPushSubscribe_RequiresBearerToken(t *testing.T) {
	srv := newPushTestServer(t)

	body := `{"endpoint":"https://push.example/abc","keys":{"p256dh":"a-key","auth":"a-secret"}}`
	resp := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", "", body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /push/subscribe with no token: status = %d, want 401", resp.StatusCode)
	}
}

// TestPushSubscribe_RejectsNonHTTPSEndpoint proves the endpoint scheme check
// design section 6.13 requires the handler (not the keys_json schema) to
// make: a non-https endpoint is rejected with 400 and writes nothing.
func TestPushSubscribe_RejectsNonHTTPSEndpoint(t *testing.T) {
	srv := newPushTestServer(t)

	body := `{"endpoint":"http://push.example/abc","keys":{"p256dh":"a-key","auth":"a-secret"}}`
	resp := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /push/subscribe with a non-https endpoint: status = %d, want 400", resp.StatusCode)
	}
}

// TestPushSubscribe_RejectsBadBody proves a malformed JSON body is rejected
// with 400.
func TestPushSubscribe_RejectsBadBody(t *testing.T) {
	srv := newPushTestServer(t)

	resp := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, `{not json`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /push/subscribe with a malformed body: status = %d, want 400", resp.StatusCode)
	}
}

// TestPushSubscribe_RejectsKeysMissingRequiredFields proves keys_json
// schema validation (missing p256dh or auth) surfaces as 400 through the
// handler, matching design section 7.1's "204, 401, or 400" for this route.
func TestPushSubscribe_RejectsKeysMissingRequiredFields(t *testing.T) {
	srv := newPushTestServer(t)

	body := `{"endpoint":"https://push.example/incomplete","keys":{"p256dh":"a-key"}}`
	resp := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /push/subscribe missing auth: status = %d, want 400", resp.StatusCode)
	}
}

// TestPushSubscribe_SucceedsWithoutSameOriginHeaders proves POST
// /push/subscribe is token-only (design section 6.13, fix 5), symmetric
// with GET /push/key: a request presenting none of the mutation guard's
// headers (no Datastar-Request, no Origin, and a Host outside the
// allowlist) still succeeds once it carries the right bearer token. A
// phone subscribing is authenticated by that token, not by browser
// same-origin, and a phone's MagicDNS host need not be in allowed_hosts.
func TestPushSubscribe_SucceedsWithoutSameOriginHeaders(t *testing.T) {
	srv := newPushTestServer(t)

	body := `{"endpoint":"https://push.example/no-guard","keys":{"p256dh":"a-key","auth":"a-secret"}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/push/subscribe", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testPushToken2)
	req.Host = "phone.example.ts.net" // deliberately outside the mutation guard's Host allowlist

	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /push/subscribe with no same-origin headers and an out-of-allowlist Host: status = %d, want 204", resp.StatusCode)
	}
}

// TestPushSubscribe_SucceedsAndIsIdempotentOnReSubscribe proves the happy
// path (204) and that subscribing the same endpoint twice is idempotent
// (design section 6.13: "It replaces by endpoint, so a re-subscribe is
// idempotent"), both calls returning 204 rather than the second conflicting.
func TestPushSubscribe_SucceedsAndIsIdempotentOnReSubscribe(t *testing.T) {
	srv := newPushTestServer(t)

	body := `{"endpoint":"https://push.example/repeat","keys":{"p256dh":"a-key","auth":"a-secret"}}`

	first := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, body))
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /push/subscribe (first): status = %d, want 204", first.StatusCode)
	}

	second := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, body))
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /push/subscribe (re-subscribe): status = %d, want 204", second.StatusCode)
	}
}

// TestPushSubscribe_RejectsKeysWithExtraProperty proves
// validSubscriptionKeys' additionalProperties:false half of the
// push_subscriptions/keys schema (review fix, PR #16): a Keys map carrying
// an unexpected third property is rejected with 400, the same as a missing
// required one (TestPushSubscribe_RejectsKeysMissingRequiredFields above).
func TestPushSubscribe_RejectsKeysWithExtraProperty(t *testing.T) {
	srv := newPushTestServer(t)

	body := `{"endpoint":"https://push.example/extra","keys":{"p256dh":"a-key","auth":"a-secret","extra":"nope"}}`
	resp := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /push/subscribe with an extra keys property: status = %d, want 400", resp.StatusCode)
	}
}

// TestPushSubscribe_StoreFailureReturns500NotBadRequest proves
// handlePushSubscribe's fixed class split (review fix, PR #16): once the
// endpoint and Keys shape both pass their own checks, a genuine store
// failure -- closing the store out from under a live server, the same
// technique TestIndexReturns500WithGenericBodyOnStoreError (console_test.go)
// uses for GET / -- reports 500 with the generic body, not 400. Before this
// fix, handlePushSubscribe treated every Subscribe error as a bad payload
// and answered 400 even here.
func TestPushSubscribe_StoreFailureReturns500NotBadRequest(t *testing.T) {
	s := newConsoleTestStore(t)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", testBindHost+":0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}

	push := notify.New(s)
	handler := console.New(s, bus.New(), nil, []string{testBindHost}, addr.Port, newTestLogHandler(t), push, testPushToken2)
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	body := `{"endpoint":"https://push.example/store-failure","keys":{"p256dh":"a-key","auth":"a-secret"}}`
	resp := doRequest(t, pushRequest(t, srv, http.MethodPost, "/push/subscribe", testPushToken2, body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("POST /push/subscribe after closing the store: status = %d, want 500", resp.StatusCode)
	}

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read response body: %v", readErr)
	}
	if got := strings.TrimSpace(string(respBody)); got != "internal error" {
		t.Errorf(`response body = %q, want exactly "internal error"`, got)
	}
}
