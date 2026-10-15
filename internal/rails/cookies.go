// Package rails implements the persisted Rails wire formats used by Campfire.
package rails

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrInvalid = errors.New("invalid signed or encrypted message")

// Secrets are derived once at boot, as in reference/crates/rails_compat/src/cookies.rs.
// Rails derives with SHA256 but signs cookies with SHA1.
type Secrets struct {
	secret     string
	signing    []byte
	signedIDs  []byte
	signedGIDs []byte
	streams    []byte
	encryption cipher.AEAD

	verified      sync.Map
	verifiedCount atomic.Int64
}

func DeriveKey(secret, salt string, length int) []byte {
	key, err := pbkdf2.Key(sha256.New, secret, []byte(salt), 1000, length)
	if err != nil {
		panic(err)
	}
	return key
}

func NewSecrets(secret string) (*Secrets, error) {
	if secret == "" {
		return nil, errors.New("SECRET_KEY_BASE is required")
	}
	block, err := aes.NewCipher(DeriveKey(secret, "authenticated encrypted cookie", 32))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Secrets{secret: secret, signedIDs: DeriveKey(secret, "active_record/signed_id", 64), signedGIDs: DeriveKey(secret, "signed_global_ids", 64), signing: DeriveKey(secret, "signed cookie", 64), encryption: aead, streams: DeriveKey(secret, "turbo/signed_stream_verifier_key", 64)}, nil
}

func encode(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	// ActiveSupport escapes HTML characters but preserves Unicode line separators.
	// Decode string tokens and re-quote separators only, without changing literal \u text.
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	for i := 0; i < len(data); i++ {
		if data[i] == '\\' && i+1 < len(data) {
			if i+5 < len(data) && (string(data[i:i+6]) == "\\u2028" || string(data[i:i+6]) == "\\u2029") {
				if data[i+5] == '8' {
					out.WriteRune('\u2028')
				} else {
					out.WriteRune('\u2029')
				}
				i += 5
			} else {
				out.WriteByte(data[i])
				i++
				out.WriteByte(data[i])
			}
		} else {
			out.WriteByte(data[i])
		}
	}
	return []byte(out.String()), nil
}

// Struct field order is part of Rails' signed byte representation.
func envelope(value any, name string, expires time.Time) ([]byte, error) {
	data, err := encode(value)
	if err != nil {
		return nil, err
	}
	var expiry *string
	if !expires.IsZero() {
		s := expires.UTC().Format("2006-01-02T15:04:05.000Z")
		expiry = &s
	}
	return encode(struct {
		Rails struct {
			Message string  `json:"message"`
			Exp     *string `json:"exp"`
			Pur     string  `json:"pur"`
		} `json:"_rails"`
	}{struct {
		Message string  `json:"message"`
		Exp     *string `json:"exp"`
		Pur     string  `json:"pur"`
	}{base64.StdEncoding.EncodeToString(data), expiry, "cookie." + name}})
}

func decode64(s string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.NewReplacer("-", "+", "_", "/").Replace(s), "="))
}

func unpack(data []byte, name string, now time.Time, dest any) error {
	_, err := unpackExpiry(data, name, now, dest)
	return err
}

// unpackExpiry also reports the envelope's expiry, or the zero time when it has none.
func unpackExpiry(data []byte, name string, now time.Time, dest any) (time.Time, error) {
	var expires time.Time
	var obj map[string]json.RawMessage
	if strings.HasPrefix(string(data), `{"_rails":{"message":`) && json.Unmarshal(data, &obj) == nil && obj["_rails"] != nil {
		var meta struct {
			Message *string `json:"message"`
			Exp     *string `json:"exp"`
			Pur     *string `json:"pur"`
		}
		if json.Unmarshal(obj["_rails"], &meta) != nil || meta.Message == nil {
			return expires, ErrInvalid
		}
		if meta.Pur != nil && *meta.Pur != "" && *meta.Pur != "cookie."+name {
			return expires, ErrInvalid
		}
		if meta.Exp != nil {
			expiry, err := time.Parse(time.RFC3339Nano, *meta.Exp)
			if err != nil || !now.Before(expiry) {
				return expires, ErrInvalid
			}
			expires = expiry
		}
		var err error
		data, err = decode64(*meta.Message)
		if err != nil {
			return expires, ErrInvalid
		}
	}
	// Pre-metadata JSON cookies are accepted; Marshal is deliberately never decoded.
	if err := json.Unmarshal(data, dest); err != nil {
		return expires, ErrInvalid
	}
	return expires, nil
}

func (s *Secrets) SignCookie(name string, value any, expires time.Time) (string, error) {
	data, err := envelope(value, name, expires)
	if err != nil {
		return "", err
	}
	payload := base64.StdEncoding.EncodeToString(data)
	mac := hmac.New(sha1.New, s.signing)
	mac.Write([]byte(payload))
	return payload + "--" + hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *Secrets) VerifyCookie(name, raw string, now time.Time, dest any) error {
	i := len(raw) - 42
	if i <= 0 || raw[i:i+2] != "--" {
		return ErrInvalid
	}
	payload, signature := raw[:i], raw[i+2:]
	mac := hmac.New(sha1.New, s.signing)
	mac.Write([]byte(payload))
	if !hmac.Equal([]byte(signature), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return ErrInvalid
	}
	data, err := decode64(payload)
	if err != nil {
		return ErrInvalid
	}
	return unpack(data, name, now, dest)
}

func (s *Secrets) EncryptCookie(name string, value any, expires time.Time) (string, error) {
	data, err := envelope(value, name, expires)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.encryption.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	encrypted := s.encryption.Seal(nil, nonce, data, nil)
	split := len(encrypted) - s.encryption.Overhead()
	b64 := base64.StdEncoding.EncodeToString
	return b64(encrypted[:split]) + "--" + b64(nonce) + "--" + b64(encrypted[split:]), nil
}

func (s *Secrets) DecryptCookie(name, raw string, now time.Time, dest any) error {
	parts := strings.Split(raw, "--")
	if len(parts) != 3 {
		return ErrInvalid
	}
	ciphertext, e1 := base64.StdEncoding.DecodeString(parts[0])
	nonce, e2 := base64.StdEncoding.DecodeString(parts[1])
	tag, e3 := base64.StdEncoding.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || len(nonce) != s.encryption.NonceSize() || len(tag) != s.encryption.Overhead() {
		return ErrInvalid
	}
	data, err := s.encryption.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return ErrInvalid
	}
	return unpack(data, name, now, dest)
}

func EscapeCookie(s string) string {
	// Rack allows '*' and escapes '~', unlike Go's QueryEscape.
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(s), "~", "%7E"), "%2A", "*")
}
func UnescapeCookie(s string) string {
	v, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	return v
}

type verifiedCookie struct {
	value   string
	expires time.Time
}

const verifiedCookieLimit = 4096

// VerifyStringCookie is VerifyCookie for string payloads. A signature that already
// verified is remembered, so repeated requests skip the HMAC and JSON decoding; the
// envelope's expiry is still checked against now on every call.
func (s *Secrets) VerifyStringCookie(name, raw string, now time.Time) (string, error) {
	return s.verifyString(name, raw, false, now)
}

// VerifyEscapedStringCookie verifies a cookie value as it arrives in the Cookie header, with
// the URL escaping Rails applies. The escaped form is the cache key, so a repeat request
// skips the unescape as well.
func (s *Secrets) VerifyEscapedStringCookie(name, escaped string, now time.Time) (string, error) {
	return s.verifyString(name, escaped, true, now)
}

func (s *Secrets) verifyString(name, raw string, escaped bool, now time.Time) (string, error) {
	key := name + "\x00" + raw
	if escaped {
		key = "e" + key
	}
	if cached, ok := s.verified.Load(key); ok {
		entry := cached.(verifiedCookie)
		if entry.expires.IsZero() || now.Before(entry.expires) {
			return entry.value, nil
		}
		s.verified.Delete(key)
	}
	if escaped {
		raw = UnescapeCookie(raw)
	}
	i := len(raw) - 42
	if i <= 0 || raw[i:i+2] != "--" {
		return "", ErrInvalid
	}
	payload, signature := raw[:i], raw[i+2:]
	mac := hmac.New(sha1.New, s.signing)
	mac.Write([]byte(payload))
	if !hmac.Equal([]byte(signature), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		return "", ErrInvalid
	}
	data, err := decode64(payload)
	if err != nil {
		return "", ErrInvalid
	}
	var value string
	expires, err := unpackExpiry(data, name, now, &value)
	if err != nil {
		return "", err
	}
	if s.verifiedCount.Add(1) > verifiedCookieLimit {
		s.verified.Clear()
		s.verifiedCount.Store(1)
	}
	s.verified.Store(key, verifiedCookie{value, expires})
	return value, nil
}
