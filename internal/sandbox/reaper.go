package sandbox

import (
	"context"
	"sync"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
)

// effectiveTTL fills unset values from defaults; negative values disable.
func (m *Manager) effectiveTTL(t api.TTL) (idle, archive, del time.Duration) {
	pick := func(v, def api.Duration) time.Duration {
		switch {
		case v < 0:
			return 0
		case v == 0:
			if def < 0 {
				return 0
			}
			return def.D()
		default:
			return v.D()
		}
	}
	d := m.cfg.Defaults.TTL
	return pick(t.IdleSuspend, d.IdleSuspend), pick(t.ArchiveAfter, d.ArchiveAfter), pick(t.DeleteAfter, d.DeleteAfter)
}

func (m *Manager) reaper() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.ReaperInterval.D())
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.ReapOnce(m.ctx)
		}
	}
}

// ReapOnce evaluates TTLs for all sandboxes once (exported for tests).
func (m *Manager) ReapOnce(ctx context.Context) {
	m.mu.Lock()
	list := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e)
	}
	m.mu.Unlock()

	now := time.Now()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, e := range list {
		rec := e.snapshot()
		idle, archive, del := m.effectiveTTL(rec.TTL)
		last := time.Unix(0, e.lastActive.Load())
		var action func(context.Context, *entry) error
		switch {
		case del > 0 && now.Sub(last) > del && rec.State != api.StateCreating && rec.State != api.StateDeleting:
			action = func(ctx context.Context, e *entry) error { m.deleteLocked(e); return nil }
		case rec.State == api.StateRunning && idle > 0 && now.Sub(last) > idle && e.busy.Load() == 0:
			action = func(ctx context.Context, e *entry) error {
				err := m.suspendIf(ctx, e, func(x *entry) bool {
					return x.busy.Load() == 0 && time.Since(time.Unix(0, x.lastActive.Load())) > idle
				})
				if err == errSkipped {
					return nil
				}
				return err
			}
		case (rec.State == api.StateSuspended || rec.State == api.StateStopped) && archive > 0 && now.Sub(rec.StateChangedAt) > archive:
			action = func(ctx context.Context, e *entry) error {
				if s := e.snapshot().State; s != api.StateSuspended && s != api.StateStopped {
					return nil
				}
				return m.archiveLocked(ctx, e)
			}
		default:
			// Persist activity timestamps occasionally so they survive restarts.
			e.mu.Lock()
			stored := e.rec.LastActiveAt
			e.mu.Unlock()
			if last.Sub(stored) > time.Minute {
				m.persist(e)
			}
			continue
		}
		if !e.op.TryLock() {
			continue // busy with another transition; try next tick
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(e *entry, action func(context.Context, *entry) error) {
			defer wg.Done()
			defer func() { <-sem }()
			defer e.op.Unlock()
			if err := action(ctx, e); err != nil {
				m.log.Warn("reaper action failed", "sandbox", e.id, "err", err)
			}
		}(e, action)
	}
	wg.Wait()
	// Suspended sandboxes' snapshots add up: keep the hot tier's minimum
	// free (a burst of suspends is archived now, not at archive_after).
	m.makeHotRoom(ctx, nil, 0)()
}
