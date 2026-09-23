// push.go: GET /push/key and POST /push/subscribe (design section 6.13),
// both guarded by the bearer token in Authorization, compared in constant
// time, on top of the same mutation guard (mw.go) every other
// state-changing route sits behind. internal/console owns the small
// PushKeys interface it consumes here; internal/notify's WebPush type
// (design section 6.1) is the concrete implementation cmd/zing wires in.
package console

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"unicode/utf8"

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
//
// The comparison itself hashes both sides to a fixed 32-byte sha256 digest
// before calling subtle.ConstantTimeCompare (review fix, package 4
// re-review): comparing got and c.pushToken directly first checked
// len(got) != len(c.pushToken), a mismatch that short-circuits before
// ConstantTimeCompare runs and so leaks, via timing, whether a guessed
// token's length matches the real one. Hashing first fixes both inputs to
// the same 32-byte length, so no length branch is ever needed.
func (c *console) checkPushToken(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if len(got) < len(prefix) || got[:len(prefix)] != prefix {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	got = got[len(prefix):]

	gotSum := sha256.Sum256([]byte(got))
	wantSum := sha256.Sum256([]byte(c.pushToken))
	// c.pushToken == "" is a configuration check, not a secret comparison
	// (an empty configured token has nothing to leak the timing of), so
	// short-circuiting on it ahead of the constant-time digest compare below
	// reopens no side channel; it only makes sure an unset token can never
	// authenticate, digest collision or not.
	if c.pushToken == "" || subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) != 1 {
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
// guarded by the bearer token only, matching GET /push/key (fix 5: the
// mutation guard was dropped so a phone outside the Host allowlist can still
// subscribe). It decodes strictly, bounds the body, rejects a non-https
// endpoint or a malformed Keys map with 400, then calls PushKeys.Subscribe.
// 204 and a bus publish on success.
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
	if !validSubscriptionKeys(req.Keys) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Every failure Subscribe can still return past the two checks above is
	// a genuine store fault, not a bad payload (review fix, PR #16):
	// isHTTPSURL already rejected a bad endpoint and validSubscriptionKeys
	// already rejected a Keys map that would fail
	// UpsertPushSubscription's own push_subscriptions/keys schema check, so
	// this package no longer needs to guess which failure class a Subscribe
	// error belongs to the way the old blanket "every failure is 400" comment
	// here used to.
	if err := c.push.Subscribe(r.Context(), notify.Subscription{Endpoint: req.Endpoint, Keys: req.Keys}); err != nil {
		slog.Error("console: subscribe push", "err", err)
		http.Error(w, genericServerErrorBody, http.StatusInternalServerError)
		return
	}

	c.bus.Publish()
	w.WriteHeader(http.StatusNoContent)
}

// pushSubscriptionKeyMaxRunes is push_subscriptions/keys.json's maxLength
// for both "p256dh" and "auth" (internal/store/schemas/push_subscriptions/
// keys.json), counted in runes to match the JSON Schema string-length rule
// (design section 6.13; the same rune-counting reasoning as
// config.minPushTokenLen).
const pushSubscriptionKeyMaxRunes = 512

// validSubscriptionKeys reports whether keys is exactly the shape
// push_subscriptions/keys.json requires: the two properties "p256dh" and
// "auth", both present, non-empty, and at most 512 runes, no others (review
// fix, PR #16). internal/console never imports the store's schema
// validator (PushKeys is this package's only seam onto internal/notify), so
// this reproduces that one small schema locally, the same way isHTTPSURL
// reproduces "an https URL" rather than reaching into another package for
// it; keep it in sync with keys.json if that schema ever changes.
func validSubscriptionKeys(keys map[string]string) bool {
	if len(keys) != 2 {
		return false
	}
	for _, key := range [2]string{"p256dh", "auth"} {
		v, ok := keys[key]
		if !ok {
			return false
		}
		n := utf8.RuneCountInString(v)
		if n == 0 || n > pushSubscriptionKeyMaxRunes {
			return false
		}
	}
	return true
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
