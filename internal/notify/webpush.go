// Web Push delivery (RFC 8291 payload encryption, RFC 8292 VAPID JWTs) lives
// in this file. notify.go generates and persists the VAPID keypair and
// stores subscriptions; this file sends to them.
package notify

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
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
