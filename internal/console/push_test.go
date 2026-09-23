package console_test

import (
	"encoding/json"
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
// token (empty means no Authorization header at all) and, for a POST, the
// same same-origin headers mutationRequest (mw_test.go) sends, since
// POST /push/subscribe sits behind both the mutation guard and the token
// check (design section 7.1: "origin + token").
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
