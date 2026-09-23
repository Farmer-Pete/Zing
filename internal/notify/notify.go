// Package notify implements the console's VAPID keygen and Web Push
// subscription storage (design section 6.13). It is the seam behind the
// small PushKeys interface internal/console declares for itself
// (PublicKey, Subscribe): internal/console never imports a Web Push
// library or the store's raw settings keys directly, only this package's
// WebPush type and its own PushKeys interface.
//
// Real push delivery (Send) is Package 10's job, not this one's: this
// package only generates and persists the VAPID keypair and stores
// subscriptions, so a later package's Notifier can send to them.
package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"zing/internal/store"
)

// Subscription is one browser Web Push subscription request (design section
// 6.13): the endpoint URL a push message is POSTed to, and the two keys the
// browser generated for it. Endpoint is validated as an https URL by the
// console handler (push.go), not here; Keys is validated against the
// push_subscriptions/keys JSON Schema inside store.UpsertPushSubscription.
type Subscription struct {
	Endpoint string            `json:"endpoint"`
	Keys     map[string]string `json:"keys"`
}

// settingVAPIDPublic and settingVAPIDPrivate are the two settings rows
// WebPush's first PublicKey call writes in one SetSettings transaction
// (design section 6.13: "stores vapid_public and vapid_private in one
// SetSettings transaction, so the pair is never half-written").
const (
	settingVAPIDPublic  = "vapid_public"
	settingVAPIDPrivate = "vapid_private"
)

// WebPush generates a VAPID keypair on first use and stores push
// subscriptions through the store. It satisfies internal/console's PushKeys
// interface (PublicKey, Subscribe); Send is out of scope here (Package 10).
type WebPush struct {
	store *store.Store

	// genMu serializes first-run VAPID keypair generation. Without it, two
	// concurrent first PublicKey calls can both see no stored key, both
	// generate their own keypair, and race to SetSettings: the loser's
	// SetSettings call persists last, so the winner's caller is handed a
	// public key whose private half the loser's write just overwrote. The
	// mutex makes the check-then-generate atomic: only one goroutine ever
	// generates, and every other goroutine re-reads the stored key.
	genMu sync.Mutex
}

// New builds a WebPush backed by st.
func New(st *store.Store) *WebPush {
	return &WebPush{store: st}
}

// PublicKey returns the VAPID public key, base64url raw-URL encoded
// (design section 6.13: "the public key is the base64url raw-url encoding
// of the 65-byte uncompressed point"). It generates and persists a fresh
// VAPID keypair on first use, when no vapid_public setting exists yet.
//
// The first read is lock-free, so the common case (a key already exists)
// never pays for the mutex. Only a miss takes genMu, and re-checks the
// setting once inside it: a caller that lost the race to another goroutine's
// concurrent first call finds the winner's key already stored and returns
// that, rather than generating (and losing) a keypair of its own.
func (w *WebPush) PublicKey(ctx context.Context) (string, error) {
	pub, ok, err := w.store.GetSetting(ctx, settingVAPIDPublic)
	if err != nil {
		return "", fmt.Errorf("notify: get %s: %w", settingVAPIDPublic, err)
	}
	if ok && pub != "" {
		return pub, nil
	}

	w.genMu.Lock()
	defer w.genMu.Unlock()

	pub, ok, err = w.store.GetSetting(ctx, settingVAPIDPublic)
	if err != nil {
		return "", fmt.Errorf("notify: get %s: %w", settingVAPIDPublic, err)
	}
	if ok && pub != "" {
		return pub, nil
	}
	return w.generateAndStoreKeypair(ctx)
}

// generateAndStoreKeypair generates one P-256 VAPID keypair with the
// standard library and stores both halves in a single SetSettings
// transaction, so a crash between the two writes can never happen (design
// section 6.13).
func (w *WebPush) generateAndStoreKeypair(ctx context.Context) (string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("notify: generate vapid keypair: %w", err)
	}

	ecdhPriv, err := priv.ECDH()
	if err != nil {
		return "", fmt.Errorf("notify: convert vapid private key: %w", err)
	}
	// PublicKey().Bytes() is the uncompressed point (0x04 || X || Y), 65
	// bytes for P-256, exactly the shape design section 6.13 names.
	pubBytes := ecdhPriv.PublicKey().Bytes()
	pubB64 := base64.RawURLEncoding.EncodeToString(pubBytes)

	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("notify: marshal vapid private key: %w", err)
	}
	privB64 := base64.RawURLEncoding.EncodeToString(privBytes)

	if err := w.store.SetSettings(ctx, settingVAPIDPublic, pubB64, settingVAPIDPrivate, privB64); err != nil {
		return "", fmt.Errorf("notify: store vapid keypair: %w", err)
	}
	// No key material in the log line (CLAUDE.md: "never log a secret"):
	// not the private key, and not the public key either, since a bare
	// info line proving generation happened needs neither.
	slog.InfoContext(ctx, "vapid keypair generated")
	return pubB64, nil
}

// Subscribe stores sub through store.UpsertPushSubscription (design section
// 6.13), serializing Keys into the keys_json column UpsertPushSubscription
// validates against push_subscriptions/keys.
func (w *WebPush) Subscribe(ctx context.Context, sub Subscription) error {
	keysJSON, err := json.Marshal(sub.Keys)
	if err != nil {
		return fmt.Errorf("notify: marshal subscription keys: %w", err)
	}
	if err := w.store.UpsertPushSubscription(ctx, store.PushSubscription{
		Endpoint: sub.Endpoint, KeysJSON: keysJSON,
	}); err != nil {
		return fmt.Errorf("notify: upsert push subscription: %w", err)
	}
	return nil
}
