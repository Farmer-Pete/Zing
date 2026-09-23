package console_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"zing/internal/bus"
	"zing/internal/console"
	"zing/internal/store"
)

// newMutationTestServer builds a console.New handler and starts it on a
// listener reserved first, so the server's real, OS-assigned port is known
// before the handler is built and can be passed to console.New itself
// (design section 6.14: the mutation guard's Host allowlist is built at
// "the console port", so a test that exercises it needs its own listener's
// port, not an arbitrary one). It returns the server and the port every
// same-origin request in this file and answer_test.go must present as
// Host/Origin's port.
func newMutationTestServer(t *testing.T, s *store.Store, b *bus.Broker) (srv *httptest.Server, port int) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a listener: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", ln.Addr())
	}
	port = addr.Port

	handler := console.New(s, b, "127.0.0.1", port)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, port
}

// mutationRequest builds a POST request to srv.URL+path with a Content-Type
// and every header a same-origin Datastar POST carries: Datastar-Request,
// and an Origin equal to srv.URL. Tests that need a different Host, Origin,
// or header set build their own request instead.
func mutationRequest(t *testing.T, srv *httptest.Server, path, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s request: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", srv.URL)
	return req
}

// doRequest runs req and returns its response, failing the test on a
// transport-level error (not an HTTP error status, which callers assert
// themselves).
func doRequest(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return resp
}

// TestMutationGuard_RejectsCrossOriginAndMalformedRequests drives every
// rejection shape design section 6.14 names against POST /read (chosen
// because it needs no fixture beyond a store), each through its own
// sub-test so one rejected case's side effects (none, here) cannot bleed
// into the next.
func TestMutationGuard_RejectsCrossOriginAndMalformedRequests(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, port := newMutationTestServer(t, s, bus.New())
	authority := "127.0.0.1:" + strconv.Itoa(port)

	tests := []struct {
		name   string
		mutate func(req *http.Request)
	}{
		{
			name: "cross-origin Origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "http://evil.example")
			},
		},
		{
			name: "wrong-scheme Origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "https://"+authority)
			},
		},
		{
			name: "wrong-port Origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "http://127.0.0.1:1")
			},
		},
		{
			name: "same wrong port on both Host and Origin",
			mutate: func(req *http.Request) {
				req.Host = "127.0.0.1:1"
				req.Header.Set("Origin", "http://127.0.0.1:1")
			},
		},
		{
			name: "omitted port resolves to the wrong scheme default",
			mutate: func(req *http.Request) {
				// No explicit port: the origin's effective port defaults to
				// 80 (http), which the test server's real port never is.
				req.Header.Set("Origin", "http://127.0.0.1")
			},
		},
		{
			name: "absent Origin and no Referer",
			mutate: func(req *http.Request) {
				req.Header.Del("Origin")
			},
		},
		{
			name: "malformed Origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "not a url")
			},
		},
		{
			name: "opaque null Origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "null")
			},
		},
		{
			name: "duplicated Origin header",
			mutate: func(req *http.Request) {
				req.Header.Add("Origin", "http://evil.example")
			},
		},
		{
			name: "form content type",
			mutate: func(req *http.Request) {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			},
		},
		{
			name: "missing Datastar-Request header",
			mutate: func(req *http.Request) {
				req.Header.Del("Datastar-Request")
			},
		},
		{
			name: "Host not in the allowlist (rebinding-style request)",
			mutate: func(req *http.Request) {
				req.Host = "evil.example:" + strconv.Itoa(port)
				req.Header.Set("Origin", "http://evil.example:"+strconv.Itoa(port))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mutationRequest(t, srv, "/read", `{"message":1}`)
			tc.mutate(req)
			resp := doRequest(t, req)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// TestMutationGuard_PassesSameOriginThroughLocalhostAnd127AndTheBoundHost
// proves a same-origin Datastar POST passes through localhost, 127.0.0.1,
// and the console's own configured bind host (design section 6.14, Task 7
// scope: "localhost/127.0.0.1 at the console port" plus the bind host
// itself). Each sub-test posts /read for a message that does not exist, so
// a passing request still reaches the store and reports through MarkRead's
// own error path (500) rather than a route-level 403 -- proving the guard,
// not MarkRead, let it through requires seeing anything but 403.
func TestMutationGuard_PassesSameOriginThroughLocalhostAnd127AndTheBoundHost(t *testing.T) {
	s := newConsoleTestStore(t)
	_, port := newMutationTestServer(t, s, bus.New())
	portStr := strconv.Itoa(port)

	hosts := []string{"127.0.0.1", "localhost"}
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			authority := host + ":" + portStr
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+authority+"/read", strings.NewReader(`{"message":1}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Host = authority
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Datastar-Request", "true")
			req.Header.Set("Origin", "http://"+authority)

			resp := doRequest(t, req)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusForbidden {
				t.Errorf("status = 403, want the guard to pass this request through (same-origin, allowlisted host)")
			}
		})
	}
}
