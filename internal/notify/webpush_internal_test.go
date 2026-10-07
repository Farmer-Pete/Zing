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
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zing/internal/store"
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

// newWebPushTestStore opens a fresh on-disk store for a WebPush.Send test.
// It is local to this file (package notify, not notify_test) because Send
// and sendOne are unexported.
func newWebPushTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "zing.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// pushSubscriptionKeysJSON builds the keys_json a browser subscription
// stores: p256dh and auth, both base64url without padding.
func pushSubscriptionKeysJSON(t *testing.T, p256dh, auth []byte) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(p256dh),
		"auth":   base64.RawURLEncoding.EncodeToString(auth),
	})
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	return b
}

// recordedPush is one POST a pushRecorder's handler observed.
type recordedPush struct {
	body    []byte
	headers http.Header
}

// pushRecorder is a fake push service: its handler method answers every
// POST with a fixed status and records the request for later assertions.
type pushRecorder struct {
	mu   sync.Mutex
	reqs []recordedPush
}

func (p *pushRecorder) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			panic(readErr)
		}
		p.mu.Lock()
		p.reqs = append(p.reqs, recordedPush{body: b, headers: r.Header.Clone()})
		p.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (p *pushRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func newPushSubscriberKeys(t *testing.T) (priv *ecdh.PrivateKey, auth []byte) {
	t.Helper()
	var err error
	priv, err = ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ua key: %v", err)
	}
	auth = make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("random auth: %v", err)
	}
	return priv, auth
}

// TestSend_NotFoundOrGoneDeletesSubscription proves that a subscription
// whose endpoint answers 404 or 410 is deleted (and Send still returns
// nil), while a sibling subscription at a different endpoint survives.
func TestSend_NotFoundOrGoneDeletesSubscription(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s := newWebPushTestStore(t)
			w := New(s)

			goneRec := &pushRecorder{}
			goneSrv := httptest.NewServer(goneRec.handler(status))
			defer goneSrv.Close()

			okRec := &pushRecorder{}
			okSrv := httptest.NewServer(okRec.handler(http.StatusCreated))
			defer okSrv.Close()

			gonePriv, goneAuth := newPushSubscriberKeys(t)
			okPriv, okAuth := newPushSubscriberKeys(t)

			if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
				Endpoint: goneSrv.URL, KeysJSON: pushSubscriptionKeysJSON(t, gonePriv.PublicKey().Bytes(), goneAuth),
			}); err != nil {
				t.Fatalf("UpsertPushSubscription (gone): %v", err)
			}
			if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
				Endpoint: okSrv.URL, KeysJSON: pushSubscriptionKeysJSON(t, okPriv.PublicKey().Bytes(), okAuth),
			}); err != nil {
				t.Fatalf("UpsertPushSubscription (ok): %v", err)
			}

			if err := w.Send(t.Context(), []byte(`{"title":"hi"}`)); err != nil {
				t.Fatalf("Send: %v", err)
			}

			subs, err := s.ListPushSubscriptions(t.Context())
			if err != nil {
				t.Fatalf("ListPushSubscriptions: %v", err)
			}
			if len(subs) != 1 {
				t.Fatalf("ListPushSubscriptions: got %d subscriptions, want 1", len(subs))
			}
			if subs[0].Endpoint != okSrv.URL {
				t.Errorf("remaining subscription endpoint = %q, want %q", subs[0].Endpoint, okSrv.URL)
			}
		})
	}
}

// TestSend_ServerErrorKeepsSubscription proves a 500 answer is an error
// that names the status but never the endpoint, and that the subscription
// is not deleted.
func TestSend_ServerErrorKeepsSubscription(t *testing.T) {
	s := newWebPushTestStore(t)
	w := New(s)

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler(http.StatusInternalServerError))
	defer srv.Close()

	priv, auth := newPushSubscriberKeys(t)
	if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
		Endpoint: srv.URL, KeysJSON: pushSubscriptionKeysJSON(t, priv.PublicKey().Bytes(), auth),
	}); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}

	err := w.Send(t.Context(), []byte(`{"title":"hi"}`))
	if err == nil {
		t.Fatal("Send: err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "subscription") || !strings.Contains(err.Error(), "500") {
		t.Errorf("Send error = %q, want it to contain \"subscription\" and \"500\"", err.Error())
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("Send error = %q, must not contain the endpoint URL", err.Error())
	}

	subs, err := s.ListPushSubscriptions(t.Context())
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Errorf("ListPushSubscriptions: got %d subscriptions, want 1 (kept)", len(subs))
	}
}

// TestSend_RedirectIsNotFollowed proves a 3xx answer is treated as an
// ordinary non-2xx status, never followed, and that an endpoint url.Parse
// rejects becomes "invalid endpoint" without quoting it.
func TestSend_RedirectIsNotFollowed(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		s := newWebPushTestStore(t)
		w := New(s)

		rec2 := &pushRecorder{}
		srv2 := httptest.NewServer(rec2.handler(http.StatusCreated))
		defer srv2.Close()

		srv1 := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			http.Redirect(rw, r, srv2.URL, http.StatusTemporaryRedirect)
		}))
		defer srv1.Close()

		priv, auth := newPushSubscriberKeys(t)
		if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
			Endpoint: srv1.URL, KeysJSON: pushSubscriptionKeysJSON(t, priv.PublicKey().Bytes(), auth),
		}); err != nil {
			t.Fatalf("UpsertPushSubscription: %v", err)
		}

		err := w.Send(t.Context(), []byte(`{"title":"hi"}`))
		if err == nil {
			t.Fatal("Send: err = nil, want an error")
		}
		if !strings.Contains(err.Error(), "307") {
			t.Errorf("Send error = %q, want it to contain 307", err.Error())
		}
		if strings.Contains(err.Error(), srv1.URL) || strings.Contains(err.Error(), srv2.URL) {
			t.Errorf("Send error = %q, must not contain either server URL", err.Error())
		}
		if got := rec2.count(); got != 0 {
			t.Errorf("srv2 (redirect target) got %d requests, want 0", got)
		}

		subs, err := s.ListPushSubscriptions(t.Context())
		if err != nil {
			t.Fatalf("ListPushSubscriptions: %v", err)
		}
		if len(subs) != 1 {
			t.Errorf("ListPushSubscriptions: got %d subscriptions, want 1 (kept)", len(subs))
		}
	})

	t.Run("invalid endpoint", func(t *testing.T) {
		s := newWebPushTestStore(t)
		w := New(s)

		priv, auth := newPushSubscriberKeys(t)
		if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
			Endpoint: "http://%zz", KeysJSON: pushSubscriptionKeysJSON(t, priv.PublicKey().Bytes(), auth),
		}); err != nil {
			t.Fatalf("UpsertPushSubscription: %v", err)
		}

		err := w.Send(t.Context(), []byte(`{"title":"hi"}`))
		if err == nil {
			t.Fatal("Send: err = nil, want an error")
		}
		if !strings.Contains(err.Error(), "subscription") || !strings.Contains(err.Error(), "invalid endpoint") {
			t.Errorf("Send error = %q, want \"subscription ID: invalid endpoint\"", err.Error())
		}
		if strings.Contains(err.Error(), "%zz") {
			t.Errorf("Send error = %q, must not contain %%zz", err.Error())
		}
	})
}

// TestSend_NoSubscriptionsSendsNothing proves Send is a no-op, and never
// touches the VAPID keypair, when there is nothing to send to.
func TestSend_NoSubscriptionsSendsNothing(t *testing.T) {
	s := newWebPushTestStore(t)
	w := New(s)

	if err := w.Send(t.Context(), []byte(`{"title":"hi"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, ok, err := s.GetSetting(t.Context(), settingVAPIDPublic); err != nil || ok {
		t.Errorf("GetSetting(%s) = (_, %v, %v), want absent", settingVAPIDPublic, ok, err)
	}
}

// TestSend_RejectsOversizePayload proves Send rejects a too-large payload
// before making any request.
func TestSend_RejectsOversizePayload(t *testing.T) {
	s := newWebPushTestStore(t)
	w := New(s)

	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler(http.StatusCreated))
	defer srv.Close()

	priv, auth := newPushSubscriberKeys(t)
	if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
		Endpoint: srv.URL, KeysJSON: pushSubscriptionKeysJSON(t, priv.PublicKey().Bytes(), auth),
	}); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}

	payload := bytes.Repeat([]byte("a"), maxPayloadBytes+1)
	err := w.Send(t.Context(), payload)
	if err == nil {
		t.Fatal("Send: err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "3993") {
		t.Errorf("Send error = %q, want it to mention 3993", err.Error())
	}
	if got := rec.count(); got != 0 {
		t.Errorf("push service received %d requests, want 0 (Send must reject before listing subscriptions)", got)
	}
}

// TestSend_PostsEncryptedPayloadToEverySubscription proves Send encrypts
// one payload per subscription that decrypts (through decryptForTest) to
// the exact payload, sets TTL 86400, and signs the Authorization header's
// k= with the same value PublicKey returns.
func TestSend_PostsEncryptedPayloadToEverySubscription(t *testing.T) {
	s := newWebPushTestStore(t)
	w := New(s)

	priv1, auth1 := newPushSubscriberKeys(t)
	priv2, auth2 := newPushSubscriberKeys(t)

	rec := &pushRecorder{}
	srv1 := httptest.NewServer(rec.handler(http.StatusCreated))
	defer srv1.Close()
	srv2 := httptest.NewServer(rec.handler(http.StatusCreated))
	defer srv2.Close()

	if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
		Endpoint: srv1.URL, KeysJSON: pushSubscriptionKeysJSON(t, priv1.PublicKey().Bytes(), auth1),
	}); err != nil {
		t.Fatalf("UpsertPushSubscription (one): %v", err)
	}
	if err := s.UpsertPushSubscription(t.Context(), store.PushSubscription{
		Endpoint: srv2.URL, KeysJSON: pushSubscriptionKeysJSON(t, priv2.PublicKey().Bytes(), auth2),
	}); err != nil {
		t.Fatalf("UpsertPushSubscription (two): %v", err)
	}

	pub, err := w.PublicKey(t.Context())
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	payload := []byte(`{"title":"hi"}`)
	if err := w.Send(t.Context(), payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := rec.count(); got != 2 {
		t.Fatalf("got %d requests, want 2", got)
	}

	privs := []*ecdh.PrivateKey{priv1, priv2}
	auths := [][]byte{auth1, auth2}
	for i, req := range rec.reqs {
		if got := req.headers.Get("TTL"); got != "86400" {
			t.Errorf("request %d TTL = %q, want 86400", i, got)
		}
		authz := req.headers.Get("Authorization")
		if !strings.HasPrefix(authz, "vapid t=") {
			t.Errorf("request %d Authorization = %q, want a \"vapid t=\" prefix", i, authz)
		}
		if !strings.Contains(authz, "k="+pub) {
			t.Errorf("request %d Authorization = %q, want k=%s", i, authz, pub)
		}
		got, err := decryptForTest(req.body, privs[i], auths[i])
		if err != nil {
			t.Fatalf("decryptForTest(request %d): %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("request %d decrypted = %q, want %q", i, got, payload)
		}
	}
}
