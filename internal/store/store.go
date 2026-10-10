// Package store persists sandbox, template, host and quota records in
// Postgres. Every fired host of a cluster uses the same database; each
// sandbox and template build row belongs to one host (host_id), and only
// that host changes it. The schema is fired's own: other services use the
// API, never these tables.
//
// Secrets are never written here: the exec path keeps them in memory only.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" database/sql driver

	"github.com/Pushpavel/fired/pkg/api"
)

var ErrNotFound = errors.New("not found")

// Store is a connection to the cluster database, scoped to one host: the
// write methods and the "local" listings act on that host's rows.
type Store struct {
	db   *sql.DB
	url  string
	host string
	// HostTimeout is how long a host may miss heartbeats before it counts
	// as down (its sandboxes stop counting against quotas).
	HostTimeout time.Duration
	// Defaults are the per-pool quota defaults (config `quotas`).
	Defaults map[string]api.Quota
	now      func() time.Time
}

// Open connects to url, migrates the schema (one host at a time, under an
// advisory lock) and returns a store for host.
func Open(ctx context.Context, url, host string) (*Store, error) {
	if host == "" {
		return nil, errors.New("store: host id is required")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetConnMaxIdleTime(5 * time.Minute)
	// The database may still be starting (compose, Kubernetes): retry for a
	// minute before giving up.
	deadline := time.Now().Add(time.Minute)
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = db.PingContext(pctx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			db.Close()
			return nil, fmt.Errorf("connect to database: %w", err)
		}
		time.Sleep(time.Second)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, url: url, host: host, HostTimeout: 30 * time.Second, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Host is the host this store writes as.
func (s *Store) Host() string { return s.host }

// DB exposes the connection pool (for LISTEN/NOTIFY and tests).
func (s *Store) DB() *sql.DB { return s.db }

// ---- sandboxes

const sandboxCols = `data, host_id`

func scanSandbox(sc interface{ Scan(...any) error }) (*api.Sandbox, error) {
	var data []byte
	var host string
	if err := sc.Scan(&data, &host); err != nil {
		return nil, err
	}
	var sb api.Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil, fmt.Errorf("decode sandbox: %w", err)
	}
	sb.Host = host
	return &sb, nil
}

func labelsJSON(l map[string]string) []byte {
	if l == nil {
		l = map[string]string{}
	}
	b, _ := json.Marshal(l)
	return b
}

// PutSandbox inserts or updates this host's sandbox record.
func (s *Store) PutSandbox(ctx context.Context, sb *api.Sandbox) error {
	b, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO sandboxes(id, host_id, state, template_id, quota_key, pool, labels, data, updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, template_id=excluded.template_id,
	quota_key=excluded.quota_key, pool=excluded.pool, labels=excluded.labels, data=excluded.data,
	updated_at=excluded.updated_at
WHERE sandboxes.host_id=excluded.host_id`,
		sb.ID, s.host, string(sb.State), sb.TemplateID, sb.QuotaKey, sb.Pool, labelsJSON(sb.Labels), b, s.now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sandbox %s belongs to another host", sb.ID)
	}
	return nil
}

// InsertSandbox fails if the id already exists on any host.
func (s *Store) InsertSandbox(ctx context.Context, sb *api.Sandbox) error {
	b, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO sandboxes(id, host_id, state, template_id, quota_key, pool, labels, data, updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		sb.ID, s.host, string(sb.State), sb.TemplateID, sb.QuotaKey, sb.Pool, labelsJSON(sb.Labels), b, s.now())
	return err
}

// GetSandbox reads a sandbox on any host.
func (s *Store) GetSandbox(ctx context.Context, id string) (*api.Sandbox, error) {
	sb, err := scanSandbox(s.db.QueryRowContext(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sb, err
}

// ListSandboxes returns this host's sandboxes.
func (s *Store) ListSandboxes(ctx context.Context) ([]*api.Sandbox, error) {
	return s.listSandboxes(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE host_id=$1`, s.host)
}

// ListAllSandboxes returns sandboxes on every host, optionally filtered by
// labels (all must match) and state.
func (s *Store) ListAllSandboxes(ctx context.Context, labels map[string]string, state string) ([]*api.Sandbox, error) {
	q := `SELECT ` + sandboxCols + ` FROM sandboxes WHERE labels @> $1`
	args := []any{labelsJSON(labels)}
	if state != "" {
		q += ` AND state=$2`
		args = append(args, state)
	}
	return s.listSandboxes(ctx, q, args...)
}

func (s *Store) listSandboxes(ctx context.Context, q string, args ...any) ([]*api.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, rows.Err()
}

// DeleteSandbox removes this host's record of a sandbox, settling any
// running time first.
func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	if err := s.Release(ctx, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id=$1 AND host_id=$2`, id, s.host)
	return err
}

func (s *Store) CountSandboxesByTemplate(ctx context.Context, templateID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sandboxes WHERE template_id=$1`, templateID).Scan(&n)
	return n, err
}

// ResetRunning marks every sandbox of this host as not running, after a
// restart (nothing is running yet). Time since the last accrual is lost.
func (s *Store) ResetRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sandboxes SET run_mb=0, accrued_at=NULL WHERE host_id=$1 AND run_mb>0`, s.host)
	return err
}

// ---- templates

func scanTemplate(sc interface{ Scan(...any) error }) (*api.Template, error) {
	var data []byte
	var host string
	if err := sc.Scan(&data, &host); err != nil {
		return nil, err
	}
	var t api.Template
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode template: %w", err)
	}
	t.Host = host
	return &t, nil
}

func (s *Store) PutTemplate(ctx context.Context, t *api.Template) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO templates(id, host_id, name, status, created_at, data) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, status=excluded.status, data=excluded.data
WHERE templates.host_id=excluded.host_id`,
		t.ID, s.host, t.Name, string(t.Status), t.CreatedAt, b)
	return err
}

// GetTemplate reads a build on any host.
func (s *Store) GetTemplate(ctx context.Context, id string) (*api.Template, error) {
	t, err := scanTemplate(s.db.QueryRowContext(ctx, `SELECT data, host_id FROM templates WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// LatestReadyTemplate returns this host's newest ready build of a name.
func (s *Store) LatestReadyTemplate(ctx context.Context, name string) (*api.Template, error) {
	t, err := scanTemplate(s.db.QueryRowContext(ctx, `
SELECT data, host_id FROM templates WHERE host_id=$1 AND name=$2 AND status=$3
ORDER BY created_at DESC LIMIT 1`, s.host, name, string(api.TemplateReady)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ListTemplates returns this host's builds.
func (s *Store) ListTemplates(ctx context.Context) ([]*api.Template, error) {
	return s.listTemplates(ctx, `SELECT data, host_id FROM templates WHERE host_id=$1 ORDER BY name, created_at DESC`, s.host)
}

// ListAllTemplates returns the builds on every host.
func (s *Store) ListAllTemplates(ctx context.Context) ([]*api.Template, error) {
	return s.listTemplates(ctx, `SELECT data, host_id FROM templates ORDER BY name, created_at DESC, host_id`)
}

func (s *Store) listTemplates(ctx context.Context, q string, args ...any) ([]*api.Template, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM templates WHERE id=$1 AND host_id=$2`, id, s.host)
	return err
}

// TemplateSpec is a template every host should build.
type TemplateSpec struct {
	Name      string
	Request   api.CreateTemplateRequest
	UpdatedAt time.Time
}

// PutTemplateSpec records (or replaces) the request every host builds.
func (s *Store) PutTemplateSpec(ctx context.Context, req api.CreateTemplateRequest, at time.Time) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO template_specs(name, request, updated_at) VALUES($1,$2,$3)
ON CONFLICT(name) DO UPDATE SET request=excluded.request, updated_at=excluded.updated_at`, req.Name, b, at)
	return err
}

func (s *Store) DeleteTemplateSpec(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM template_specs WHERE name=$1`, name)
	return err
}

func (s *Store) TemplateSpecs(ctx context.Context) ([]TemplateSpec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, request, updated_at FROM template_specs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateSpec
	for rows.Next() {
		var sp TemplateSpec
		var b []byte
		if err := rows.Scan(&sp.Name, &b, &sp.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &sp.Request); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// ---- hosts

// Heartbeat records this host as alive, with its current figures.
func (s *Store) Heartbeat(ctx context.Context, h api.HostRecord) error {
	pools, _ := json.Marshal(h.Pools)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO hosts(id, url, pools, version, schema, capacity_mb, running_mb, sandboxes, started_at, last_seen)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT(id) DO UPDATE SET url=excluded.url, pools=excluded.pools, version=excluded.version,
	schema=excluded.schema, capacity_mb=excluded.capacity_mb, running_mb=excluded.running_mb,
	sandboxes=excluded.sandboxes, started_at=excluded.started_at, last_seen=excluded.last_seen`,
		s.host, h.URL, pools, h.Version, SchemaVersion, h.CapacityMB, h.RunningMB, h.Sandboxes, h.StartedAt, s.now())
	return err
}

// Hosts lists every host that has ever heartbeated, with Alive set.
func (s *Store) Hosts(ctx context.Context) ([]api.HostRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, url, pools, version, schema, capacity_mb, running_mb, sandboxes, started_at, last_seen
FROM hosts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.HostRecord
	cutoff := s.now().Add(-s.HostTimeout)
	for rows.Next() {
		var h api.HostRecord
		var pools []byte
		if err := rows.Scan(&h.ID, &h.URL, &pools, &h.Version, &h.Schema, &h.CapacityMB, &h.RunningMB,
			&h.Sandboxes, &h.StartedAt, &h.LastSeen); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(pools, &h.Pools)
		h.Alive = h.LastSeen.After(cutoff)
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetHost returns one host record.
func (s *Store) GetHost(ctx context.Context, id string) (*api.HostRecord, error) {
	hs, err := s.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range hs {
		if hs[i].ID == id {
			return &hs[i], nil
		}
	}
	return nil, ErrNotFound
}

// MarkDown records this host as stopped (a clean shutdown), so others stop
// routing to it without waiting for the host timeout.
func (s *Store) MarkDown(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET last_seen=$2 WHERE id=$1`, s.host, time.Unix(0, 0).UTC())
	return err
}

// DeleteHost forgets a host (after it was removed for good). Its sandbox and
// template records stay until deleted.
func (s *Store) DeleteHost(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM hosts WHERE id=$1`, id)
	return err
}

// ---- notifications

// Notify sends a payload on a Postgres channel (delivered to every host
// that LISTENs, including this one).
func (s *Store) Notify(ctx context.Context, channel, payload string) error {
	_, err := s.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, channel, payload)
	return err
}
