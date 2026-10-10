package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pushpavel/fired/internal/store/storetest"
	"github.com/Pushpavel/fired/pkg/api"
)

func open(t *testing.T, url, host string, clock *time.Time) *Store {
	t.Helper()
	s, err := Open(context.Background(), url, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if clock != nil {
		s.now = func() time.Time { return *clock }
	}
	return s
}

func TestMigrations(t *testing.T) {
	url := storetest.URL(t)
	ctx := context.Background()
	a := open(t, url, "a", nil)
	open(t, url, "b", nil) // a second host: migrations already applied, no error

	// A schema from a future version that old code can still read is fine;
	// one that needs newer code refuses to start.
	if _, err := a.db.ExecContext(ctx, `UPDATE schema_meta SET version=$1, min_code=$2`, SchemaVersion+1, SchemaVersion); err != nil {
		t.Fatal(err)
	}
	open(t, url, "c", nil)
	if _, err := a.db.ExecContext(ctx, `UPDATE schema_meta SET min_code=$1`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, url, "d"); err == nil {
		t.Fatal("opened a database that needs newer code")
	}
}

func TestOwnership(t *testing.T) {
	url := storetest.URL(t)
	ctx := context.Background()
	a, b := open(t, url, "a", nil), open(t, url, "b", nil)
	sb := &api.Sandbox{ID: "sb-1", State: api.StateRunning, TemplateID: "t", Labels: map[string]string{"user": "u1", "x": "y"}}
	if err := a.InsertSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	if err := b.InsertSandbox(ctx, sb); err == nil {
		t.Fatal("ids must be unique across hosts")
	}
	if err := b.PutSandbox(ctx, sb); err == nil {
		t.Fatal("another host updated a sandbox it doesn't own")
	}
	got, err := b.GetSandbox(ctx, "sb-1")
	if err != nil || got.Host != "a" {
		t.Fatalf("get from another host: %+v %v", got, err)
	}
	if l, _ := b.ListSandboxes(ctx); len(l) != 0 {
		t.Fatalf("b's own list: %d", len(l))
	}
	if l, _ := b.ListAllSandboxes(ctx, map[string]string{"user": "u1"}, "running"); len(l) != 1 {
		t.Fatalf("cluster list by label: %d", len(l))
	}
	if l, _ := b.ListAllSandboxes(ctx, map[string]string{"user": "u2"}, ""); len(l) != 0 {
		t.Fatalf("label filter: %d", len(l))
	}
	if _, err := b.GetSandbox(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAdmission(t *testing.T) {
	url := storetest.URL(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	a, b := open(t, url, "a", &now), open(t, url, "b", &now)
	for _, s := range []*Store{a, b} {
		s.Defaults = map[string]api.Quota{"chat": {MaxMemoryMB: 3072, GBHoursPerDay: 4}}
		if err := s.Heartbeat(ctx, api.HostRecord{StartedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// tick moves the clock, with both hosts heartbeating as they would.
	tick := func(d time.Duration) {
		t.Helper()
		now = now.Add(d)
		for _, s := range []*Store{a, b} {
			if err := s.Heartbeat(ctx, api.HostRecord{StartedAt: now}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk := func(s *Store, id, key, pool string) {
		t.Helper()
		if err := s.InsertSandbox(ctx, &api.Sandbox{ID: id, State: api.StateCreating, TemplateID: "t", QuotaKey: key, Pool: pool}); err != nil {
			t.Fatal(err)
		}
	}
	quotaErr := func(err error) {
		t.Helper()
		var qe *QuotaError
		if !errors.As(err, &qe) {
			t.Fatalf("want a quota error, got %v", err)
		}
	}
	mk(a, "a1", "u1", "chat")
	mk(b, "b1", "u1", "chat")
	mk(b, "b2", "u1", "chat")
	mk(b, "x1", "u1", "artifacts")

	// Memory: 2 GB on host a plus 1 GB on host b fills the 3 GB cap.
	if err := a.Admit(ctx, "a1", "u1", "chat", 2048); err != nil {
		t.Fatal(err)
	}
	if err := b.Admit(ctx, "b1", "u1", "chat", 1024); err != nil {
		t.Fatal(err)
	}
	quotaErr(b.Admit(ctx, "b2", "u1", "chat", 512))
	// Pools are separate budgets (artifacts has no default: unlimited).
	if err := b.Admit(ctx, "x1", "u1", "artifacts", 8192); err != nil {
		t.Fatal(err)
	}
	// Releasing frees the memory and settles the time used.
	tick(30 * time.Minute)
	if err := b.Release(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Admit(ctx, "b2", "u1", "chat", 512); err != nil {
		t.Fatal(err)
	}
	st, err := a.QuotaStatus(ctx, "u1", "chat")
	if err != nil {
		t.Fatal(err)
	}
	// a1: 2 GB × 0.5 h = 1 (live), b1: 1 GB × 0.5 h = 0.5 (settled).
	if st.RunningMB != 2560 || st.GBHoursToday != 1.5 || st.Effective.MaxMemoryMB != 3072 {
		t.Fatalf("status: %+v", st)
	}

	// GB-hours: after 1.5 h more, a1 (2 GB) and b2 (0.5 GB) reach 1.5 + 3 + 0.75 = 5.25 > 4.
	tick(90 * time.Minute)
	if err := a.Accrue(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Release(ctx, "b2"); err != nil {
		t.Fatal(err)
	}
	mk(a, "a2", "u1", "chat")
	quotaErr(a.Admit(ctx, "a2", "u1", "chat", 128))
	// A per-key limit overrides the default; negative means none.
	if err := a.SetQuota(ctx, "u1", "chat", api.Quota{GBHoursPerDay: -1}); err != nil {
		t.Fatal(err)
	}
	if err := a.Admit(ctx, "a2", "u1", "chat", 128); err != nil {
		t.Fatal(err)
	}
	// Next day, usage starts over.
	if err := a.Release(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetQuota(ctx, "u1", "chat", api.Quota{}); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	tick(24 * time.Hour)
	if err := a.Admit(ctx, "a2", "u1", "chat", 128); err != nil {
		t.Fatalf("new day: %v", err)
	}

	// A host that stops heartbeating no longer holds its user's quota.
	mk(b, "b3", "u2", "chat")
	mk(a, "a3", "u2", "chat")
	if err := b.Admit(ctx, "b3", "u2", "chat", 3072); err != nil {
		t.Fatal(err)
	}
	quotaErr(a.Admit(ctx, "a3", "u2", "chat", 1024))
	now = now.Add(time.Minute)
	_ = a.Heartbeat(ctx, api.HostRecord{StartedAt: now}) // only a is alive
	if err := a.Admit(ctx, "a3", "u2", "chat", 1024); err != nil {
		t.Fatalf("dead host still counted: %v", err)
	}
	hs, _ := a.Hosts(ctx)
	if len(hs) != 2 || !hs[0].Alive || hs[1].Alive {
		t.Fatalf("hosts: %+v", hs)
	}
}
