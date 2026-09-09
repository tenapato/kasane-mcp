package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestBrowserMutationRequiresExactOrigin(t *testing.T) {
	for _, origin := range []string{"", "https://evil.example", "https://kasane.example.evil"} {
		r := httptest.NewRequest("POST", "https://kasane.example/api/v1/workspaces", nil)
		r.Header.Set("Origin", origin)
		if validOrigin(r, "https://kasane.example") {
			t.Fatalf("accepted %q", origin)
		}
	}
	r := httptest.NewRequest("POST", "https://kasane.example/api/v1/workspaces", nil)
	r.Header.Set("Origin", "https://kasane.example")
	if !validOrigin(r, "https://kasane.example") {
		t.Fatal("rejected exact origin")
	}
}

func TestTokenHashDoesNotExposeBearer(t *testing.T) {
	token := randomToken()
	if len(token) < 40 || hashToken(token) == token || hashToken(token) != hashToken(token) {
		t.Fatal("bad token hashing")
	}
	if hashToken(token) == hashToken(randomToken()) {
		t.Fatal("token collision")
	}
}

func TestLoginThrottleExpires(t *testing.T) {
	limiter := newLimiter()
	now := time.Now()
	for i := 0; i < 10; i++ {
		if !limiter.allow("client", now) {
			t.Fatal("blocked too early")
		}
	}
	if limiter.allow("client", now) {
		t.Fatal("brute force not limited")
	}
	if !limiter.allow("client", now.Add(16*time.Minute)) {
		t.Fatal("window never expires")
	}
}

func TestRejectsPlainHTTPForPublicHost(t *testing.T) {
	if _, err := New(nil, nil, Config{PublicURL: "http://kasane.example"}); err == nil {
		t.Fatal("public HTTP allowed")
	}
}
