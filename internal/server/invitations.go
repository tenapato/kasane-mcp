package server

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tenapato/kasane-mcp/internal/core"
	"golang.org/x/crypto/bcrypt"
)

type waitlistEntry struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Invited   bool      `json:"invited"`
}

type invitation struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func normalizeEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) == 0 || len(email) > 320 {
		return "", false
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || !strings.Contains(email, "@") {
		return "", false
	}
	return email, true
}

func isUniqueError(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (a *App) joinWaitlist(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r, a.cfg.PublicURL) {
		fail(w, http.StatusForbidden, "invalid origin")
		return
	}
	if !a.loginLimiter.allow("waitlist:"+a.clientIP(r), time.Now()) {
		fail(w, http.StatusTooManyRequests, "too many requests; try again in 15 minutes")
		return
	}
	var in struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	email, ok := normalizeEmail(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	if !ok {
		fail(w, http.StatusBadRequest, "valid email required")
		return
	}
	if in.Name == "" || len(in.Name) > 200 {
		fail(w, http.StatusBadRequest, "name must be 1–200 bytes")
		return
	}
	_, err := a.store.DB.Exec(r.Context(), "INSERT INTO waitlist(email,name) VALUES($1,$2) ON CONFLICT (email) DO NOTHING", email, in.Name)
	if err != nil {
		a.failure(w, err)
		return
	}
	// Deliberately return the same response for new and existing addresses.
	respond(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (a *App) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r, a.cfg.PublicURL) {
		fail(w, http.StatusForbidden, "invalid origin")
		return
	}
	if !a.loginLimiter.allow("invitation:"+a.clientIP(r), time.Now()) {
		fail(w, http.StatusTooManyRequests, "too many requests; try again in 15 minutes")
		return
	}
	var in struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if len(in.Token) < 20 || len(in.Token) > 128 {
		fail(w, http.StatusBadRequest, "invalid invitation")
		return
	}
	if in.Username == "" || len(in.Username) > 128 || len(in.Password) < 12 || len(in.Password) > 72 {
		fail(w, http.StatusBadRequest, "username required; password must be 12–72 bytes")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		a.failure(w, err)
		return
	}
	tx, err := a.store.DB.Begin(r.Context())
	if err != nil {
		a.failure(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var email string
	err = tx.QueryRow(r.Context(), `SELECT email FROM invitations WHERE token_hash=$1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, hashToken(in.Token)).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusBadRequest, "invalid or expired invitation")
		return
	}
	if err != nil {
		a.failure(w, err)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO owners(username,email,password_hash,role) VALUES($1,$2,$3,'user')`, in.Username, email, string(hash)); err != nil {
		if isUniqueError(err) {
			fail(w, http.StatusConflict, "account details unavailable; choose another username or contact administrator")
		} else {
			a.failure(w, err)
		}
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO workspaces(id,name,owner_username) VALUES(gen_random_uuid(),'Personal',$1)`, in.Username); err != nil {
		a.failure(w, err)
		return
	}
	if _, err = tx.Exec(r.Context(), `UPDATE invitations SET accepted_at=now() WHERE token_hash=$1`, hashToken(in.Token)); err != nil {
		a.failure(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		a.failure(w, err)
		return
	}
	respond(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (a *App) listWaitlist(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.DB.Query(r.Context(), `SELECT w.id::text,w.email,w.name,w.created_at,EXISTS(SELECT 1 FROM invitations i WHERE i.email=w.email AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at>now()) FROM waitlist w ORDER BY w.created_at,w.id`)
	if err != nil {
		a.failure(w, err)
		return
	}
	defer rows.Close()
	out := []waitlistEntry{}
	for rows.Next() {
		var entry waitlistEntry
		if err = rows.Scan(&entry.ID, &entry.Email, &entry.Name, &entry.CreatedAt, &entry.Invited); err != nil {
			a.failure(w, err)
			return
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		a.failure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"entries": out})
}

func (a *App) invitations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.store.DB.Query(r.Context(), `SELECT id::text,email,created_at,expires_at,accepted_at,revoked_at FROM invitations ORDER BY created_at DESC,id`)
		if err != nil {
			a.failure(w, err)
			return
		}
		defer rows.Close()
		out := []invitation{}
		for rows.Next() {
			var item invitation
			if err = rows.Scan(&item.ID, &item.Email, &item.CreatedAt, &item.ExpiresAt, &item.AcceptedAt, &item.RevokedAt); err != nil {
				a.failure(w, err)
				return
			}
			out = append(out, item)
		}
		if err = rows.Err(); err != nil {
			a.failure(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]any{"invitations": out})
	case http.MethodPost:
		var in struct {
			Email string `json:"email"`
		}
		if !decode(w, r, &in) {
			return
		}
		email, ok := normalizeEmail(in.Email)
		if !ok {
			fail(w, http.StatusBadRequest, "valid email required")
			return
		}
		var registered bool
		if err := a.store.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM owners WHERE email=$1)`, email).Scan(&registered); err != nil {
			a.failure(w, err)
			return
		}
		if registered {
			fail(w, http.StatusConflict, "account already exists")
			return
		}
		token := randomToken()
		var item invitation
		err := a.store.DB.QueryRow(r.Context(), `INSERT INTO invitations(email,token_hash,expires_at) VALUES($1,$2,now()+interval '7 days') RETURNING id::text,email,created_at,expires_at,accepted_at,revoked_at`, email, hashToken(token)).Scan(&item.ID, &item.Email, &item.CreatedAt, &item.ExpiresAt, &item.AcceptedAt, &item.RevokedAt)
		if err != nil {
			if isUniqueError(err) {
				fail(w, http.StatusConflict, "invitation already exists")
				return
			}
			a.failure(w, err)
			return
		}
		respond(w, http.StatusCreated, map[string]any{"invitation": item, "invite_url": a.cfg.PublicURL + "/invite#token=" + token})
	case http.MethodDelete:
		id := r.PathValue("invitation")
		if !core.ValidID(id) {
			fail(w, http.StatusBadRequest, "invalid invitation ID")
			return
		}
		result, err := a.store.DB.Exec(r.Context(), `UPDATE invitations SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1::uuid`, id)
		if err != nil {
			a.failure(w, err)
			return
		}
		if result.RowsAffected() == 0 {
			fail(w, http.StatusNotFound, "invitation not found")
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
