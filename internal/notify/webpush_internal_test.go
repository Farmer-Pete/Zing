package notify

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// decryptForTest implements the receiver half of RFC 8291 section 3.4: given
// the aes128gcm message body encrypt produces, the receiver's own private
// key, and the auth secret the subscription was created with, it recovers
// the original plaintext payload. It exists only to prove encrypt against a
// real receiver, inside the same test file that exercises it.
func decryptForTest(message []byte, uaPriv *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if len(message) < 21 {
		return nil, fmt.Errorf("message too short: %d bytes", len(message))
	}
	salt := message[0:16]
	_ = binary.BigEndian.Uint32(message[16:20]) // record size, unused here
	keyIDLen := int(message[20])
	if len(message) < 21+keyIDLen {
		return nil, errors.New("message too short for key id")
	}
	asPublicBytes := message[21 : 21+keyIDLen]
	ciphertext := message[21+keyIDLen:]

	asPublic, err := ecdh.P256().NewPublicKey(asPublicBytes)
	if err != nil {
		return nil, fmt.Errorf("parse as_public: %w", err)
	}

	ecdhSecret, err := uaPriv.ECDH(asPublic)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}

	uaPublic := uaPriv.PublicKey().Bytes()
	keyInfo := make([]byte, 0, len("WebPush: info")+1+len(uaPublic)+len(asPublicBytes))
	keyInfo = append(keyInfo, "WebPush: info"...)
	keyInfo = append(keyInfo, 0x00)
	keyInfo = append(keyInfo, uaPublic...)
	keyInfo = append(keyInfo, asPublicBytes...)

	ikm, err := hkdf.Key(sha256.New, ecdhSecret, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, fmt.Errorf("derive ikm: %w", err)
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("derive cek: %w", err)
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("derive nonce: %w", err)
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("new aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	padded, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("gcm open: %w", err)
	}
	if len(padded) == 0 || padded[len(padded)-1] != 0x02 {
		return nil, errors.New("missing delimiter byte")
	}
	return padded[:len(padded)-1], nil
}

func b64url(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestEncrypt_RFC8291AppendixA(t *testing.T) {
	plaintext := []byte("When I grow up, I want to be a watermelon")
	asPrivateB := b64url(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	asPublicB := b64url(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	uaPublicB := b64url(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	authB := b64url(t, "BTBZMqHH6r4Tts7J_aSIgg")
	saltB := b64url(t, "DGv6ra1nlYgDCS1FRnbzlw")
	wantBody := b64url(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")

	asPriv, err := ecdh.P256().NewPrivateKey(asPrivateB)
	if err != nil {
		t.Fatalf("new as_private: %v", err)
	}
	if got := asPriv.PublicKey().Bytes(); !bytes.Equal(got, asPublicB) {
		t.Fatalf("as_private does not match as_public: got %x want %x", got, asPublicB)
	}

	body, err := encrypt(plaintext, uaPublicB, authB, saltB, asPriv)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("encrypt body mismatch:\n got: %s\nwant: %s",
			base64.RawURLEncoding.EncodeToString(body),
			base64.RawURLEncoding.EncodeToString(wantBody))
	}
}

func TestEncrypt_RoundTripsThroughReceiver(t *testing.T) {
	uaPriv, err := ecdh.P256().GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ua key: %v", err)
	}
	asPriv, err := ecdh.P256().GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate as key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err = rand.Read(auth); err != nil {
		t.Fatalf("random auth: %v", err)
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		t.Fatalf("random salt: %v", err)
	}

	payload := []byte(`{"title":"hello"}`)
	body, err := encrypt(payload, uaPriv.PublicKey().Bytes(), auth, salt, asPriv)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if len(body) < 86 {
		t.Fatalf("body shorter than the 86-byte header: %d", len(body))
	}
	recordSizeGot := binary.BigEndian.Uint32(body[16:20])
	if recordSizeGot != 4096 {
		t.Fatalf("record size = %d, want 4096", recordSizeGot)
	}
	keyLen := body[20]
	if keyLen != 65 {
		t.Fatalf("key length = %d, want 65", keyLen)
	}

	got, err := decryptForTest(body, uaPriv, auth)
	if err != nil {
		t.Fatalf("decryptForTest: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %q want %q", got, payload)
	}

	// decryptForTest must also decrypt the appendix A expected body with
	// ua_private and return the watermelon plaintext.
	uaPrivateB := b64url(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	authApp := b64url(t, "BTBZMqHH6r4Tts7J_aSIgg")
	appendixBody := b64url(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")
	uaPrivForAppendix, err := ecdh.P256().NewPrivateKey(uaPrivateB)
	if err != nil {
		t.Fatalf("new ua_private: %v", err)
	}
	gotAppendix, err := decryptForTest(appendixBody, uaPrivForAppendix, authApp)
	if err != nil {
		t.Fatalf("decryptForTest(appendix): %v", err)
	}
	if !bytes.Equal(gotAppendix, []byte("When I grow up, I want to be a watermelon")) {
		t.Fatalf("decryptForTest(appendix) = %q", gotAppendix)
	}
}

func TestVAPIDJWT_SignsES256WithClaims(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate vapid key: %v", err)
	}
	const sub = "mailto:owner@example.com"
	now := time.Date(2026, 10, 7, 15, 4, 5, 0, time.UTC)

	tok, err := vapidJWT(priv, "https://push.example/subscription/abc", sub, now)
	if err != nil {
		t.Fatalf("vapidJWT: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3: %q", len(parts), tok)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims vapidJWTClaims
	if err = json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Aud != "https://push.example" {
		t.Errorf("aud = %q, want https://push.example", claims.Aud)
	}
	if claims.Sub != sub {
		t.Errorf("sub = %q, want %q", claims.Sub, sub)
	}
	wantExp := now.Add(12 * time.Hour).Unix()
	if claims.Exp != wantExp {
		t.Errorf("exp = %d, want %d", claims.Exp, wantExp)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, s) {
		t.Fatal("signature does not verify")
	}

	if _, err := vapidJWT(priv, "http://%zz", sub, now); err == nil || err.Error() != "invalid endpoint" {
		t.Fatalf("vapidJWT(bad endpoint) error = %v, want \"invalid endpoint\"", err)
	}
}
