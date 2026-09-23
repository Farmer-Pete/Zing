// push.go: GET /push/key and POST /push/subscribe (design section 6.13),
// both guarded by the bearer token in Authorization, compared in constant
// time, on top of the same mutation guard (mw.go) every other
// state-changing route sits behind. internal/console owns the small
// PushKeys interface it consumes here; internal/notify's WebPush type
// (design section 6.1) is the concrete implementation cmd/zing wires in.
package console

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"

	"zing/internal/notify"
)

// PushKeys is the console's own seam onto VAPID key access and subscription
// storage (design section 6.13): "The console defines the small interface
// it needs, console.PushKeys, and internal/notify provides a concrete
// webpush type that satisfies it." Declared here, not in internal/notify,
// so internal/notify stays a leaf the console depends on, not the reverse.
type PushKeys interface {
	PublicKey(ctx context.Context) (string, error)
	Subscribe(ctx context.Context, sub notify.Subscription) error
}

// maxPushSubscribeBodyBytes bounds POST /push/subscribe's body. A real
// subscription is a short JSON object (an endpoint URL plus two short
// base64url keys), so this is generous headroom, not a tight fit.
const maxPushSubscribeBodyBytes = 8 << 10 // 8 KiB

// checkPushToken compares the Authorization header's bearer token against
// c.pushToken in constant time (design section 6.13, 9: "compared in
// constant time"), writing 401 and returning false on any mismatch,
// including a missing or malformed header. c.pushToken is resolved once at
// startup by cmd/zing/serve.go per the section 6.13 precedence rule: an
// explicit console.push_token always wins, else the token persisted in
// settings.push_token from an earlier run (or generated and persisted on
// the very first run) is reused.
func (c *console) checkPushToken(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if len(got) < len(prefix) || got[:len(prefix)] != prefix {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	got = got[len(prefix):]

	// subtle.ConstantTimeCompare requires equal-length inputs to avoid a
	// length-derived timing signal; comparing the SHA-nothing-needed simple
	// byte-length check above is itself constant relative to the secret (it
	// only depends on the untrusted request, not c.pushToken), so this does
	// not reopen a timing side channel on the token itself.
	if len(got) != len(c.pushToken) || subtle.ConstantTimeCompare([]byte(got), []byte(c.pushToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// pushKeyResponse is GET /push/key's body (design section 7.1: "the VAPID
// public key").
type pushKeyResponse struct {
	Key string `json:"key"`
}

// handlePushKey is GET /push/key (design section 6.13, 7.1): the VAPID
// public key, guarded by the bearer token only (no origin guard: it is a
// read, and a phone hits it from outside the tab).
func (c *console) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if !c.checkPushToken(w, r) {
		return
	}

	key, err := c.push.PublicKey(r.Context())
	if err != nil {
		slog.Error("console: get vapid public key", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentTypeJSON)
	if err := json.NewEncoder(w).Encode(pushKeyResponse{Key: key}); err != nil {
		slog.Error("console: write push key response", "err", err)
	}
}

// pushSubscribeRequest is POST /push/subscribe's body: a Web Push
// subscription, decoded strictly (design section 6.13: "decodes the full
// request strictly, bounds its size, and checks the endpoint is an https
// URL").
type pushSubscribeRequest struct {
	Endpoint string            `json:"endpoint"`
	Keys     map[string]string `json:"keys"`
}

// handlePushSubscribe is POST /push/subscribe (design section 6.13, 7.1):
// guarded by both the mutation guard (mw.go, wired in server.go) and the
// bearer token. It decodes strictly, bounds the body, rejects a non-https
// endpoint with 400, then calls PushKeys.Subscribe, which validates
// keys_json against the push_subscriptions/keys schema; a validation
// failure there also reports 400, since it is the same "bad body" class as
// an invalid endpoint (design section 7.1: "204, 401, or 400"). 204 and a
// bus publish on success.
func (c *console) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !c.checkPushToken(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPushSubscribeBodyBytes)

	var req pushSubscribeRequest
	if err := decodeStrict(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if !isHTTPSURL(req.Endpoint) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := c.push.Subscribe(r.Context(), notify.Subscription{Endpoint: req.Endpoint, Keys: req.Keys}); err != nil {
		// Every failure here is a bad subscription payload (keys_json failed
		// schema validation) rather than a genuine server fault, since the
		// endpoint itself was already checked above and Subscribe's only
		// other work is a validated store write; report it the same way
		// writeDecodeError reports a malformed body.
		slog.Warn("console: subscribe push", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}

// isHTTPSURL reports whether s parses as an absolute URL with scheme
// "https" and a non-empty host (design section 6.13: "checks the endpoint
// is an https URL").
func isHTTPSURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == schemeHTTPS && u.Host != ""
}
