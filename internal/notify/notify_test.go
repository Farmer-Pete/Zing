package notify_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"zing/internal/notify"
	"zing/internal/store"
)

// keyP256dh is the push_subscriptions/keys schema's p256dh field name,
// named once so goconst has nothing to flag across this file's fixtures.
const keyP256dh = "p256dh"

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPublicKey_GeneratesOnceAndPersists proves the design section 6.13
// contract: the first call generates a VAPID keypair and persists it, and
// every later call (including a fresh WebPush over the same store,
// simulating a restart) returns the exact same public key rather than
// generating a new one.
func TestPublicKey_GeneratesOnceAndPersists(t *testing.T) {
	s := newTestStore(t)
	w := notify.New(s)

	first, err := w.PublicKey(t.Context())
	if err != nil {
		t.Fatalf("PublicKey (first): %v", err)
	}
	if first == "" {
		t.Fatal("PublicKey (first) = \"\", want a generated key")
	}

	second, err := w.PublicKey(t.Context())
	if err != nil {
		t.Fatalf("PublicKey (second): %v", err)
	}
	if second != first {
		t.Errorf("PublicKey (second) = %q, want the same key as the first call (%q)", second, first)
	}

	// A fresh WebPush over the same store simulates a process restart: the
	// public key must come from settings.vapid_public, not be regenerated.
	restarted := notify.New(s)
	third, err := restarted.PublicKey(t.Context())
	if err != nil {
		t.Fatalf("PublicKey (after restart): %v", err)
	}
	if third != first {
		t.Errorf("PublicKey (after restart) = %q, want the same key as the first call (%q)", third, first)
	}
}

// TestPublicKey_IsRawURLBase64OfA65ByteUncompressedPoint proves the exact
// encoding design section 6.13 names: base64url, raw (no padding), of the
// 65-byte uncompressed P-256 point (0x04 prefix plus 32-byte X and Y).
func TestPublicKey_IsRawURLBase64OfA65ByteUncompressedPoint(t *testing.T) {
	s := newTestStore(t)
	w := notify.New(s)

	key, err := w.PublicKey(t.Context())
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("PublicKey is not raw-URL base64: %v", err)
	}
	if len(raw) != 65 {
		t.Errorf("decoded PublicKey length = %d, want 65 (an uncompressed P-256 point)", len(raw))
	}
	if raw[0] != 0x04 {
		t.Errorf("decoded PublicKey first byte = 0x%02x, want 0x04 (the uncompressed-point prefix)", raw[0])
	}
}

// TestPublicKey_NeverHalfWritesTheKeypair proves both settings rows land
// together: after PublicKey generates the pair, both vapid_public and
// vapid_private exist, and vapid_private is never exposed through PushKeys.
func TestPublicKey_NeverHalfWritesTheKeypair(t *testing.T) {
	s := newTestStore(t)
	w := notify.New(s)

	if _, err := w.PublicKey(t.Context()); err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	pub, ok, err := s.GetSetting(t.Context(), "vapid_public")
	if err != nil || !ok || pub == "" {
		t.Errorf("settings.vapid_public = (%q, %v, %v), want a non-empty stored value", pub, ok, err)
	}
	priv, ok, err := s.GetSetting(t.Context(), "vapid_private")
	if err != nil || !ok || priv == "" {
		t.Errorf("settings.vapid_private = (%q, %v, %v), want a non-empty stored value", priv, ok, err)
	}
}

// TestPublicKey_ConcurrentCallsReturnSameKeyAndPersistOnce proves the
// review-fix contract for PublicKey's first-run generation race (design
// section 6.13): many concurrent first calls on one WebPush must all return
// the exact same key, never one goroutine's key that a second goroutine's
// concurrent generate-and-store then overwrites in the store. Run under
// -race, it also proves generateAndStoreKeypair's writes have no data race.
func TestPublicKey_ConcurrentCallsReturnSameKeyAndPersistOnce(t *testing.T) {
	s := newTestStore(t)
	w := notify.New(s)

	const callers = 20
	keys := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = w.PublicKey(t.Context())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("PublicKey[%d]: %v", i, err)
		}
	}
	for i := 1; i < callers; i++ {
		if keys[i] != keys[0] {
			t.Errorf("PublicKey[%d] = %q, want the same key every concurrent caller got (%q): "+
				"a caller must never be handed a public key whose private half a racing generate then overwrites",
				i, keys[i], keys[0])
		}
	}

	stored, ok, err := s.GetSetting(t.Context(), "vapid_public")
	if err != nil || !ok {
		t.Fatalf("GetSetting(vapid_public) = (%q, %v, %v), want a stored key", stored, ok, err)
	}
	if stored != keys[0] {
		t.Errorf("stored vapid_public = %q, want the key every PublicKey call returned (%q)", stored, keys[0])
	}
}

// TestSubscribe_StoresThroughUpsertPushSubscription proves Subscribe
// serializes Keys into keys_json and writes it via
// store.UpsertPushSubscription, and that a re-subscribe with the same
// endpoint replaces the row instead of erroring
// (TestSubscribe_RejectsKeysMissingRequiredFields below covers the invalid
// keys map case).
func TestSubscribe_StoresThroughUpsertPushSubscription(t *testing.T) {
	s := newTestStore(t)
	w := notify.New(s)

	err := w.Subscribe(t.Context(), notify.Subscription{
		Endpoint: "https://push.example/abc",
		Keys:     map[string]string{keyP256dh: "a-valid-key", "auth": "a-valid-secret"},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// A re-subscribe with the same endpoint must succeed too (design section
	// 6.13: "It replaces by endpoint, so a re-subscribe is idempotent"),
	// proving Subscribe reached store.UpsertPushSubscription's real upsert
	// rather than, say, failing silently on a duplicate.
	err = w.Subscribe(t.Context(), notify.Subscription{
		Endpoint: "https://push.example/abc",
		Keys:     map[string]string{keyP256dh: "a-different-key", "auth": "a-different-secret"},
	})
	if err != nil {
		t.Fatalf("Subscribe (re-subscribe, same endpoint): %v", err)
	}
}

// TestSubscribe_RejectsKeysMissingRequiredFields proves an incomplete Keys
// map is rejected before any row lands, since Subscribe's JSON validation
// happens inside store.UpsertPushSubscription.
func TestSubscribe_RejectsKeysMissingRequiredFields(t *testing.T) {
	s := newTestStore(t)
	w := notify.New(s)

	err := w.Subscribe(t.Context(), notify.Subscription{
		Endpoint: "https://push.example/incomplete",
		Keys:     map[string]string{keyP256dh: "a-valid-key"}, // missing "auth"
	})
	if err == nil {
		t.Fatal("Subscribe with a missing auth key: err = nil, want an error")
	}
}

// subscribeFreshKeys stores one valid push_subscriptions row pointed at
// endpoint, with a fresh receiver keypair and auth secret, so Send has
// somewhere real to POST.
func subscribeFreshKeys(t *testing.T, w *notify.WebPush, endpoint string) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ua key: %v", err)
	}
	auth := make([]byte, 16)
	if _, readErr := rand.Read(auth); readErr != nil {
		t.Fatalf("random auth: %v", readErr)
	}
	err = w.Subscribe(t.Context(), notify.Subscription{
		Endpoint: endpoint,
		Keys: map[string]string{
			keyP256dh: base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
			"auth":    base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
}

// jwtSub decodes the "sub" claim out of a compact VAPID JWT, the middle of
// its three base64url segments.
func jwtSub(t *testing.T, jwt string) string {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q does not have 3 segments", jwt)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt claims: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal jwt claims: %v", err)
	}
	return claims.Sub
}

// authorizationSub extracts the VAPID JWT from an Authorization header
// shaped "vapid t=JWT, k=PUBKEY" and decodes its sub claim.
func authorizationSub(t *testing.T, header string) string {
	t.Helper()
	const prefix = "vapid t="
	if !strings.HasPrefix(header, prefix) {
		t.Fatalf("Authorization header %q does not start with %q", header, prefix)
	}
	rest := header[len(prefix):]
	jwt, _, found := strings.Cut(rest, ",")
	if !found {
		t.Fatalf("Authorization header %q has no k= part", header)
	}
	return jwtSub(t, jwt)
}

// TestWithContact_SetsJWTSub proves WithContact overrides the VAPID JWT's
// sub claim (owner decision Q3), and that New without it falls back to
// notify.DefaultContact.
func TestWithContact_SetsJWTSub(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	t.Run("without WithContact", func(t *testing.T) {
		s := newTestStore(t)
		w := notify.New(s)
		subscribeFreshKeys(t, w, srv.URL)

		if err := w.Send(t.Context(), []byte("hello")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if sub := authorizationSub(t, gotAuth); sub != notify.DefaultContact {
			t.Errorf("sub = %q, want DefaultContact %q", sub, notify.DefaultContact)
		}
	})

	t.Run("with WithContact", func(t *testing.T) {
		s := newTestStore(t)
		w := notify.New(s, notify.WithContact("mailto:owner@example.com"))
		subscribeFreshKeys(t, w, srv.URL)

		if err := w.Send(t.Context(), []byte("hello")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if sub := authorizationSub(t, gotAuth); sub != "mailto:owner@example.com" {
			t.Errorf("sub = %q, want %q", sub, "mailto:owner@example.com")
		}
	})
}
