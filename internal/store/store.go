package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tenapato/kasane-mcp/internal/core"
)

type Store struct{ DB *pgxpool.Pool }
type Stats struct {
	PendingJobs int `json:"pending_jobs"`
	FailedJobs  int `json:"failed_jobs"`
	MemoryCount int `json:"memory_count"`
}

func Open(ctx context.Context, url string) (*Store, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{DB: p}, nil
}
func (s *Store) Close() {
	if s != nil && s.DB != nil {
		s.DB.Close()
	}
}

func uuid() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:])), nil
}
func validUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i, c := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (s *Store) CreateWorkspace(ctx context.Context, name string) (core.Workspace, error) {
	if strings.TrimSpace(name) == "" {
		return core.Workspace{}, core.ErrInvalid
	}
	id, e := uuid()
	if e != nil {
		return core.Workspace{}, e
	}
	var w core.Workspace
	e = s.DB.QueryRow(ctx, `INSERT INTO workspaces(id,name) VALUES($1,$2) RETURNING id::text,name,created_at`, id, strings.TrimSpace(name)).Scan(&w.ID, &w.Name, &w.CreatedAt)
	return w, e
}
func (s *Store) Workspaces(ctx context.Context) ([]core.Workspace, error) {
	rows, e := s.DB.Query(ctx, `SELECT id::text,name,created_at FROM workspaces ORDER BY created_at,id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []core.Workspace{}
	for rows.Next() {
		var w core.Workspace
		if e = rows.Scan(&w.ID, &w.Name, &w.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (s *Store) WorkspaceExists(ctx context.Context, id string) (bool, error) {
	if !validUUID(id) {
		return false, core.ErrInvalid
	}
	var x bool
	e := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1::uuid)`, id).Scan(&x)
	return x, e
}

const memCols = `kind,id::text,workspace_id::text,title,content,tags,source,revision,indexed_revision,(indexed_revision=revision),created_at,updated_at,deleted`

func scanMemory(row pgx.Row) (core.Memory, error) {
	var m core.Memory
	var indexed bool
	e := row.Scan(&m.Kind, &m.ID, &m.WorkspaceID, &m.Title, &m.Content, &m.Tags, &m.Source, &m.Revision, &m.IndexedRevision, &indexed, &m.CreatedAt, &m.UpdatedAt, &m.Deleted)
	if e == nil {
		if indexed {
			m.IndexingStatus = "indexed"
		} else {
			m.IndexingStatus = "pending"
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
	}
	return m, e
}
func (s *Store) getTx(ctx context.Context, tx pgx.Tx, ws, id string, includeDeleted bool) (core.Memory, error) {
	q := `SELECT ` + memCols + ` FROM memories WHERE workspace_id=$1::uuid AND id=$2::uuid`
	if !includeDeleted {
		q += ` AND deleted=false`
	}
	return scanMemory(tx.QueryRow(ctx, q, ws, id))
}

func (s *Store) Remember(ctx context.Context, ws string, in core.RememberInput) (core.Memory, error) {
	if e := in.Validate(); e != nil {
		return core.Memory{}, e
	}
	if in.ID != "" && !validUUID(in.ID) {
		return core.Memory{}, core.ErrInvalid
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return core.Memory{}, e
	}
	defer tx.Rollback(ctx)
	if ok, e := s.workspaceTx(ctx, tx, ws); e != nil {
		return core.Memory{}, e
	} else if !ok {
		return core.Memory{}, core.ErrNotFound
	}
	if _, e = tx.Exec(ctx, `SELECT id FROM workspaces WHERE id=$1::uuid FOR UPDATE`, ws); e != nil {
		return core.Memory{}, e
	}
	if in.ID == "" && in.IdempotencyKey != "" {
		var id string
		var same bool
		e = tx.QueryRow(ctx, `SELECT id::text,(title=$3 AND content=$4 AND tags=$5 AND source=$6 AND kind=$7) FROM memories WHERE workspace_id=$1::uuid AND idempotency_key=$2 FOR UPDATE`, ws, in.IdempotencyKey, in.Title, in.Content, in.Tags, in.Source, in.Kind).Scan(&id, &same)
		if e == nil {
			m, ge := s.getTx(ctx, tx, ws, id, true)
			if ge != nil {
				return core.Memory{}, ge
			}
			if m.Deleted || !same {
				return core.Memory{}, core.ErrConflict
			}
			return m, tx.Commit(ctx)
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return core.Memory{}, e
		}
	}
	id := in.ID
	if id == "" {
		id, e = uuid()
		if e != nil {
			return core.Memory{}, e
		}
		if in.Kind == "stack" || in.Kind == "practice" {
			var n int
			if e = tx.QueryRow(ctx, `SELECT count(*) FROM memories WHERE workspace_id=$1::uuid AND deleted=false AND kind IN ('stack','practice')`, ws).Scan(&n); e != nil {
				return core.Memory{}, e
			}
			if n >= 100 {
				return core.Memory{}, fmt.Errorf("%w: development profile is limited to 100 entries", core.ErrInvalid)
			}
		}
		var m core.Memory
		var dummyBool bool
		var idem any = in.IdempotencyKey
		if in.IdempotencyKey == "" {
			idem = nil
		}
		e = tx.QueryRow(ctx, `INSERT INTO memories(kind,id,workspace_id,title,content,tags,source,revision,indexed_revision,idempotency_key) VALUES($1,$2::uuid,$3::uuid,$4,$5,$6,$7,1,0,$8) RETURNING `+memCols, in.Kind, id, ws, in.Title, in.Content, in.Tags, in.Source, idem).Scan(&m.Kind, &m.ID, &m.WorkspaceID, &m.Title, &m.Content, &m.Tags, &m.Source, &m.Revision, &m.IndexedRevision, &dummyBool, &m.CreatedAt, &m.UpdatedAt, &m.Deleted)
		if e != nil {
			return core.Memory{}, e
		}
		m.IndexingStatus = "pending"
		e = s.outboxTx(ctx, tx, id, "upsert", 1)
		if e == nil {
			e = tx.Commit(ctx)
		}
		return m, e
	}
	m, e := s.getTx(ctx, tx, ws, id, false)
	if e != nil {
		return core.Memory{}, mapErr(e)
	}
	if in.ExpectedRevision <= 0 || m.Revision != in.ExpectedRevision {
		return core.Memory{}, core.ErrConflict
	}
	if m.Kind == "memory" && (in.Kind == "stack" || in.Kind == "practice") {
		var n int
		if e = tx.QueryRow(ctx, `SELECT count(*) FROM memories WHERE workspace_id=$1::uuid AND deleted=false AND kind IN ('stack','practice')`, ws).Scan(&n); e != nil {
			return core.Memory{}, e
		}
		if n >= 100 {
			return core.Memory{}, fmt.Errorf("%w: development profile is limited to 100 entries", core.ErrInvalid)
		}
	}
	var out core.Memory
	var dummyBool bool
	e = tx.QueryRow(ctx, `UPDATE memories SET kind=$1,title=$2,content=$3,tags=$4,source=$5,revision=revision+1,indexed_revision=0,updated_at=now() WHERE workspace_id=$6::uuid AND id=$7::uuid AND revision=$8 RETURNING `+memCols, in.Kind, in.Title, in.Content, in.Tags, in.Source, ws, id, in.ExpectedRevision).Scan(&out.Kind, &out.ID, &out.WorkspaceID, &out.Title, &out.Content, &out.Tags, &out.Source, &out.Revision, &out.IndexedRevision, &dummyBool, &out.CreatedAt, &out.UpdatedAt, &out.Deleted)
	if e == nil {
		out.IndexingStatus = "pending"
		e = s.outboxTx(ctx, tx, id, "upsert", out.Revision)
	}
	if errors.Is(e, pgx.ErrNoRows) {
		e = core.ErrConflict
	}
	if e == nil {
		e = tx.Commit(ctx)
	}
	return out, e
}

func mapErr(e error) error {
	if errors.Is(e, pgx.ErrNoRows) {
		return core.ErrNotFound
	}
	return e
}
func (s *Store) workspaceTx(ctx context.Context, tx pgx.Tx, ws string) (bool, error) {
	var ok bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1::uuid)`, ws).Scan(&ok)
	return ok, e
}
func (s *Store) outboxTx(ctx context.Context, tx pgx.Tx, id, kind string, rev int64) error {
	_, e := tx.Exec(ctx, `INSERT INTO outbox(memory_id,event_kind,revision,next_attempt) VALUES($1::uuid,$2,$3,now()) ON CONFLICT(memory_id) DO UPDATE SET event_kind=EXCLUDED.event_kind,revision=EXCLUDED.revision,next_attempt=now(),attempts=0,last_error=''`, id, kind, rev)
	return e
}

func (s *Store) Get(ctx context.Context, ws, id string) (core.Memory, error) {
	if !validUUID(id) {
		return core.Memory{}, core.ErrInvalid
	}
	m, e := scanMemory(s.DB.QueryRow(ctx, `SELECT `+memCols+` FROM memories WHERE workspace_id=$1::uuid AND id=$2::uuid AND deleted=false`, ws, id))
	return m, mapErr(e)
}
func (s *Store) Forget(ctx context.Context, ws, id string) error {
	if !validUUID(id) {
		return core.ErrInvalid
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var rev int64
	e = tx.QueryRow(ctx, `UPDATE memories SET title='',content='',tags='{}',source='',deleted=true,revision=revision+1,indexed_revision=0,updated_at=now() WHERE workspace_id=$1::uuid AND id=$2::uuid AND deleted=false RETURNING revision`, ws, id).Scan(&rev)
	if errors.Is(e, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if e != nil {
		return e
	}
	if e = s.outboxTx(ctx, tx, id, "delete", rev); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func (s *Store) List(ctx context.Context, ws string, in core.SearchInput) (core.SearchResult, error) {
	if e := in.Validate(); e != nil {
		return core.SearchResult{}, e
	}
	args := []any{ws}
	where := []string{"workspace_id=$1::uuid", "deleted=false"}
	if in.Kind != "" {
		args = append(args, in.Kind)
		where = append(where, fmt.Sprintf("kind=$%d", len(args)))
	}
	if in.Tag != "" {
		args = append(args, in.Tag)
		where = append(where, fmt.Sprintf("$%d=ANY(tags)", len(args)))
	}
	if in.Query != "" {
		args = append(args, in.Query)
		where = append(where, fmt.Sprintf("search_vector @@ websearch_to_tsquery('simple',$%d)", len(args)))
	}
	base := strings.Join(where, " AND ")
	var total int
	e := s.DB.QueryRow(ctx, "SELECT count(*) FROM memories WHERE "+base, args...).Scan(&total)
	if e != nil {
		return core.SearchResult{}, e
	}
	args = append(args, in.Limit, in.Offset)
	order := "updated_at DESC,id DESC"
	if in.Query != "" {
		order = fmt.Sprintf("ts_rank(search_vector,websearch_to_tsquery('simple',$%d)) DESC,updated_at DESC,id DESC", len(args)-2)
	}
	q := "SELECT " + memCols + " FROM memories WHERE " + base + " ORDER BY " + order + " LIMIT $" + fmt.Sprint(len(args)-1) + " OFFSET $" + fmt.Sprint(len(args))
	rows, e := s.DB.Query(ctx, q, args...)
	if e != nil {
		return core.SearchResult{}, e
	}
	defer rows.Close()
	out := core.SearchResult{Memories: []core.Memory{}, Total: total}
	for rows.Next() {
		m, e := scanMemory(rows)
		if e != nil {
			return out, e
		}
		out.Memories = append(out.Memories, m)
	}
	return out, rows.Err()
}
func (s *Store) Rehydrate(ctx context.Context, ws string, hits []core.Hit) ([]core.Memory, error) {
	out := make([]core.Memory, 0, len(hits))
	for _, h := range hits {
		if !validUUID(h.ID) {
			continue
		}
		m, e := s.Get(ctx, ws, h.ID)
		if errors.Is(e, pgx.ErrNoRows) || errors.Is(e, core.ErrNotFound) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if m.Revision != h.Revision {
			continue
		}
		m.Score = h.Score
		out = append(out, m)
	}
	return out, nil
}
func (s *Store) Profile(ctx context.Context, ws string) ([]core.Memory, error) {
	rows, e := s.DB.Query(ctx, `SELECT `+memCols+` FROM memories WHERE workspace_id=$1::uuid AND deleted=false AND kind IN ('stack','practice') ORDER BY kind,title,id LIMIT 100`, ws)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []core.Memory{}
	for rows.Next() {
		m, e := scanMemory(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (s *Store) Reindex(ctx context.Context, ws string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	q := `SELECT id::text,revision FROM memories WHERE deleted=false`
	args := []any{}
	if ws != "" {
		q += ` AND workspace_id=$1::uuid`
		args = append(args, ws)
	}
	rows, e := tx.Query(ctx, q, args...)
	if e != nil {
		return e
	}
	items := []struct {
		id  string
		rev int64
	}{}
	for rows.Next() {
		var id string
		var rev int64
		if e = rows.Scan(&id, &rev); e != nil {
			return e
		}
		items = append(items, struct {
			id  string
			rev int64
		}{id, rev})
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return e
	}
	rows.Close()
	for _, item := range items {
		id, rev := item.id, item.rev
		if _, e = tx.Exec(ctx, `UPDATE memories SET indexed_revision=0 WHERE id=$1::uuid`, id); e != nil {
			return e
		}
		if e = s.outboxTx(ctx, tx, id, "upsert", rev); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ProcessOne(ctx context.Context, apply func(context.Context, core.Memory) error) (bool, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	var id, kind string
	var rev int64
	e = tx.QueryRow(ctx, `SELECT o.memory_id::text,o.event_kind,o.revision FROM outbox o JOIN memories m ON m.id=o.memory_id WHERE o.next_attempt<=now() ORDER BY o.next_attempt,o.created_at FOR UPDATE OF m SKIP LOCKED LIMIT 1`).Scan(&id, &kind, &rev)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	m, e := s.getAnyTx(ctx, tx, id)
	if e != nil {
		return false, e
	}
	// Lock memory first, then outbox. This ordering is shared with writers and
	// prevents a worker callback from racing a revision update.
	var due time.Time
	e = tx.QueryRow(ctx, `SELECT event_kind,revision,next_attempt FROM outbox WHERE memory_id=$1::uuid FOR UPDATE`, id).Scan(&kind, &rev, &due)
	if errors.Is(e, pgx.ErrNoRows) || due.After(time.Now()) {
		return false, tx.Commit(ctx)
	}
	if e != nil {
		return false, e
	}
	callbackCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	e = apply(callbackCtx, m)
	cancel()
	if e != nil {
		_, ue := tx.Exec(ctx, `UPDATE outbox SET attempts=attempts+1,last_error='indexer callback failed',next_attempt=now()+least(interval '5 minutes', interval '1 second'*power(2,least(attempts+1,9))) WHERE memory_id=$1::uuid`, id)
		if ue != nil {
			return false, ue
		}
		return true, tx.Commit(ctx)
	}
	if kind == "delete" {
		_, e = tx.Exec(ctx, `DELETE FROM outbox WHERE memory_id=$1::uuid`, id)
	} else {
		_, e = tx.Exec(ctx, `UPDATE memories SET indexed_revision=revision WHERE id=$1::uuid`, id)
		if e == nil {
			_, e = tx.Exec(ctx, `DELETE FROM outbox WHERE memory_id=$1::uuid`, id)
		}
	}
	if e != nil {
		return false, e
	}
	return true, tx.Commit(ctx)
}
func (s *Store) getAnyTx(ctx context.Context, tx pgx.Tx, id string) (core.Memory, error) {
	return scanMemory(tx.QueryRow(ctx, `SELECT `+memCols+` FROM memories WHERE id=$1::uuid FOR UPDATE`, id))
}
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var x Stats
	e := s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE attempts>=0),count(*) FILTER (WHERE attempts>0) FROM outbox`).Scan(&x.PendingJobs, &x.FailedJobs)
	if e != nil {
		return x, e
	}
	e = s.DB.QueryRow(ctx, `SELECT count(*) FROM memories WHERE deleted=false`).Scan(&x.MemoryCount)
	return x, e
}
