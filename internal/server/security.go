package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func hashToken(token string) string {
	b := sha256.Sum256([]byte(token))
	return hex.EncodeToString(b[:])
}
func validOrigin(r *http.Request, publicURL string) bool {
	return r.Header.Get("Origin") == strings.TrimRight(publicURL, "/")
}

type identity struct{ Username, CSRF, Workspace, Scope string }
type identityKey struct{}

type throttle struct {
	Count   int
	Expires time.Time
}
type limiter struct {
	mu      sync.Mutex
	entries map[string]throttle
}

func newLimiter() *limiter { return &limiter{entries: map[string]throttle{}} }
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.entries[key]
	if !now.Before(v.Expires) {
		v = throttle{Expires: now.Add(15 * time.Minute)}
	}
	if v.Count >= 10 {
		return false
	}
	if len(l.entries) >= 10000 {
		for k, item := range l.entries {
			if !now.Before(item.Expires) {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= 10000 {
			return false
		}
	}
	v.Count++
	l.entries[key] = v
	return true
}

func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	trusted := func(ip netip.Addr) bool {
		for _, p := range a.proxies {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(ip) {
		return host
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(chain) - 1; i >= 0; i-- {
		next, e := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if e != nil {
			return host
		}
		ip = next
		if !trusted(ip) {
			return ip.String()
		}
	}
	return ip.String()
}

func Bootstrap(ctx context.Context, s *store.Store, username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 128 || len(password) < 12 || len(password) > 72 {
		return fmt.Errorf("username required; password must be 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(701921)"); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM owners").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("owner already exists; bootstrap cannot replace an account")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO owners(username,password_hash) VALUES($1,$2)", username, string(hash)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r, a.cfg.PublicURL) {
		fail(w, http.StatusForbidden, "invalid origin")
		return
	}
	if !a.loginLimiter.allow(a.clientIP(r), time.Now()) {
		fail(w, http.StatusTooManyRequests, "too many login attempts; try again in 15 minutes")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	var hash string
	err := a.store.DB.QueryRow(r.Context(), "SELECT password_hash FROM owners WHERE username=$1", in.Username).Scan(&hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.failure(w, err)
		return
	}
	if err != nil {
		hash = a.dummyHash
	}
	compare := bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password))
	if err != nil || compare != nil {
		fail(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token, csrf := randomToken(), randomToken()
	tx, e := a.store.DB.Begin(r.Context())
	if e != nil {
		a.failure(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	_, e = tx.Exec(r.Context(), "DELETE FROM sessions WHERE expires_at < now()")
	if e == nil {
		_, e = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,username,csrf_token,expires_at) VALUES($1,$2,$3,$4)", hashToken(token), in.Username, csrf, time.Now().Add(24*time.Hour))
	}
	if e == nil {
		e = tx.Commit(r.Context())
	}
	if e != nil {
		a.failure(w, e)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "kasane_session", Value: token, Path: "/", MaxAge: 86400, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
	respond(w, 200, map[string]any{"username": in.Username, "csrf_token": csrf})
}

func (a *App) owner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("kasane_session")
		if err != nil || len(cookie.Value) > 128 {
			fail(w, 401, "sign in required")
			return
		}
		var id identity
		err = a.store.DB.QueryRow(r.Context(), "SELECT username,csrf_token FROM sessions WHERE token_hash=$1 AND expires_at>now()", hashToken(cookie.Value)).Scan(&id.Username, &id.CSRF)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 401, "sign in required")
			return
		}
		if err != nil {
			a.failure(w, err)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !validOrigin(r, a.cfg.PublicURL) || subtle.ConstantTimeCompare([]byte(id.CSRF), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
				fail(w, 403, "invalid origin or CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (a *App) agent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !validOrigin(r, a.cfg.MCPPublicURL) {
			fail(w, 403, "invalid origin")
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || len(auth) > 160 {
			fail(w, 401, "valid agent key required")
			return
		}
		var id identity
		err := a.store.DB.QueryRow(r.Context(), "UPDATE agent_keys SET last_used_at=now() WHERE token_hash=$1 AND revoked_at IS NULL RETURNING workspace_id,scope", hashToken(strings.TrimPrefix(auth, "Bearer "))).Scan(&id.Workspace, &id.Scope)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 401, "valid agent key required")
			return
		}
		if err != nil {
			a.failure(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func principal(ctx context.Context, write bool) (identity, error) {
	id, ok := ctx.Value(identityKey{}).(identity)
	if !ok || id.Workspace == "" {
		return id, errors.New("agent authentication required")
	}
	if write && id.Scope != "write" {
		return id, fmt.Errorf("%w: key is read-only", errForbidden)
	}
	return id, nil
}

var errForbidden = errors.New("permission denied")

type agentKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func (a *App) keys(w http.ResponseWriter, r *http.Request) {
	workspace := r.PathValue("workspace")
	switch r.Method {
	case "GET":
		rows, err := a.store.DB.Query(r.Context(), "SELECT id,name,prefix,scope,created_at,last_used_at,revoked_at FROM agent_keys WHERE workspace_id=$1 ORDER BY created_at DESC", workspace)
		if err != nil {
			a.failure(w, err)
			return
		}
		defer rows.Close()
		keys := []agentKey{}
		for rows.Next() {
			var k agentKey
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scope, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
				a.failure(w, err)
				return
			}
			keys = append(keys, k)
		}
		if rows.Err() != nil {
			a.failure(w, rows.Err())
			return
		}
		respond(w, 200, map[string]any{"keys": keys})
	case "POST":
		var in struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 120 || (in.Scope != "read" && in.Scope != "write") {
			fail(w, 400, "name and scope (read or write) required")
			return
		}
		token := "ks_" + randomToken()
		k := agentKey{ID: core.UUID(), Name: in.Name, Scope: in.Scope, Prefix: token[:11]}
		err := a.store.DB.QueryRow(r.Context(), "INSERT INTO agent_keys(id,workspace_id,name,prefix,token_hash,scope) VALUES($1,$2,$3,$4,$5,$6) RETURNING created_at", k.ID, workspace, k.Name, k.Prefix, hashToken(token), k.Scope).Scan(&k.CreatedAt)
		if err != nil {
			a.failure(w, err)
			return
		}
		respond(w, 201, map[string]any{"key": k, "token": token})
	case "DELETE":
		id := r.PathValue("key")
		if !core.ValidID(id) {
			fail(w, 400, "invalid key ID")
			return
		}
		_, err := a.store.DB.Exec(r.Context(), "UPDATE agent_keys SET revoked_at=now() WHERE id=$1 AND workspace_id=$2 AND revoked_at IS NULL", id, workspace)
		if err != nil {
			a.failure(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	}
}
