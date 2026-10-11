package artifacts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// store persists artifacts, versions, file trees, jobs, runs and quotas in
// Postgres, in the schema "artifacts" of whatever database it's given
// (normally fired's, whose tables it never touches). File contents live in
// Blobs; rows only hold hashes.
type store struct {
	db  pg      // queries outside transactions
	raw *sql.DB // the pool, for transactions
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pg runs queries written with "?" placeholders (also built dynamically)
// as Postgres "$n" ones.
type pg struct{ q querier }

func (p pg) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return p.q.ExecContext(ctx, rebind(query), args...)
}
func (p pg) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return p.q.QueryContext(ctx, rebind(query), args...)
}
func (p pg) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return p.q.QueryRowContext(ctx, rebind(query), args...)
}

func rebind(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

var errNotFound = errors.New("not found")

// dbSchema is where this service's tables live.
const dbSchema = "artifacts"

func openStore(ctx context.Context, url string) (*store, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database_url: %w", err)
	}
	cfg.RuntimeParams["search_path"] = dbSchema
	raw := stdlib.OpenDB(*cfg)
	raw.SetMaxOpenConns(16)
	raw.SetConnMaxIdleTime(5 * time.Minute)
	// The database may still be starting (compose, Kubernetes): retry for a
	// minute before giving up.
	deadline := time.Now().Add(time.Minute)
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = raw.PingContext(pctx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			raw.Close()
			return nil, fmt.Errorf("connect to database: %w", err)
		}
		time.Sleep(time.Second)
	}
	if err := migrate(ctx, raw); err != nil {
		raw.Close()
		return nil, err
	}
	return &store{db: pg{raw}, raw: raw}, nil
}

// migrations are applied in order, once each, under an advisory lock.
// Nothing is deployed yet, so they can still be edited; once it is, only
// append.
var migrations = []string{`
CREATE TABLE artifacts (
	id              TEXT PRIMARY KEY,
	quota_key       TEXT NOT NULL,
	current_version INT NOT NULL DEFAULT 0,
	data            TEXT NOT NULL,
	created_at      BIGINT NOT NULL,
	updated_at      BIGINT NOT NULL,
	last_active_at  BIGINT NOT NULL
);
CREATE INDEX artifacts_quota ON artifacts(quota_key);
CREATE TABLE versions (
	artifact   TEXT NOT NULL,
	version    INT NOT NULL,
	origin     TEXT NOT NULL,
	data       TEXT NOT NULL,
	created_at BIGINT NOT NULL,
	PRIMARY KEY (artifact, version)
);
CREATE TABLE files (
	artifact TEXT NOT NULL,
	version  INT NOT NULL,
	path     TEXT NOT NULL,
	hash     TEXT NOT NULL,
	size     BIGINT NOT NULL,
	mode     INT NOT NULL,
	PRIMARY KEY (artifact, version, path)
);
CREATE INDEX files_hash ON files(hash);
CREATE TABLE jobs (
	artifact      TEXT NOT NULL,
	job           TEXT NOT NULL,
	armed_until   BIGINT NOT NULL DEFAULT 0,
	grant_until   BIGINT NOT NULL DEFAULT 0,
	grant_max_run BIGINT NOT NULL DEFAULT 0,
	sched_key     TEXT NOT NULL DEFAULT '',
	next_at       BIGINT NOT NULL DEFAULT 0,
	last_fire_at  BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (artifact, job)
);
CREATE TABLE runs (
	id          TEXT PRIMARY KEY,
	artifact    TEXT NOT NULL,
	job         TEXT NOT NULL,
	status      TEXT NOT NULL,
	trigger     TEXT NOT NULL,
	payload     BYTEA,
	logs        TEXT NOT NULL DEFAULT '',
	data        TEXT NOT NULL,
	created_at  BIGINT NOT NULL,
	started_at  BIGINT NOT NULL DEFAULT 0,
	finished_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX runs_status ON runs(status, created_at);
CREATE INDEX runs_artifact ON runs(artifact, created_at);
CREATE TABLE run_state (
	artifact   TEXT NOT NULL,
	job        TEXT NOT NULL,
	hash       TEXT NOT NULL,
	updated_at BIGINT NOT NULL,
	PRIMARY KEY (artifact, job)
);
CREATE TABLE notify (
	artifact TEXT NOT NULL,
	version  INT NOT NULL,
	target   TEXT NOT NULL,
	PRIMARY KEY (artifact, version)
);
CREATE TABLE quotas (
	key  TEXT PRIMARY KEY,
	data TEXT NOT NULL
);
CREATE TABLE tiers (
	name TEXT PRIMARY KEY,
	data TEXT NOT NULL
);
`}

// lockKey is the advisory lock for migrations and write transactions
// ("artifact" in ASCII).
const lockKey = 0x6172746966616374

func migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+dbSchema+`;
CREATE TABLE IF NOT EXISTS schema_version (version INT NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var v int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for ; v < len(migrations); v++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate to %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES($1)`, v+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) close() error { return s.raw.Close() }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// tx runs fn in a transaction. Write transactions take one advisory lock,
// so they run one at a time: read-check-write sequences (base_version,
// the next version number) stay correct without row locks.
func (s *store) tx(ctx context.Context, fn func(q querier) error) error {
	tx, err := s.raw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := fn(pg{tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- artifacts

type artifactData struct {
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Meta        json.RawMessage   `json:"meta,omitempty"`
	DeleteAfter pa.Duration       `json:"delete_after,omitempty"`
	RetainRuns  int               `json:"retain_runs,omitempty"`
	ForkedFrom  *pa.VersionRef    `json:"forked_from,omitempty"`
}

func putArtifact(ctx context.Context, q querier, a *pa.Artifact) error {
	d, _ := json.Marshal(artifactData{Title: a.Title, Description: a.Description, Labels: a.Labels, Meta: a.Meta, DeleteAfter: a.DeleteAfter, RetainRuns: a.RetainRuns, ForkedFrom: a.ForkedFrom})
	_, err := q.ExecContext(ctx, `
INSERT INTO artifacts(id, quota_key, current_version, data, created_at, updated_at, last_active_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET quota_key=excluded.quota_key, current_version=excluded.current_version,
	data=excluded.data, updated_at=excluded.updated_at, last_active_at=excluded.last_active_at`,
		a.ID, a.QuotaKey, a.CurrentVersion, string(d), ms(a.CreatedAt), ms(a.UpdatedAt), ms(a.LastActiveAt))
	return err
}

func scanArtifact(sc interface{ Scan(...any) error }) (*pa.Artifact, error) {
	var a pa.Artifact
	var data string
	var c, u, l int64
	if err := sc.Scan(&a.ID, &a.QuotaKey, &a.CurrentVersion, &data, &c, &u, &l); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	var d artifactData
	_ = json.Unmarshal([]byte(data), &d)
	a.Title, a.Description = d.Title, d.Description
	a.Labels, a.Meta, a.DeleteAfter, a.RetainRuns, a.ForkedFrom = d.Labels, d.Meta, d.DeleteAfter, d.RetainRuns, d.ForkedFrom
	a.CreatedAt, a.UpdatedAt, a.LastActiveAt = fromMS(c), fromMS(u), fromMS(l)
	return &a, nil
}

const artifactCols = `id, quota_key, current_version, data, created_at, updated_at, last_active_at`

func getArtifact(ctx context.Context, q querier, id string) (*pa.Artifact, error) {
	return scanArtifact(q.QueryRowContext(ctx, `SELECT `+artifactCols+` FROM artifacts WHERE id=?`, id))
}

func listArtifacts(ctx context.Context, q querier, quotaKey string) ([]pa.Artifact, error) {
	query := `SELECT ` + artifactCols + ` FROM artifacts`
	var args []any
	if quotaKey != "" {
		query += ` WHERE quota_key=?`
		args = append(args, quotaKey)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pa.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func touchArtifact(ctx context.Context, q querier, id string, t time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE artifacts SET last_active_at=? WHERE id=? AND last_active_at<?`, ms(t), id, ms(t))
	return err
}

func deleteArtifactRows(ctx context.Context, q querier, id string) error {
	for _, t := range []string{"files", "versions", "jobs", "runs", "run_state", "notify"} {
		if _, err := q.ExecContext(ctx, `DELETE FROM `+t+` WHERE artifact=?`, id); err != nil {
			return err
		}
	}
	_, err := q.ExecContext(ctx, `DELETE FROM artifacts WHERE id=?`, id)
	return err
}

// ---- versions

type versionData struct {
	Parent    int               `json:"parent,omitempty"`
	Label     string            `json:"label,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	RunID     string            `json:"run_id,omitempty"`
	Job       string            `json:"job,omitempty"`
	FileCount int               `json:"file_count"`
	SizeBytes int64             `json:"size_bytes"`
	Spec      pa.Spec           `json:"spec"`
}

func insertVersion(ctx context.Context, q querier, v *pa.Version, tree map[string]pa.File) error {
	d, _ := json.Marshal(versionData{Parent: v.Parent, Label: v.Label, Labels: v.Labels, RunID: v.RunID, Job: v.Job,
		FileCount: v.FileCount, SizeBytes: v.SizeBytes, Spec: v.Spec})
	if _, err := q.ExecContext(ctx, `INSERT INTO versions(artifact, version, origin, data, created_at) VALUES(?,?,?,?,?)`,
		v.Artifact, v.Version, v.Origin, string(d), ms(v.CreatedAt)); err != nil {
		return err
	}
	for _, f := range tree {
		if _, err := q.ExecContext(ctx, `INSERT INTO files(artifact, version, path, hash, size, mode) VALUES(?,?,?,?,?,?)`,
			v.Artifact, v.Version, f.Path, f.Hash, f.Size, f.Mode); err != nil {
			return err
		}
	}
	return nil
}

func scanVersion(sc interface{ Scan(...any) error }) (*pa.Version, error) {
	var v pa.Version
	var data string
	var c int64
	if err := sc.Scan(&v.Artifact, &v.Version, &v.Origin, &data, &c); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	var d versionData
	_ = json.Unmarshal([]byte(data), &d)
	v.Parent, v.Label, v.Labels, v.RunID, v.Job = d.Parent, d.Label, d.Labels, d.RunID, d.Job
	v.FileCount, v.SizeBytes, v.Spec = d.FileCount, d.SizeBytes, d.Spec
	v.CreatedAt = fromMS(c)
	return &v, nil
}

func getVersion(ctx context.Context, q querier, id string, n int) (*pa.Version, error) {
	return scanVersion(q.QueryRowContext(ctx, `SELECT artifact, version, origin, data, created_at FROM versions WHERE artifact=? AND version=?`, id, n))
}

func listVersions(ctx context.Context, q querier, id string) ([]pa.Version, error) {
	rows, err := q.QueryContext(ctx, `SELECT artifact, version, origin, data, created_at FROM versions WHERE artifact=? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pa.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

func maxVersion(ctx context.Context, q querier, id string) (int, error) {
	var n sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM versions WHERE artifact=?`, id).Scan(&n)
	return int(n.Int64), err
}

func getTree(ctx context.Context, q querier, id string, n int) (map[string]pa.File, error) {
	rows, err := q.QueryContext(ctx, `SELECT path, hash, size, mode FROM files WHERE artifact=? AND version=?`, id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tree := map[string]pa.File{}
	for rows.Next() {
		var f pa.File
		if err := rows.Scan(&f.Path, &f.Hash, &f.Size, &f.Mode); err != nil {
			return nil, err
		}
		tree[f.Path] = f
	}
	return tree, rows.Err()
}

func getFile(ctx context.Context, q querier, id string, n int, p string) (*pa.File, error) {
	var f pa.File
	err := q.QueryRowContext(ctx, `SELECT path, hash, size, mode FROM files WHERE artifact=? AND version=? AND path=?`, id, n, p).
		Scan(&f.Path, &f.Hash, &f.Size, &f.Mode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return &f, err
}

func deleteVersion(ctx context.Context, q querier, id string, n int) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM files WHERE artifact=? AND version=?`, id, n); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM notify WHERE artifact=? AND version=?`, id, n); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `DELETE FROM versions WHERE artifact=? AND version=?`, id, n)
	return err
}

// storageUsed is the size of the distinct blobs referenced by all versions
// of all artifacts with this quota key (deduplicated, like the store).
func storageUsed(ctx context.Context, q querier, key string) (int64, error) {
	var n sql.NullInt64
	err := q.QueryRowContext(ctx, `
SELECT SUM(size)::BIGINT FROM (SELECT DISTINCT f.hash, f.size FROM files f JOIN artifacts a ON a.id=f.artifact WHERE a.quota_key=?)`, key).Scan(&n)
	return n.Int64, err
}

// knownHashes returns which of hashes are already referenced under key.
func knownHashes(ctx context.Context, q querier, key string, hashes map[string]int64) (map[string]bool, error) {
	out := map[string]bool{}
	for h := range hashes {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM files f JOIN artifacts a ON a.id=f.artifact WHERE a.quota_key=? AND f.hash=? LIMIT 1`, key, h).Scan(&one)
		if err == nil {
			out[h] = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return out, nil
}

// ---- notify

func setNotify(ctx context.Context, q querier, id string, n int, target string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO notify(artifact, version, target) VALUES(?,?,?) ON CONFLICT(artifact, version) DO UPDATE SET target=excluded.target`, id, n, target)
	return err
}

// takeNotify returns and clears the notify target of a version.
func takeNotify(ctx context.Context, q querier, id string, n int) (string, error) {
	var t string
	err := q.QueryRowContext(ctx, `SELECT target FROM notify WHERE artifact=? AND version=?`, id, n).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = q.ExecContext(ctx, `DELETE FROM notify WHERE artifact=? AND version=?`, id, n)
	return t, err
}

// ---- jobs

type jobRow struct {
	Artifact    string
	Job         string
	ArmedUntil  time.Time
	GrantUntil  time.Time
	GrantMaxRun time.Duration
	SchedKey    string
	NextAt      time.Time
	LastFireAt  time.Time
}

func (j jobRow) activeUntil() time.Time {
	if j.GrantUntil.After(j.ArmedUntil) {
		return j.GrantUntil
	}
	return j.ArmedUntil
}

func getJob(ctx context.Context, q querier, id, job string) (jobRow, error) {
	r := jobRow{Artifact: id, Job: job}
	var a, g, gm, n, l int64
	err := q.QueryRowContext(ctx, `SELECT armed_until, grant_until, grant_max_run, sched_key, next_at, last_fire_at FROM jobs WHERE artifact=? AND job=?`, id, job).
		Scan(&a, &g, &gm, &r.SchedKey, &n, &l)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.ArmedUntil, r.GrantUntil, r.GrantMaxRun, r.NextAt, r.LastFireAt = fromMS(a), fromMS(g), time.Duration(gm), fromMS(n), fromMS(l)
	return r, nil
}

func putJob(ctx context.Context, q querier, r jobRow) error {
	_, err := q.ExecContext(ctx, `
INSERT INTO jobs(artifact, job, armed_until, grant_until, grant_max_run, sched_key, next_at, last_fire_at) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(artifact, job) DO UPDATE SET armed_until=excluded.armed_until, grant_until=excluded.grant_until,
	grant_max_run=excluded.grant_max_run, sched_key=excluded.sched_key, next_at=excluded.next_at, last_fire_at=excluded.last_fire_at`,
		r.Artifact, r.Job, ms(r.ArmedUntil), ms(r.GrantUntil), int64(r.GrantMaxRun), r.SchedKey, ms(r.NextAt), ms(r.LastFireAt))
	return err
}

// ---- runs

type runData struct {
	Version       int    `json:"version,omitempty"`
	ResultVersion int    `json:"result_version,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	Reason        string `json:"reason,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
}

type runRow struct {
	pa.Run
	Payload []byte
	Logs    string
}

func putRun(ctx context.Context, q querier, r *runRow) error {
	d, _ := json.Marshal(runData{Version: r.Version, ResultVersion: r.ResultVersion, ExitCode: r.ExitCode, Reason: r.Reason, DurationMS: r.DurationMS})
	var started, finished int64
	if r.StartedAt != nil {
		started = ms(*r.StartedAt)
	}
	if r.FinishedAt != nil {
		finished = ms(*r.FinishedAt)
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO runs(id, artifact, job, status, trigger, payload, logs, data, created_at, started_at, finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET status=excluded.status, payload=excluded.payload, logs=excluded.logs, data=excluded.data,
	started_at=excluded.started_at, finished_at=excluded.finished_at`,
		r.ID, r.Artifact, r.Job, r.Status, r.Trigger, r.Payload, r.Logs, string(d), ms(r.CreatedAt), started, finished)
	return err
}

const runCols = `id, artifact, job, status, trigger, payload, logs, data, created_at, started_at, finished_at`

func scanRun(sc interface{ Scan(...any) error }) (*runRow, error) {
	var r runRow
	var data string
	var c, st, fi int64
	if err := sc.Scan(&r.ID, &r.Artifact, &r.Job, &r.Status, &r.Trigger, &r.Payload, &r.Logs, &data, &c, &st, &fi); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	var d runData
	_ = json.Unmarshal([]byte(data), &d)
	r.Version, r.ResultVersion, r.ExitCode, r.Reason, r.DurationMS = d.Version, d.ResultVersion, d.ExitCode, d.Reason, d.DurationMS
	r.CreatedAt = fromMS(c)
	if st != 0 {
		t := fromMS(st)
		r.StartedAt = &t
	}
	if fi != 0 {
		t := fromMS(fi)
		r.FinishedAt = &t
	}
	return &r, nil
}

func getRun(ctx context.Context, q querier, id string) (*runRow, error) {
	return scanRun(q.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id=?`, id))
}

func queryRuns(ctx context.Context, q querier, where string, args ...any) ([]*runRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*runRow
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- run state

func getRunState(ctx context.Context, q querier, id, job string) (string, error) {
	var h string
	err := q.QueryRowContext(ctx, `SELECT hash FROM run_state WHERE artifact=? AND job=?`, id, job).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

func putRunState(ctx context.Context, q querier, id, job, hash string, t time.Time) error {
	if hash == "" {
		_, err := q.ExecContext(ctx, `DELETE FROM run_state WHERE artifact=? AND job=?`, id, job)
		return err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO run_state(artifact, job, hash, updated_at) VALUES(?,?,?,?) ON CONFLICT(artifact, job) DO UPDATE SET hash=excluded.hash, updated_at=excluded.updated_at`, id, job, hash, ms(t))
	return err
}

// ---- quotas

func getQuota(ctx context.Context, q querier, key string) (pa.Quota, error) {
	var data string
	var out pa.Quota
	err := q.QueryRowContext(ctx, `SELECT data FROM quotas WHERE key=?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal([]byte(data), &out)
	return out, nil
}

func putQuota(ctx context.Context, q querier, key string, v pa.Quota) error {
	d, _ := json.Marshal(v)
	_, err := q.ExecContext(ctx, `INSERT INTO quotas(key, data) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data`, key, string(d))
	return err
}

func getTier(ctx context.Context, q querier, name string) (pa.Quota, bool, error) {
	var data string
	var out pa.Quota
	err := q.QueryRowContext(ctx, `SELECT data FROM tiers WHERE name=?`, name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	_ = json.Unmarshal([]byte(data), &out)
	return out, true, nil
}

func putTier(ctx context.Context, q querier, t pa.Tier) error {
	d, _ := json.Marshal(t.Limits)
	_, err := q.ExecContext(ctx, `INSERT INTO tiers(name, data) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET data=excluded.data`, t.Name, string(d))
	return err
}

func listTiers(ctx context.Context, q querier) ([]pa.Tier, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, data FROM tiers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pa.Tier{}
	for rows.Next() {
		var t pa.Tier
		var data string
		if err := rows.Scan(&t.Name, &data); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(data), &t.Limits)
		out = append(out, t)
	}
	return out, rows.Err()
}

func deleteTier(ctx context.Context, q querier, name string) (bool, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM tiers WHERE name=?`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- garbage collection

// referencedBlobs returns every hash still in use.
func referencedBlobs(ctx context.Context, q querier) (map[string]bool, error) {
	out := map[string]bool{}
	for _, query := range []string{
		`SELECT DISTINCT hash FROM files`,
		`SELECT DISTINCT hash FROM run_state`,
		`SELECT DISTINCT logs FROM runs WHERE logs<>''`,
	} {
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return nil, err
			}
			out[h] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
