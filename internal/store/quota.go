package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
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

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ErrTierInUse is returned when deleting a tier that keys are mapped to.
var ErrTierInUse = errors.New("tier in use")

func keyTier(ctx context.Context, q querier, key string) (string, error) {
	var t string
	err := q.QueryRowContext(ctx, `SELECT tier FROM key_tiers WHERE quota_key=$1`, key).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return api.DefaultTier, nil
	}
	return t, err
}

// tierPool is a tier's limits in one pool (zero when either is missing).
func tierPool(ctx context.Context, q querier, tier, pool string) (api.Quota, error) {
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT pools FROM tiers WHERE name=$1`, tier).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Quota{}, nil
	}
	if err != nil {
		return api.Quota{}, err
	}
	var pools map[string]api.Quota
	if err := json.Unmarshal(b, &pools); err != nil {
		return api.Quota{}, err
	}
	return pools[pool], nil
}

// effective resolves a key's limits in a pool: its own override, else its
// tier's, else the default tier's. Anything ≤ 0 after that means no limit
// (reported as -1).
func effective(ctx context.Context, q querier, key, pool string, own api.Quota) (api.Quota, string, error) {
	tier, err := keyTier(ctx, q, key)
	if err != nil {
		return api.Quota{}, "", err
	}
	t, err := tierPool(ctx, q, tier, pool)
	if err != nil {
		return api.Quota{}, "", err
	}
	var d api.Quota
	if tier != api.DefaultTier {
		if d, err = tierPool(ctx, q, api.DefaultTier, pool); err != nil {
			return api.Quota{}, "", err
		}
	}
	eff := own
	if eff.MaxMemoryMB == 0 {
		eff.MaxMemoryMB = t.MaxMemoryMB
	}
	if eff.MaxMemoryMB == 0 {
		eff.MaxMemoryMB = d.MaxMemoryMB
	}
	if eff.GBHoursPerDay == 0 {
		eff.GBHoursPerDay = t.GBHoursPerDay
	}
	if eff.GBHoursPerDay == 0 {
		eff.GBHoursPerDay = d.GBHoursPerDay
	}
	if eff.MaxMemoryMB <= 0 {
		eff.MaxMemoryMB = -1
	}
	if eff.GBHoursPerDay <= 0 {
		eff.GBHoursPerDay = -1
	}
	return eff, tier, nil
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
	eff, _, err := effective(ctx, tx, key, pool, q)
	if err != nil {
		return err
	}
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
	if st.Effective, st.Tier, err = effective(ctx, s.db, key, pool, st.Quota); err != nil {
		return nil, err
	}
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
// its tier and the default tier define.
func (s *Store) QuotaPools(ctx context.Context, key string) ([]string, error) {
	tier, err := keyTier(ctx, s.db, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT pool FROM quotas WHERE quota_key=$1
UNION SELECT pool FROM usage WHERE quota_key=$1
UNION SELECT pool FROM sandboxes WHERE quota_key=$1
UNION SELECT jsonb_object_keys(pools) FROM tiers WHERE name IN ($2, $3)`, key, tier, api.DefaultTier)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// KeyTier is the tier a key is mapped to (the default tier if none).
func (s *Store) KeyTier(ctx context.Context, key string) (string, error) {
	return keyTier(ctx, s.db, key)
}

// SetKeyTier maps a key to a tier; "" or the default tier removes the
// mapping. The tier must exist (except the default one).
func (s *Store) SetKeyTier(ctx context.Context, key, tier string) error {
	if tier == "" || tier == api.DefaultTier {
		_, err := s.db.ExecContext(ctx, `DELETE FROM key_tiers WHERE quota_key=$1`, key)
		return err
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO key_tiers(quota_key, tier) SELECT $1, name FROM tiers WHERE name=$2
ON CONFLICT(quota_key) DO UPDATE SET tier=excluded.tier`, key, tier)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutTier creates or replaces a tier.
func (s *Store) PutTier(ctx context.Context, t api.Tier) error {
	if t.Pools == nil {
		t.Pools = map[string]api.Quota{}
	}
	b, err := json.Marshal(t.Pools)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO tiers(name, pools) VALUES($1,$2) ON CONFLICT(name) DO UPDATE SET pools=excluded.pools`, t.Name, b)
	return err
}

// Tiers lists every tier.
func (s *Store) Tiers(ctx context.Context) ([]api.Tier, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, pools FROM tiers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Tier{}
	for rows.Next() {
		var t api.Tier
		var b []byte
		if err := rows.Scan(&t.Name, &b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &t.Pools); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTier returns one tier.
func (s *Store) GetTier(ctx context.Context, name string) (*api.Tier, error) {
	ts, err := s.Tiers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ts {
		if ts[i].Name == name {
			return &ts[i], nil
		}
	}
	return nil, ErrNotFound
}

// DeleteTier removes a tier no key is mapped to (ErrTierInUse otherwise).
func (s *Store) DeleteTier(ctx context.Context, name string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_tiers WHERE tier=$1`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d quota keys are in tier %s", ErrTierInUse, n, name)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM tiers WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
