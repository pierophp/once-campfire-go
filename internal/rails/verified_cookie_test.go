package rails

import (
	"testing"
	"time"
)

func TestVerifyStringCookieRemembersSignatureButChecksExpiry(t *testing.T) {
	secrets, err := NewSecrets("test-secret-key-base-for-verified-cookies")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	raw, err := secrets.SignCookie("session_token", "abc", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got, err := secrets.VerifyStringCookie("session_token", raw, now); err != nil || got != "abc" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	if _, err = secrets.VerifyStringCookie("session_token", raw, now.Add(2*time.Hour)); err == nil {
		t.Fatal("remembered cookie outlived its expiry")
	}
	if _, err = secrets.VerifyStringCookie("other", raw, now); err == nil {
		t.Fatal("cookie verified under another purpose")
	}
	if _, err = secrets.VerifyStringCookie("session_token", raw[:len(raw)-1]+"0", now); err == nil && raw[len(raw)-1] != '0' {
		t.Fatal("tampered signature verified")
	}
}
