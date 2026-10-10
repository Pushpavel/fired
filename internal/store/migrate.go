package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Schema changes follow expand/contract, because hosts of different
// versions share the database during a rolling upgrade:
//
//   - A migration only adds (tables, nullable or defaulted columns,
//     indexes). Older code ignores what it doesn't know.
//   - Dropping or renaming something happens in a later migration, once no
//     running version uses it. Such a migration raises minCode to the first
//     schema version whose code no longer needs the old shape, and older
//     hosts refuse to start against it instead of misreading data.
//
// Migrations run inside fired (on startup, or `fired migrate`) one host at a
// time, under a Postgres advisory lock.
type migration struct {
	sql string
	// minCode is the lowest code schema version that can run against the
	// database once this migration is applied.
	minCode int
}

var migrations = []migration{
	{minCode: 1, sql: `
CREATE TABLE hosts (
	id          TEXT PRIMARY KEY,
	url         TEXT NOT NULL,
	pools       JSONB NOT NULL DEFAULT '[]',
	version     TEXT NOT NULL DEFAULT '',
	schema      INT NOT NULL DEFAULT 0,
	capacity_mb INT NOT NULL DEFAULT 0,
	running_mb  INT NOT NULL DEFAULT 0,
	sandboxes   INT NOT NULL DEFAULT 0,
	started_at  TIMESTAMPTZ NOT NULL,
	last_seen   TIMESTAMPTZ NOT NULL
);
CREATE TABLE sandboxes (
	id          TEXT PRIMARY KEY,
	host_id     TEXT NOT NULL,
	state       TEXT NOT NULL,
	template_id TEXT NOT NULL,
	quota_key   TEXT NOT NULL DEFAULT '',
	pool        TEXT NOT NULL DEFAULT '',
	labels      JSONB NOT NULL DEFAULT '{}',
	data        JSONB NOT NULL,
	updated_at  TIMESTAMPTZ NOT NULL,
	-- memory counted against the quota key while running (0 = not running)
	run_mb      INT NOT NULL DEFAULT 0,
	-- running time up to here has been added to usage
	accrued_at  TIMESTAMPTZ
);
CREATE INDEX sandboxes_host ON sandboxes(host_id);
CREATE INDEX sandboxes_template ON sandboxes(template_id);
CREATE INDEX sandboxes_running ON sandboxes(quota_key, pool) WHERE run_mb > 0;
CREATE INDEX sandboxes_labels ON sandboxes USING GIN (labels);
CREATE TABLE templates (
	id         TEXT PRIMARY KEY,
	host_id    TEXT NOT NULL,
	name       TEXT NOT NULL,
	status     TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	data       JSONB NOT NULL
);
CREATE INDEX templates_name ON templates(name, host_id);
-- templates every host should build
CREATE TABLE template_specs (
	name       TEXT PRIMARY KEY,
	request    JSONB NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE quotas (
	quota_key        TEXT NOT NULL,
	pool             TEXT NOT NULL,
	max_memory_mb    INT NOT NULL DEFAULT 0,
	gb_hours_per_day DOUBLE PRECISION NOT NULL DEFAULT 0,
	PRIMARY KEY (quota_key, pool)
);
CREATE TABLE usage (
	quota_key  TEXT NOT NULL,
	pool       TEXT NOT NULL,
	day        DATE NOT NULL,
	mb_seconds BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (quota_key, pool, day)
);
`},
}

// SchemaVersion is the schema this code is built for.
var SchemaVersion = len(migrations)

// migrateLock is the advisory lock key ("fired" in ASCII).
const migrateLock = 0x6669726564

// Migrate brings the database at url up to SchemaVersion and closes it
// (`fired migrate`).
func Migrate(ctx context.Context, url string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	return migrate(ctx, db)
}

// Ping checks that the database at url is reachable, without migrating it.
func Ping(ctx context.Context, url string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

func migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLock); err != nil {
		return fmt.Errorf("lock schema: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLock)

	if _, err := conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_meta (
	id       INT PRIMARY KEY CHECK (id = 1),
	version  INT NOT NULL,
	min_code INT NOT NULL
);
INSERT INTO schema_meta(id, version, min_code) VALUES (1, 0, 0) ON CONFLICT DO NOTHING`); err != nil {
		return fmt.Errorf("schema_meta: %w", err)
	}
	var version, minCode int
	if err := conn.QueryRowContext(ctx, `SELECT version, min_code FROM schema_meta WHERE id=1`).Scan(&version, &minCode); err != nil {
		return err
	}
	if version > SchemaVersion {
		if minCode > SchemaVersion {
			return fmt.Errorf("database schema is v%d and needs fired built for schema v%d or newer; this build is v%d: upgrade fired",
				version, minCode, SchemaVersion)
		}
		return nil // newer, but still readable by this code
	}
	for v := version; v < SchemaVersion; v++ {
		m := migrations[v]
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_meta SET version=$1, min_code=GREATEST(min_code, $2) WHERE id=1`, v+1, m.minCode); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
