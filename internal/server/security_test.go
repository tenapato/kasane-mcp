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

func TestDedicatedMCPHost(t *testing.T) {
	a, err := New(nil, nil, Config{PublicURL: "https://kasane.example", MCPPublicURL: "https://mcp.kasane.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, path, origin string
		want               int
	}{
		{"mcp.kasane.example", "/mcp", "", 401},
		{"mcp.kasane.example", "/mcp", "https://mcp.kasane.example", 401},
		{"mcp.kasane.example", "/mcp", "https://evil.example", 403},
		{"kasane.example", "/mcp", "", 403},
		{"mcp.kasane.example", "/api/v1/session", "", 403},
		{"mcp.kasane.example", "/panel", "", 403},
		{"kasane.example", "/api/v1/session", "", 401},
	} {
		r := httptest.NewRequest("GET", "https://"+tc.host+tc.path, nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s%s origin=%s: got %d want %d", tc.host, tc.path, tc.origin, w.Code, tc.want)
		}
	}
}

func TestMCPOriginValidationAndDefault(t *testing.T) {
	for _, origin := range []string{"http://mcp.example.com", "https://mcp.example.com/path", "https://user:pass@mcp.example.com", "https://mcp.example.com?q=x"} {
		if _, err := New(nil, nil, Config{PublicURL: "https://kasane.example", MCPPublicURL: origin}); err == nil {
			t.Errorf("accepted invalid MCP origin %s", origin)
		}
	}
	a, err := New(nil, nil, Config{PublicURL: "http://localhost:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.MCPPublicURL != a.cfg.PublicURL {
		t.Fatal("default must preserve same-host deployments")
	}
}
