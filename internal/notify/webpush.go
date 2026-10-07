// Web Push delivery (RFC 8291 payload encryption, RFC 8292 VAPID JWTs) lives
// in this file. notify.go generates and persists the VAPID keypair and
// stores subscriptions; this file sends to them.
package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zing/internal/store"
)

// maxPayloadBytes is the largest plaintext encrypt accepts: the aes128gcm
// record size (4096) minus the 86-byte fixed header, the 16-byte GCM tag,
// and the one padding-delimiter byte encrypt appends (RFC 8188 section 2,
// RFC 8291 section 4).
const maxPayloadBytes = 4096 - 86 - 16 - 1

// recordSize is the aes128gcm record size RFC 8291 section 4 uses for a
// single-record push message.
const recordSize = 4096

// uncompressedP256PointLen is the byte length of an uncompressed P-256
// point (0x04 || X || Y): 1 + 32 + 32. as_public always has this length,
// which is also the "key id length" byte RFC 8291 section 4 writes into
// the message header.
const uncompressedP256PointLen = 65

// encrypt implements the sender half of RFC 8291 (Web Push message
// encryption) using the aes128gcm content encoding of RFC 8188. uaPublic is
// the subscription's p256dh (an uncompressed P-256 point), authSecret is its
// 16-byte auth secret, salt is a fresh 16-byte random value, and asPriv is a
// fresh ephemeral P-256 key pair generated for this message. It returns the
// aes128gcm message body: salt || record size || key id length || as_public
// || ciphertext.
func encrypt(payload, uaPublic, authSecret, salt []byte, asPriv *ecdh.PrivateKey) ([]byte, error) {
	if len(payload) > maxPayloadBytes {
		return nil, fmt.Errorf("notify: payload is %d bytes, at most %d", len(payload), maxPayloadBytes)
	}
	if len(salt) != 16 {
		return nil, fmt.Errorf("notify: salt is %d bytes, want 16", len(salt))
	}

	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("notify: parse subscriber key: %w", err)
	}

	ecdhSecret, err := asPriv.ECDH(uaKey)
	if err != nil {
		return nil, fmt.Errorf("notify: compute ecdh secret: %w", err)
	}

	asPublic := asPriv.PublicKey().Bytes()

	keyInfo := make([]byte, 0, len("WebPush: info")+1+len(uaPublic)+len(asPublic))
	keyInfo = append(keyInfo, "WebPush: info"...)
	keyInfo = append(keyInfo, 0x00)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublic...)

	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, fmt.Errorf("notify: derive ikm: %w", err)
	}

	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("notify: derive cek: %w", err)
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("notify: derive nonce: %w", err)
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("notify: new aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("notify: new gcm: %w", err)
	}

	plaintext := make([]byte, 0, len(payload)+1)
	plaintext = append(plaintext, payload...)
	plaintext = append(plaintext, 0x02)
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	body := make([]byte, 0, 16+4+1+len(asPublic)+len(ciphertext))
	body = append(body, salt...)
	var recordSizeBytes [4]byte
	binary.BigEndian.PutUint32(recordSizeBytes[:], recordSize)
	body = append(body, recordSizeBytes[:]...)
	body = append(body, uncompressedP256PointLen)
	body = append(body, asPublic...)
	body = append(body, ciphertext...)

	return body, nil
}

// vapidExpiry is how far in the future vapidJWT sets the exp claim (RFC 8292
// recommends at most 24 hours).
const vapidExpiry = 12 * time.Hour

// vapidJWTHeader is the fixed VAPID JWT header (RFC 8292 section 2): ES256
// over a compact JWT.
type vapidJWTHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
}

// vapidJWTClaims is the VAPID JWT claim set (RFC 8292 section 2). Sub
// identifies the sender to the push service, as a mailto: or https: URL.
type vapidJWTClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}

// vapidJWT builds and signs a compact VAPID JWT (RFC 8292) for a POST to
// endpoint, with sub as the contact claim. now is the signing time; exp is
// set to now plus vapidExpiry. It returns the error "invalid endpoint"
// without wrapping when endpoint fails to parse or lacks a scheme or host,
// since a wrapped url.Parse error would quote endpoint.
func vapidJWT(priv *ecdsa.PrivateKey, endpoint, sub string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	hasOrigin := err == nil && u.Scheme != "" && u.Host != ""
	if !hasOrigin {
		return "", errors.New("invalid endpoint")
	}
	aud := u.Scheme + "://" + u.Host

	headerJSON, err := json.Marshal(vapidJWTHeader{Typ: "JWT", Alg: "ES256"})
	if err != nil {
		return "", fmt.Errorf("notify: marshal vapid jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(vapidJWTClaims{Aud: aud, Exp: now.Add(vapidExpiry).Unix(), Sub: sub})
	if err != nil {
		return "", fmt.Errorf("notify: marshal vapid jwt claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))

	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", fmt.Errorf("notify: sign vapid jwt: %w", err)
	}

	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// subscriptionKeys is the keys_json shape a browser's subscription stores:
// p256dh is its 65-byte uncompressed P-256 point and auth is its 16-byte
// auth secret, both base64url without padding (RFC 8291).
type subscriptionKeys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// decodeB64URL decodes a browser-supplied base64url value that may carry a
// trailing padding character, which base64.RawURLEncoding rejects.
func decodeB64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Send encrypts payload once per stored subscription and POSTs it, with a
// VAPID JWT, to each one (RFC 8291, RFC 8292). It rejects an oversize
// payload before touching the store. An empty subscription list is a no-op:
// it returns nil without ever reading the VAPID keypair. Otherwise it reads
// (generating on first use) the VAPID keypair once and calls sendOne for
// every subscription, continuing after a per-subscription failure. It
// returns errors.Join of every per-subscription error, each prefixed with
// "subscription ID:", and nil when every send (including a 404 or 410
// cleanup) succeeded.
func (w *WebPush) Send(ctx context.Context, payload []byte) error {
	if len(payload) > maxPayloadBytes {
		return fmt.Errorf("notify: payload is %d bytes, at most %d", len(payload), maxPayloadBytes)
	}

	subs, err := w.store.ListPushSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("notify: list push subscriptions: %w", err)
	}
	if len(subs) == 0 {
		slog.DebugContext(ctx, "notify: no push subscriptions")
		return nil
	}

	pubB64, err := w.PublicKey(ctx)
	if err != nil {
		return fmt.Errorf("notify: public key: %w", err)
	}
	privB64, ok, err := w.store.GetSetting(ctx, settingVAPIDPrivate)
	if err != nil {
		return fmt.Errorf("notify: get %s: %w", settingVAPIDPrivate, err)
	}
	if !ok || privB64 == "" {
		return fmt.Errorf("notify: %s missing", settingVAPIDPrivate)
	}
	privBytes, err := base64.RawURLEncoding.DecodeString(privB64)
	if err != nil {
		return fmt.Errorf("notify: decode %s: %w", settingVAPIDPrivate, err)
	}
	priv, err := x509.ParseECPrivateKey(privBytes)
	if err != nil {
		return fmt.Errorf("notify: parse %s: %w", settingVAPIDPrivate, err)
	}

	var errs []error
	for _, sub := range subs {
		if sendErr := w.sendOne(ctx, sub, payload, priv, pubB64); sendErr != nil {
			errs = append(errs, fmt.Errorf("subscription %d: %w", sub.ID, sendErr))
		}
	}
	return errors.Join(errs...)
}

// sendOne encrypts payload for one subscription, signs a VAPID JWT for its
// endpoint, and POSTs the result. A 2xx answer, including the 404/410 case
// handled below, is success. A 404 or 410 answer deletes the subscription
// (it is gone from the push service) and still counts as success. Every
// other error is a fixed string that never quotes the endpoint: "invalid
// keys", "invalid endpoint", "request failed" (with the inner
// context.DeadlineExceeded or context.Canceled only), or "push service
// answered STATUS".
func (w *WebPush) sendOne(ctx context.Context, sub store.PushSubscription, payload []byte, priv *ecdsa.PrivateKey, pubB64 string) error {
	var keys subscriptionKeys
	if err := json.Unmarshal(sub.KeysJSON, &keys); err != nil {
		return errors.New("invalid keys")
	}
	uaPublic, err := decodeB64URL(keys.P256dh)
	if err != nil || len(uaPublic) != uncompressedP256PointLen {
		return errors.New("invalid keys")
	}
	if _, pubErr := ecdh.P256().NewPublicKey(uaPublic); pubErr != nil {
		return errors.New("invalid keys")
	}
	authSecret, err := decodeB64URL(keys.Auth)
	if err != nil || len(authSecret) != 16 {
		return errors.New("invalid keys")
	}

	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("notify: generate ephemeral key: %w", err)
	}
	salt := make([]byte, 16)
	if _, saltErr := rand.Read(salt); saltErr != nil {
		return fmt.Errorf("notify: random salt: %w", saltErr)
	}

	body, err := encrypt(payload, uaPublic, authSecret, salt, asPriv)
	if err != nil {
		return err
	}

	jwt, err := vapidJWT(priv, sub.Endpoint, w.contact, w.now())
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid endpoint")
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+pubB64)

	resp, err := w.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("request failed: %w", context.DeadlineExceeded)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("request failed: %w", context.Canceled)
		}
		return errors.New("request failed")
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	if status >= 200 && status < 300 {
		slog.DebugContext(ctx, "notify: push delivered", "subscription_id", sub.ID, "status", status)
		return nil
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		if delErr := w.store.DeletePushSubscription(ctx, sub.Endpoint); delErr != nil {
			return fmt.Errorf("notify: delete expired subscription: %w", delErr)
		}
		slog.InfoContext(ctx, "notify: removed expired push subscription", "subscription_id", sub.ID, "status", status)
		return nil
	}
	return fmt.Errorf("push service answered %d", status)
}
