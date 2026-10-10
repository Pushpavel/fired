package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
)

// Usage is counted in MB-seconds (memory × running time) and reported in
// GB-hours: 2 GB running for 2 h is 4 GB-hours.
const mbSecondsPerGBHour = 1024 * 3600

// QuotaError is a start refused because of the quota key's limits.
type QuotaError struct {
	QuotaKey, Pool string
	Msg            string
}

func (e *QuotaError) Error() string { return e.Msg }

// effective fills a key's quota from the pool's default: 0 takes the
// default, and anything ≤ 0 after that means no limit (reported as -1).
func (s *Store) effective(pool string, q api.Quota) api.Quota {
	d := s.Defaults[pool]
	if q.MaxMemoryMB == 0 {
		q.MaxMemoryMB = d.MaxMemoryMB
	}
	if q.GBHoursPerDay == 0 {
		q.GBHoursPerDay = d.GBHoursPerDay
	}
	if q.MaxMemoryMB <= 0 {
		q.MaxMemoryMB = -1
	}
	if q.GBHoursPerDay <= 0 {
		q.GBHoursPerDay = -1
	}
	return q
}

func day(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }

// Admit lets sandbox id start with mb of memory, charged to (key, pool),
// or returns a *QuotaError. The check and the "now running" mark happen in
// one transaction holding the key's quota row, so starts on different hosts
// can't both slip under a limit. Sandboxes on hosts that stopped
// heartbeating don't count.
func (s *Store) Admit(ctx context.Context, id, key, pool string, mb int) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO quotas(quota_key, pool) VALUES($1,$2) ON CONFLICT DO NOTHING`, key, pool); err != nil {
		return err
	}
	var q api.Quota
	if err := tx.QueryRowContext(ctx, `SELECT max_memory_mb, gb_hours_per_day FROM quotas WHERE quota_key=$1 AND pool=$2 FOR UPDATE`,
		key, pool).Scan(&q.MaxMemoryMB, &q.GBHoursPerDay); err != nil {
		return err
	}
	eff := s.effective(pool, q)
	cutoff := now.Add(-s.HostTimeout)
	if eff.MaxMemoryMB > 0 {
		var running int
		if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(SUM(sb.run_mb), 0) FROM sandboxes sb JOIN hosts h ON h.id = sb.host_id
WHERE sb.quota_key=$1 AND sb.pool=$2 AND sb.run_mb > 0 AND sb.id <> $3 AND h.last_seen > $4`,
			key, pool, id, cutoff).Scan(&running); err != nil {
			return err
		}
		if running+mb > eff.MaxMemoryMB {
			return &QuotaError{key, pool, fmt.Sprintf("%s has %d MB running in pool %s; starting %d MB more would exceed its %d MB limit",
				key, running, pool, mb, eff.MaxMemoryMB)}
		}
	}
	if eff.GBHoursPerDay > 0 {
		used, err := usedToday(ctx, tx, key, pool, now, cutoff)
		if err != nil {
			return err
		}
		if used >= eff.GBHoursPerDay {
			return &QuotaError{key, pool, fmt.Sprintf("%s has used %.2f of its %.2f GB-hours today in pool %s",
				key, used, eff.GBHoursPerDay, pool)}
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE sandboxes SET run_mb=$2, accrued_at=$3 WHERE id=$1 AND host_id=$4`, id, mb, now, s.host)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("admit %s: no such sandbox on this host", id)
	}
	return tx.Commit()
}

// usedToday is today's settled usage plus what running sandboxes have used
// since their last accrual, in GB-hours.
func usedToday(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key, pool string, now, cutoff time.Time) (float64, error) {
	var settled int64
	err := q.QueryRowContext(ctx, `SELECT mb_seconds FROM usage WHERE quota_key=$1 AND pool=$2 AND day=$3`, key, pool, day(now)).Scan(&settled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var live float64
	if err := q.QueryRowContext(ctx, `
SELECT COALESCE(SUM(sb.run_mb * EXTRACT(EPOCH FROM ($3::timestamptz - GREATEST(sb.accrued_at, $4::timestamptz)))), 0)
FROM sandboxes sb JOIN hosts h ON h.id = sb.host_id
WHERE sb.quota_key=$1 AND sb.pool=$2 AND sb.run_mb > 0 AND h.last_seen > $5`,
		key, pool, now, day(now), cutoff).Scan(&live); err != nil {
		return 0, err
	}
	return (float64(settled) + math.Max(live, 0)) / mbSecondsPerGBHour, nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func addUsage(ctx context.Context, x execer, key, pool string, at time.Time, mbSeconds int64) error {
	if key == "" || mbSeconds <= 0 {
		return nil
	}
	_, err := x.ExecContext(ctx, `
INSERT INTO usage(quota_key, pool, day, mb_seconds) VALUES($1,$2,$3,$4)
ON CONFLICT(quota_key, pool, day) DO UPDATE SET mb_seconds = usage.mb_seconds + excluded.mb_seconds`,
		key, pool, day(at), mbSeconds)
	return err
}

// Release marks a sandbox as no longer running and settles its running
// time into usage.
func (s *Store) Release(ctx context.Context, id string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var key, pool string
	var mb int
	var accrued sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT quota_key, pool, run_mb, accrued_at FROM sandboxes WHERE id=$1 AND host_id=$2 FOR UPDATE`,
		id, s.host).Scan(&key, &pool, &mb, &accrued)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && mb == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	if accrued.Valid {
		if err := addUsage(ctx, tx, key, pool, now, int64(float64(mb)*now.Sub(accrued.Time).Seconds())); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sandboxes SET run_mb=0, accrued_at=NULL WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Accrue settles the running time of this host's running sandboxes into
// today's usage. Hosts call it every minute, so usage lands on the right
// day and stops growing when a host dies.
func (s *Store) Accrue(ctx context.Context) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT id, quota_key, pool, run_mb, accrued_at FROM sandboxes
WHERE host_id=$1 AND run_mb > 0 AND accrued_at IS NOT NULL FOR UPDATE`, s.host)
	if err != nil {
		return err
	}
	type row struct {
		id, key, pool string
		mb            int
		at            time.Time
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key, &r.pool, &r.mb, &r.at); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	for _, r := range list {
		if err := addUsage(ctx, tx, r.key, r.pool, now, int64(float64(r.mb)*now.Sub(r.at).Seconds())); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandboxes SET accrued_at=$2 WHERE id=$1`, r.id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetQuota sets a key's limits in a pool (0 = pool default, negative = no
// limit).
func (s *Store) SetQuota(ctx context.Context, key, pool string, q api.Quota) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO quotas(quota_key, pool, max_memory_mb, gb_hours_per_day) VALUES($1,$2,$3,$4)
ON CONFLICT(quota_key, pool) DO UPDATE SET max_memory_mb=excluded.max_memory_mb, gb_hours_per_day=excluded.gb_hours_per_day`,
		key, pool, q.MaxMemoryMB, q.GBHoursPerDay)
	return err
}

// QuotaStatus reports a key's limits and usage in a pool.
func (s *Store) QuotaStatus(ctx context.Context, key, pool string) (*api.QuotaStatus, error) {
	now := s.now()
	cutoff := now.Add(-s.HostTimeout)
	st := &api.QuotaStatus{QuotaKey: key, Pool: pool}
	err := s.db.QueryRowContext(ctx, `SELECT max_memory_mb, gb_hours_per_day FROM quotas WHERE quota_key=$1 AND pool=$2`,
		key, pool).Scan(&st.Quota.MaxMemoryMB, &st.Quota.GBHoursPerDay)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	st.Effective = s.effective(pool, st.Quota)
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(sb.run_mb), 0) FROM sandboxes sb JOIN hosts h ON h.id = sb.host_id
WHERE sb.quota_key=$1 AND sb.pool=$2 AND sb.run_mb > 0 AND h.last_seen > $3`, key, pool, cutoff).Scan(&st.RunningMB); err != nil {
		return nil, err
	}
	if st.GBHoursToday, err = usedToday(ctx, s.db, key, pool, now, cutoff); err != nil {
		return nil, err
	}
	st.GBHoursToday = math.Round(st.GBHoursToday*1000) / 1000
	return st, nil
}

// QuotaPools lists the pools a key has limits or usage in, plus every pool
// with configured defaults.
func (s *Store) QuotaPools(ctx context.Context, key string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT pool FROM quotas WHERE quota_key=$1
UNION SELECT pool FROM usage WHERE quota_key=$1
UNION SELECT pool FROM sandboxes WHERE quota_key=$1`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for p := range s.Defaults {
		if !seen[p] {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}
