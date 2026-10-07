// Web Push delivery (RFC 8291 payload encryption, RFC 8292 VAPID JWTs) lives
// in this file. notify.go generates and persists the VAPID keypair and
// stores subscriptions; this file sends to them.
package notify

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
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

	if len(asPublic) != uncompressedP256PointLen {
		return nil, fmt.Errorf("notify: as_public is %d bytes, want %d", len(asPublic), uncompressedP256PointLen)
	}

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
	if err != nil || u.Scheme == "" || u.Host == "" {
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
