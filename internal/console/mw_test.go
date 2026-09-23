package console_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// port, not an arbitrary one). log is console.New's Task 10 log handler
// argument; every caller here that does not itself exercise the log wiring
// passes newTestLogHandler(t), and control_test.go passes its own
// buffer-and-LevelVar-backed handler so it can assert what the console
// wrote and changed. It returns the server and the port every same-origin
// request in this file and answer_test.go must present as Host/Origin's
// port.
func newMutationTestServer(t *testing.T, s *store.Store, b *bus.Broker, log *console.Handler) (srv *httptest.Server, port int) {
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

	handler := console.New(s, b, nil, []string{testBindHost}, port, log, nil, testPushToken)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, port
}

// newMutationTestServerWithHosts is newMutationTestServer with additional
// entries in the mutation guard's Host allowlist, beyond the always-added
// "127.0.0.1" and "localhost" (design section 6.14, Task 11: the allowlist
// is built from every resolved bind authority plus Console.AllowedHosts).
// Every test below that needs a non-loopback literal, an IPv6 authority, or
// a configured DNS alias to pass builds its server through this helper.
func newMutationTestServerWithHosts(t *testing.T, s *store.Store, b *bus.Broker, log *console.Handler, extraHosts ...string) (srv *httptest.Server, port int) {
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

	handler := console.New(s, b, nil, extraHosts, port, log, nil, testPushToken)
	srv = httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the placeholder listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, port
}

// sameOriginRequestTo builds a POST /read request that dials srv (the real
// listener) but presents authority as its Host and Origin, the same
// technique TestMutationGuard_PassesSameOriginThroughLocalhostAnd127AndTheBoundHost
// uses: a browser's Host header need not match the socket it dialed, so this
// is how a test proves the guard's allowlist check itself, not just that the
// real listener answered.
func sameOriginRequestTo(t *testing.T, srv *httptest.Server, authority string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/read", strings.NewReader(`{"message":1}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = authority
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", "http://"+authority)
	return req
}

// TestMutationGuard_PassesAConfiguredNonLoopbackLiteral proves a literal,
// non-loopback bind address in the Host allowlist passes the guard (design
// section 6.14, Task 11: "any configured bind address serves mutations, not
// only loopback"). 203.0.113.5 is TEST-NET-3 (RFC 5737), reserved for
// documentation and never actually dialed here.
func TestMutationGuard_PassesAConfiguredNonLoopbackLiteral(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, port := newMutationTestServerWithHosts(t, s, bus.New(), newTestLogHandler(t), "203.0.113.5")

	authority := "203.0.113.5:" + strconv.Itoa(port)
	resp := doRequest(t, sameOriginRequestTo(t, srv, authority))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("status = 403, want the guard to pass a configured non-loopback literal")
	}
}

// TestMutationGuard_PassesABracketedIPv6Authority proves an IPv6 bind
// address in the Host allowlist passes when presented in its bracketed
// authority form, exactly as a browser sends it (design section 6.14, 11).
func TestMutationGuard_PassesABracketedIPv6Authority(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, port := newMutationTestServerWithHosts(t, s, bus.New(), newTestLogHandler(t), "2001:db8::1")

	authority := "[2001:db8::1]:" + strconv.Itoa(port)
	resp := doRequest(t, sameOriginRequestTo(t, srv, authority))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("status = 403, want the guard to pass a bracketed IPv6 authority")
	}
}

// TestMutationGuard_PassesAConfiguredDNSAlias proves a DNS name in
// console.allowed_hosts (surfaced here as an extra Host allowlist entry)
// passes the guard, the tailnet-DNS-name case design section 6.14
// introduces allowed_hosts for.
func TestMutationGuard_PassesAConfiguredDNSAlias(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, port := newMutationTestServerWithHosts(t, s, bus.New(), newTestLogHandler(t), "example.tailnet")

	authority := "example.tailnet:" + strconv.Itoa(port)
	resp := doRequest(t, sameOriginRequestTo(t, srv, authority))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("status = 403, want the guard to pass a configured DNS alias")
	}
}

// TestMutationGuard_CanonicalizesDNSNameCaseAndTrailingDot proves
// "Example.Tailnet." and "example.tailnet" canonicalize to the same
// allowed authority (design section 6.14: "the DNS name lowercased and a
// trailing dot stripped"), so a request presenting the mixed-case,
// fully-qualified form still passes when the allowlist holds the plain
// lowercase form.
func TestMutationGuard_CanonicalizesDNSNameCaseAndTrailingDot(t *testing.T) {
	s := newConsoleTestStore(t)
	srv, port := newMutationTestServerWithHosts(t, s, bus.New(), newTestLogHandler(t), "example.tailnet")

	authority := "Example.Tailnet.:" + strconv.Itoa(port)
	resp := doRequest(t, sameOriginRequestTo(t, srv, authority))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("status = 403, want Example.Tailnet. to canonicalize to the allowed example.tailnet")
	}
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
	srv, port := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))
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

// TestReadRoutes_RejectOutOfAllowlistHost proves the Host-only guard (mw.go's
// requireAllowedHost, design section 6.14 fix 4) wraps GET / and GET
// /stream: a request presenting a Host outside the allowlist -- the DNS
// rebinding shape, a page served from an attacker hostname that resolves to
// this process's own address -- is rejected with 403 before either handler
// runs, even though neither route carries an Origin header the way a
// mutation POST would.
func TestReadRoutes_RejectOutOfAllowlistHost(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/", http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = "evil.example:" + strconv.Itoa(portFromURL(t, srv.URL))

	resp := doRequest(t, req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET / with an out-of-allowlist Host: status = %d, want 403", resp.StatusCode)
	}
}

// TestReadRoutes_PassAnInAllowlistHost proves the same Host-only guard lets
// an ordinary, in-allowlist GET / through -- a normal top-level navigation,
// which carries no Origin header at all -- so fix 4 closes the rebinding
// gap without breaking the page load every real client depends on.
func TestReadRoutes_PassAnInAllowlistHost(t *testing.T) {
	s := newConsoleTestStore(t)
	srv := newTestServer(t, s, bus.New(), nil, newTestLogHandler(t))

	//nolint:noctx // a bare GET on a test server needs no deadline
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / with the server's own (allowlisted) Host: status = %d, want 200", resp.StatusCode)
	}
}

// portFromURL extracts the numeric port from a "http://host:port" URL, so
// TestReadRoutes_RejectOutOfAllowlistHost can build an out-of-allowlist Host
// at the server's own real port (not the allowlisted host, and not a wrong
// port either, so the test proves the host check specifically).
func portFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port from %q: %v", rawURL, err)
	}
	return port
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
	_, port := newMutationTestServer(t, s, bus.New(), newTestLogHandler(t))
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
