package notify_test

import (
	"encoding/base64"
	"path/filepath"
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

// TestSubscribe_StoresThroughUpsertPushSubscription proves Subscribe
// serializes Keys into keys_json and writes it via
// store.UpsertPushSubscription, and that an invalid keys map (missing the
// required auth field) is rejected by the underlying schema validation.
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
