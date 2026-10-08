// Package store persists sandbox and template records in SQLite.
//
// Secrets are never written here: the exec path keeps them in memory only.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Pushpavel/fired/pkg/api"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(dir string) (*Store, error) {
	dsn := "file:" + filepath.Join(dir, "fired.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS sandboxes (
	id          TEXT PRIMARY KEY,
	state       TEXT NOT NULL,
	template_id TEXT NOT NULL,
	data        TEXT NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sandboxes_template ON sandboxes(template_id);
CREATE TABLE IF NOT EXISTS templates (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	status     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	data       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS templates_name ON templates(name);
`)
	return err
}

// ---- sandboxes ----

func (s *Store) PutSandbox(ctx context.Context, sb *api.Sandbox) error {
	b, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO sandboxes(id, state, template_id, data, updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, template_id=excluded.template_id,
	data=excluded.data, updated_at=excluded.updated_at`,
		sb.ID, string(sb.State), sb.TemplateID, string(b), time.Now().UnixMilli())
	return err
}

// InsertSandbox fails if the id already exists.
func (s *Store) InsertSandbox(ctx context.Context, sb *api.Sandbox) error {
	b, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sandboxes(id, state, template_id, data, updated_at) VALUES(?,?,?,?,?)`,
		sb.ID, string(sb.State), sb.TemplateID, string(b), time.Now().UnixMilli())
	return err
}

func (s *Store) GetSandbox(ctx context.Context, id string) (*api.Sandbox, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM sandboxes WHERE id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var sb api.Sandbox
	if err := json.Unmarshal([]byte(data), &sb); err != nil {
		return nil, fmt.Errorf("decode sandbox %s: %w", id, err)
	}
	return &sb, nil
}

func (s *Store) ListSandboxes(ctx context.Context) ([]*api.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM sandboxes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Sandbox
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var sb api.Sandbox
		if err := json.Unmarshal([]byte(data), &sb); err != nil {
			return nil, err
		}
		out = append(out, &sb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, rows.Err()
}

func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id=?`, id)
	return err
}

func (s *Store) CountSandboxesByTemplate(ctx context.Context, templateID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sandboxes WHERE template_id=?`, templateID).Scan(&n)
	return n, err
}

// ---- templates ----

func (s *Store) PutTemplate(ctx context.Context, t *api.Template) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO templates(id, name, status, created_at, data) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, status=excluded.status, data=excluded.data`,
		t.ID, t.Name, string(t.Status), t.CreatedAt.UnixMilli(), string(b))
	return err
}

func (s *Store) GetTemplate(ctx context.Context, id string) (*api.Template, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM templates WHERE id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t api.Template
	return &t, json.Unmarshal([]byte(data), &t)
}

// LatestReadyTemplate returns the newest ready build of a template name.
func (s *Store) LatestReadyTemplate(ctx context.Context, name string) (*api.Template, error) {
	var data string
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM templates WHERE name=? AND status=? ORDER BY created_at DESC LIMIT 1`,
		name, string(api.TemplateReady)).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t api.Template
	return &t, json.Unmarshal([]byte(data), &t)
}

func (s *Store) ListTemplates(ctx context.Context) ([]*api.Template, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM templates ORDER BY name, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Template
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var t api.Template
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM templates WHERE id=?`, id)
	return err
}
