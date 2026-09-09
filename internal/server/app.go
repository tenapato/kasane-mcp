package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tenapato/kasane-mcp/internal/core"
	"github.com/tenapato/kasane-mcp/internal/search"
	"github.com/tenapato/kasane-mcp/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type Config struct{ PublicURL, WebDir, TrustedProxyCIDRs string }
type App struct {
	store        *store.Store
	index        *search.Qdrant
	search       *search.Service
	cfg          Config
	secure       bool
	proxies      []netip.Prefix
	loginLimiter *limiter
	dummyHash    string
}

func New(s *store.Store, index *search.Qdrant, cfg Config) (*App, error) {
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	u, e := url.Parse(cfg.PublicURL)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("PUBLIC_URL must be an http(s) origin without a path")
	}
	if u.Scheme == "http" {
		ip, err := netip.ParseAddr(u.Hostname())
		if u.Hostname() != "localhost" && (err != nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("PUBLIC_URL requires HTTPS except for localhost development")
		}
	}
	a := &App{store: s, index: index, search: &search.Service{Repo: s, Index: index}, cfg: cfg, secure: u.Scheme == "https", loginLimiter: newLimiter()}
	for _, s := range strings.Split(cfg.TrustedProxyCIDRs, ",") {
		if s = strings.TrimSpace(s); s != "" {
			p, e := netip.ParsePrefix(s)
			if e != nil {
				return nil, fmt.Errorf("invalid trusted proxy CIDR: %w", e)
			}
			a.proxies = append(a.proxies, p)
		}
	}
	b, e := bcrypt.GenerateFromPassword([]byte(randomToken()), bcrypt.DefaultCost)
	if e != nil {
		return nil, e
	}
	a.dummyHash = string(b)
	if cfg.WebDir != "" {
		if _, e := os.Stat(cfg.WebDir + "/index.html"); e != nil {
			return nil, fmt.Errorf("WEB_DIR lacks index.html: %w", e)
		}
	}
	return a, nil
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func (a *App) failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrInvalid):
		fail(w, 400, err.Error())
	case errors.Is(err, core.ErrConflict):
		fail(w, 409, core.ErrConflict.Error())
	case errors.Is(err, core.ErrNotFound):
		fail(w, 404, "not found")
	case errors.Is(err, errForbidden):
		fail(w, 403, "permission denied")
	default:
		slog.Error("request failed", "error_type", fmt.Sprintf("%T", err))
		fail(w, 503, "service temporarily unavailable")
	}
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "invalid JSON request")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "request must contain one JSON object")
		return false
	}
	return true
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if a.store.DB.Ping(ctx) != nil {
			fail(w, 503, "database unavailable")
			return
		}
		respond(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /api/v1/login", a.login)
	auth := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, a.owner(fn)) }
	auth("GET /api/v1/session", func(w http.ResponseWriter, r *http.Request) {
		id := r.Context().Value(identityKey{}).(identity)
		respond(w, 200, map[string]string{"username": id.Username, "csrf_token": id.CSRF})
	})
	auth("POST /api/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("kasane_session")
		if _, e := a.store.DB.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1", hashToken(c.Value)); e != nil {
			a.failure(w, e)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "kasane_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
		respond(w, 200, map[string]bool{"ok": true})
	})
	auth("GET /api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		ws, e := a.store.Workspaces(r.Context())
		if e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 200, map[string]any{"workspaces": ws})
	})
	auth("POST /api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 120 {
			fail(w, 400, "workspace name must be 1–120 bytes")
			return
		}
		ws, e := a.store.CreateWorkspace(r.Context(), in.Name)
		if e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 201, ws)
	})
	workspace := func(pattern string, fn http.HandlerFunc) {
		auth(pattern, func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("workspace")
			if !core.ValidID(id) {
				fail(w, 400, "invalid workspace ID")
				return
			}
			ok, e := a.store.WorkspaceExists(r.Context(), id)
			if e != nil {
				a.failure(w, e)
				return
			}
			if !ok {
				fail(w, 404, "workspace not found")
				return
			}
			fn(w, r)
		})
	}
	workspace("GET /api/v1/workspaces/{workspace}/memories", a.memories)
	workspace("POST /api/v1/workspaces/{workspace}/memories", a.memories)
	workspace("GET /api/v1/workspaces/{workspace}/memories/{memory}", a.memory)
	workspace("PUT /api/v1/workspaces/{workspace}/memories/{memory}", a.memory)
	workspace("DELETE /api/v1/workspaces/{workspace}/memories/{memory}", a.memory)
	workspace("GET /api/v1/workspaces/{workspace}/keys", a.keys)
	workspace("POST /api/v1/workspaces/{workspace}/keys", a.keys)
	workspace("DELETE /api/v1/workspaces/{workspace}/keys/{key}", a.keys)
	workspace("POST /api/v1/workspaces/{workspace}/reindex", func(w http.ResponseWriter, r *http.Request) {
		if e := a.store.Reindex(r.Context(), r.PathValue("workspace")); e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	auth("GET /api/v1/status", a.status)
	mux.Handle("/mcp", a.agent(a.mcpHandler()))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "endpoint not found") })
	if a.cfg.WebDir != "" {
		mux.Handle("/", spa(os.DirFS(a.cfg.WebDir)))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			respond(w, 200, map[string]string{"name": "kasane-mcp", "mcp": "/mcp", "status": "running", "web_ui": "not installed"})
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if a.secure {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		// Validate Host ourselves, including reverse proxy deployments on loopback.
		public, _ := url.Parse(a.cfg.PublicURL)
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.Host != public.Host {
			fail(w, 403, "unexpected host")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func spa(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(405)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		stat, err := fs.Stat(root, name)
		if err != nil || stat.IsDir() {
			if strings.Contains(name, ".") {
				http.NotFound(w, r)
				return
			}
			data, e := fs.ReadFile(root, "index.html")
			if e != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func (a *App) memories(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("workspace")
	if r.Method == "POST" {
		var in core.RememberInput
		if !decode(w, r, &in) {
			return
		}
		if in.ID != "" {
			fail(w, 400, "use PUT to update a memory")
			return
		}
		m, e := a.store.Remember(r.Context(), ws, in)
		if e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 201, m)
		return
	}
	q := r.URL.Query()
	limit := 50
	offset := 0
	var e error
	if q.Has("limit") {
		limit, e = strconv.Atoi(q.Get("limit"))
		if e != nil {
			fail(w, 400, "invalid limit")
			return
		}
	}
	if q.Has("offset") {
		offset, e = strconv.Atoi(q.Get("offset"))
		if e != nil {
			fail(w, 400, "invalid offset")
			return
		}
	}
	out, e := a.search.Search(r.Context(), ws, core.SearchInput{Query: q.Get("q"), Tag: q.Get("tag"), Kind: q.Get("kind"), Limit: limit, Offset: offset})
	if e != nil {
		a.failure(w, e)
		return
	}
	respond(w, 200, out)
}

func (a *App) memory(w http.ResponseWriter, r *http.Request) {
	ws, id := r.PathValue("workspace"), r.PathValue("memory")
	switch r.Method {
	case "GET":
		m, e := a.store.Get(r.Context(), ws, id)
		if e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 200, m)
	case "PUT":
		var in core.RememberInput
		if !decode(w, r, &in) {
			return
		}
		in.ID = id
		m, e := a.store.Remember(r.Context(), ws, in)
		if e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 200, m)
	case "DELETE":
		if e := a.store.Forget(r.Context(), ws, id); e != nil {
			a.failure(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	}
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	stats, e := a.store.Stats(r.Context())
	if e != nil {
		a.failure(w, e)
		return
	}
	state := "ready"
	if a.index.Health(r.Context()) != nil {
		state = "unavailable"
	}
	respond(w, 200, map[string]any{"postgres": "ready", "qdrant": state, "pending_jobs": stats.PendingJobs, "failed_jobs": stats.FailedJobs, "memory_count": stats.MemoryCount, "mcp_url": a.cfg.PublicURL + "/mcp"})
}

func (a *App) RunWorker(ctx context.Context) {
	ready := false
	lastCheck := time.Time{}
	for ctx.Err() == nil {
		if !ready || time.Since(lastCheck) > 30*time.Second {
			ready = a.index.Ensure(ctx) == nil
			lastCheck = time.Now()
		}
		worked := false
		if ready {
			var e error
			worked, e = a.store.ProcessOne(ctx, a.index.Apply)
			if e != nil && ctx.Err() == nil {
				slog.Warn("index queue temporarily unavailable")
				ready = false
			}
		}
		if !worked {
			delay := time.Second
			if !ready {
				delay = 5 * time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
