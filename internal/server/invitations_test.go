package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tenapato/kasane-mcp/internal/search"
)

func invitationApp(t *testing.T) *App {
	t.Helper()
	a, err := New(testStore(t), search.NewQdrant("http://127.0.0.1:1", ""), Config{PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.store.Close)
	return a
}

func invitationRequest(t *testing.T, h http.HandlerFunc, body any, origin string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "http://localhost/api", bytes.NewReader(raw))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.NewDecoder(w.Body).Decode(&out)
	return w.Code, out
}

func TestWaitlistDuplicateAndOriginRateLimit(t *testing.T) {
	a := invitationApp(t)
	body := map[string]string{"email": "person@example.com", "name": "Person"}
	status, first := invitationRequest(t, a.joinWaitlist, body, "http://localhost")
	if status != http.StatusAccepted || first["ok"] != true {
		t.Fatalf("first waitlist request: %d %+v", status, first)
	}
	status, second := invitationRequest(t, a.joinWaitlist, body, "http://localhost")
	if status != http.StatusAccepted || second["ok"] != true {
		t.Fatalf("duplicate waitlist request: %d %+v", status, second)
	}
	status, _ = invitationRequest(t, a.joinWaitlist, body, "https://other.example")
	if status != http.StatusForbidden {
		t.Fatalf("invalid origin status %d", status)
	}
	for i := 0; i < 8; i++ {
		status, _ = invitationRequest(t, a.joinWaitlist, map[string]string{"email": "person" + string(rune('a'+i)) + "@example.com", "name": "Person"}, "http://localhost")
	}
	if status != http.StatusAccepted {
		t.Fatalf("tenth request status %d", status)
	}
	status, _ = invitationRequest(t, a.joinWaitlist, map[string]string{"email": "last@example.com", "name": "Person"}, "http://localhost")
	if status != http.StatusTooManyRequests {
		t.Fatalf("rate limit status %d", status)
	}
}

func TestInvitationAcceptedOnceAndCreatesPrivateWorkspace(t *testing.T) {
	a := invitationApp(t)
	ctx := context.Background()
	token := randomToken()
	if _, err := a.store.DB.Exec(ctx, `INSERT INTO invitations(email,token_hash,expires_at) VALUES('invite@example.com',$1,now()+interval '7 days')`, hashToken(token)); err != nil {
		t.Fatal(err)
	}
	status, _ := invitationRequest(t, a.acceptInvitation, map[string]string{"token": token, "username": "new-user", "password": "password-long-enough"}, "http://localhost")
	if status != http.StatusCreated {
		t.Fatalf("accept status %d", status)
	}
	var role, email, owner string
	if err := a.store.DB.QueryRow(ctx, `SELECT role,email FROM owners WHERE username='new-user'`).Scan(&role, &email); err != nil {
		t.Fatal(err)
	}
	if role != "user" || email != "invite@example.com" {
		t.Fatalf("owner fields role=%q email=%q", role, email)
	}
	if err := a.store.DB.QueryRow(ctx, `SELECT owner_username FROM workspaces WHERE owner_username='new-user' AND name='Personal'`).Scan(&owner); err != nil || owner != "new-user" {
		t.Fatalf("private workspace owner=%q err=%v", owner, err)
	}
	status, _ = invitationRequest(t, a.acceptInvitation, map[string]string{"token": token, "username": "another", "password": "password-long-enough"}, "http://localhost")
	if status != http.StatusBadRequest {
		t.Fatalf("reused token status %d", status)
	}
}

func TestInvitationExpiredRevokedAndUsernameRollback(t *testing.T) {
	a := invitationApp(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		token string
		when  string
	}{
		{"expired", randomToken(), "now()-interval '1 minute'"},
		{"revoked", randomToken(), "now()+interval '7 days'"},
	} {
		var revoked any
		if tc.name == "revoked" {
			revoked = time.Now()
		}
		_, err := a.store.DB.Exec(ctx, `INSERT INTO invitations(email,token_hash,expires_at,revoked_at) VALUES($1,$2,`+tc.when+`,$3)`, tc.name+"@example.com", hashToken(tc.token), revoked)
		if err != nil {
			t.Fatal(err)
		}
		status, _ := invitationRequest(t, a.acceptInvitation, map[string]string{"token": tc.token, "username": tc.name, "password": "password-long-enough"}, "http://localhost")
		if status != http.StatusBadRequest {
			t.Fatalf("%s status %d", tc.name, status)
		}
	}
	if _, err := a.store.DB.Exec(ctx, `INSERT INTO owners(username,password_hash,role) VALUES('taken','x','admin')`); err != nil {
		t.Fatal(err)
	}
	token := randomToken()
	if _, err := a.store.DB.Exec(ctx, `INSERT INTO invitations(email,token_hash,expires_at) VALUES('rollback@example.com',$1,now()+interval '7 days')`, hashToken(token)); err != nil {
		t.Fatal(err)
	}
	status, _ := invitationRequest(t, a.acceptInvitation, map[string]string{"token": token, "username": "taken", "password": "password-long-enough"}, "http://localhost")
	if status != http.StatusConflict {
		t.Fatalf("existing username status %d", status)
	}
	var count int
	if err := a.store.DB.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE owner_username='taken'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback workspace count=%d err=%v", count, err)
	}
}
