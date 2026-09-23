// mw.go is the console's mutation middleware (design section 6.14): the
// same-origin plus Host-allowlist guard every state-changing route sits
// behind. It extends Package 3's requireSameOrigin (server.go, Task 3) --
// which checked only Sec-Fetch-Site and the Datastar-Request header -- into
// the fuller guard section 6.14 specifies, in one place rather than a
// second guard layered on top.
package console

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// datastarRequestHeader is the header every Datastar backend action sends
// (datastar skill, attributes.md: "All backend actions send a
// Datastar-Request: true header"). A plain cross-site form POST cannot set a
// custom header without triggering a CORS preflight, so requiring it here
// rules out that attack shape even when Origin is absent or spoofed by
// something other than a browser.
const datastarRequestHeader = "Datastar-Request"

// formContentTypes are the three MIME types a plain HTML <form> can submit
// without any JavaScript (the CORS "simple request" content types). A
// mutation route rejects all three: nothing this console serves ever POSTs
// a form.
var formContentTypes = map[string]bool{
	"application/x-www-form-urlencoded": true,
	"multipart/form-data":               true,
	"text/plain":                        true,
}

// mutationGuard is the same-origin plus Host-allowlist middleware (design
// section 6.14): every request to a route it wraps must carry the
// Datastar-Request header, a non-form Content-Type, a Host in allowedHosts
// whose effective port is port, and an Origin (or, failing that, Referer)
// authority that matches the request's own scheme, host, and effective
// port.
type mutationGuard struct {
	port         int
	allowedHosts map[string]bool // canonical "host:port" authorities, lowercased, no trailing dot
}

// newMutationGuard builds a guard whose Host allowlist is every entry in
// hosts (bare hostnames, no port), each canonicalized at port. For this
// task the caller passes the console's one bound host plus localhost and
// 127.0.0.1 (design section 6.14, Task 7 scope note); Task 11 widens the
// source to every resolved bind authority plus Console.AllowedHosts, still
// through this same constructor.
func newMutationGuard(port int, hosts ...string) *mutationGuard {
	g := &mutationGuard{port: port, allowedHosts: make(map[string]bool, len(hosts))}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		g.allowedHosts[canonicalAuthority(h, port)] = true
	}
	return g
}

// canonicalAuthority lowercases host, strips one trailing dot (a
// fully-qualified DNS name), and joins it with port, the one normal form
// every Host and Origin comparison in this file reduces to (design section
// 6.14: "lowercase the host, strip a trailing dot, and rebuild the
// authority for the compare").
func canonicalAuthority(host string, port int) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// requireSameOrigin wraps next with the guard (design section 6.14): the
// Datastar-Request header, a non-form Content-Type, a Host in the
// allowlist at the right port, and an Origin (or Referer) authority that
// matches. Every rejection is 403, fail-closed, before next ever runs.
func (g *mutationGuard) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(datastarRequestHeader) != "true" {
			forbidden(w)
			return
		}
		if isFormContentType(r.Header.Get("Content-Type")) {
			forbidden(w)
			return
		}

		hostAuth, ok := g.checkHost(r)
		if !ok {
			forbidden(w)
			return
		}
		if !g.originMatches(r, hostAuth) {
			forbidden(w)
			return
		}

		next(w, r)
	}
}

// forbidden writes the one 403 body every rejection in this file shares.
func forbidden(w http.ResponseWriter) {
	http.Error(w, "cross-site request rejected", http.StatusForbidden)
}

// requestScheme reports the scheme this request arrived over: "https" when
// TLS terminated inside this process, "http" otherwise. The console never
// terminates TLS itself (loopback and tailnet, no login), so this is "http"
// in practice, but the check stays scheme-aware rather than hardcoded, per
// design section 6.14's own "effective port ... scheme default 80/443"
// language.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// defaultPort is the scheme default effective-port computation falls back
// to when an authority carries no explicit port (design section 6.14).
func defaultPort(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

// splitAuthority parses authority ("host", "host:port", or a bracketed
// IPv6 "[::1]:port") into its host and effective port: the explicit port
// when present, else scheme's default. It is used for both the request
// Host header and an Origin or Referer's authority, so the two are computed
// the same way (design section 6.14).
func splitAuthority(authority, scheme string) (host string, port int, ok bool) {
	if authority == "" {
		return "", 0, false
	}
	// A bare authority (no scheme) parses cleanly as a scheme-relative URL:
	// "//host:port". This also handles a bracketed IPv6 host without any
	// special-casing here.
	u, err := url.Parse("//" + authority)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", 0, false
	}
	host = u.Hostname()
	if p := u.Port(); p != "" {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 1 || n > 65535 {
			return "", 0, false
		}
		return host, n, true
	}
	return host, defaultPort(scheme), true
}

// checkHost validates the request's Host header: it must parse, its
// effective port must equal g.port, and its canonicalized authority must be
// in the allowlist. It returns that canonical authority so originMatches
// can compare the Origin against exactly what Host resolved to.
func (g *mutationGuard) checkHost(r *http.Request) (authority string, ok bool) {
	host, port, ok := splitAuthority(r.Host, requestScheme(r))
	if !ok || port != g.port {
		return "", false
	}
	auth := canonicalAuthority(host, port)
	if !g.allowedHosts[auth] {
		return "", false
	}
	return auth, true
}

// originMatches validates the Origin header (or, when Origin is absent, the
// Referer's authority) against hostAuth, the request's own canonicalized
// Host authority (design section 6.14). It fails closed -- returns false --
// on every shape the design names: an absent Origin with no usable Referer,
// a malformed Origin or Referer, an opaque "null" Origin, more than one
// Origin header, a scheme or effective port that does not match, or a host
// that does not match.
func (g *mutationGuard) originMatches(r *http.Request, hostAuth string) bool {
	values := r.Header.Values("Origin")

	var scheme, authority string
	switch len(values) {
	case 0:
		ref := r.Header.Get("Referer")
		if ref == "" {
			return false
		}
		u, err := url.Parse(ref)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return false
		}
		scheme, authority = u.Scheme, u.Host
	case 1:
		origin := values[0]
		if origin == "" || origin == "null" {
			return false
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return false
		}
		scheme, authority = u.Scheme, u.Host
	default:
		return false // duplicated Origin header: fail closed
	}

	if scheme != requestScheme(r) {
		return false
	}
	host, port, ok := splitAuthority(authority, scheme)
	if !ok || port != g.port {
		return false
	}
	return canonicalAuthority(host, port) == hostAuth
}

// isFormContentType reports whether ct names one of the three MIME types a
// plain HTML form can submit without JavaScript (formContentTypes above). A
// malformed Content-Type is not classified as a form here; the
// Datastar-Request and origin checks are what actually guard these routes.
func isFormContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return formContentTypes[mt]
}
