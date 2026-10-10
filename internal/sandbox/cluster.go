package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/pkg/api"
)

// The cluster: every fired host shares one Postgres database. Each sandbox
// and template build belongs to one host (the one that runs it); any host
// answers for all of them through the API (the server forwards per-sandbox
// requests to the owner). This file keeps the shared view current:
// heartbeats, usage accrual, events between hosts, placement and the
// templates every host builds.

// eventsChannel is the Postgres channel events travel between hosts on.
const eventsChannel = "fired_events"

const (
	accrueInterval    = time.Minute
	templateReconcile = 30 * time.Second
)

func (m *Manager) heartbeat(ctx context.Context) error {
	m.mu.Lock()
	running, n := m.runningMem, len(m.entries)
	m.mu.Unlock()
	return m.store.Heartbeat(ctx, api.HostRecord{
		URL: m.cfg.Cluster.AdvertiseURL, Pools: m.cfg.Cluster.Pools, Version: m.version,
		CapacityMB: m.cfg.Capacity.MaxRunningMemoryMB, RunningMB: running, Sandboxes: n, StartedAt: m.startedAt,
	})
}

func (m *Manager) clusterLoop() {
	defer m.wg.Done()
	hb := time.NewTicker(m.cfg.Cluster.Heartbeat.D())
	defer hb.Stop()
	acc := time.NewTicker(accrueInterval)
	defer acc.Stop()
	tpl := time.NewTicker(templateReconcile)
	defer tpl.Stop()
	m.reconcileTemplates(m.ctx)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-hb.C:
			if err := m.heartbeat(m.ctx); err != nil && m.ctx.Err() == nil {
				m.log.Error("heartbeat", "err", err)
			}
		case <-acc.C:
			if err := m.store.Accrue(m.ctx); err != nil && m.ctx.Err() == nil {
				m.log.Error("accrue usage", "err", err)
			}
		case <-tpl.C:
			m.reconcileTemplates(m.ctx)
		}
	}
}

// relayEvents sends this host's events to the other hosts.
func (m *Manager) relayEvents() {
	defer m.wg.Done()
	ch, cancel := m.bus.Subscribe()
	defer cancel()
	for {
		select {
		case <-m.ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Host != m.host {
				continue // came from another host
			}
			b, _ := json.Marshal(e)
			if err := m.store.Notify(m.ctx, eventsChannel, string(b)); err != nil && m.ctx.Err() == nil {
				m.log.Warn("relay event", "err", err)
			}
		}
	}
}

// receiveEvent publishes another host's event to this host's subscribers.
func (m *Manager) receiveEvent(payload string) {
	var e api.Event
	if err := json.Unmarshal([]byte(payload), &e); err != nil || e.Host == m.host {
		return
	}
	m.bus.Publish(e)
	if e.Type == "template.spec" {
		m.reconcileTemplates(m.ctx)
	}
}

// Hosts lists the cluster's hosts.
func (m *Manager) Hosts(ctx context.Context) ([]api.HostRecord, error) {
	return m.store.Hosts(ctx)
}

// Owner is the host a sandbox lives on, or ErrNotFound.
func (m *Manager) Owner(ctx context.Context, id string) (*api.Sandbox, *api.HostRecord, error) {
	sb, err := m.store.GetSandbox(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	h, err := m.store.GetHost(ctx, sb.Host)
	if errors.Is(err, store.ErrNotFound) {
		return sb, nil, errf(503, "host_unavailable", "sandbox %s is on host %s, which is not registered", id, sb.Host)
	}
	if err != nil {
		return nil, nil, err
	}
	if !h.Alive {
		sb.HostDown = true
	}
	return sb, h, nil
}

// ListAll returns sandboxes on every host. This host's come from memory
// (freshest); the others' are the state their host last recorded, with
// HostDown set when that host stopped heartbeating.
func (m *Manager) ListAll(ctx context.Context, labels map[string]string, state string) ([]api.Sandbox, error) {
	recs, err := m.store.ListAllSandboxes(ctx, labels, state)
	if err != nil {
		return nil, err
	}
	hosts, err := m.store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	alive := map[string]bool{}
	for _, h := range hosts {
		alive[h.ID] = h.Alive
	}
	out := []api.Sandbox{}
	for _, r := range recs {
		if r.Host == m.host {
			continue
		}
		r.HostDown = !alive[r.Host]
		out = append(out, *r)
	}
	out = append(out, m.List(labels, state)...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func servesPool(pools []string, pool string) bool {
	for _, p := range pools {
		if p == "*" || p == pool {
			return true
		}
	}
	return false
}

// Place picks the host a new sandbox should run on: a live host that runs
// its pool and has its template ready, with the most free memory (this host
// on a tie, to save a hop). A template given by build ID pins the host.
func (m *Manager) Place(ctx context.Context, req api.CreateSandboxRequest) (*api.HostRecord, error) {
	pool := req.Pool
	if pool == "" {
		pool = m.cfg.Cluster.DefaultPool
	}
	ref := req.Template
	if ref == "" {
		ref = m.cfg.Defaults.Template
	}
	if ref == "" {
		return nil, badRequest("template is required (no defaults.template configured)")
	}
	hosts, err := m.store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	tpls, err := m.store.ListAllTemplates(ctx)
	if err != nil {
		return nil, err
	}
	ready := map[string]*api.Template{} // host → newest ready build of ref
	for _, t := range tpls {
		if t.Status != api.TemplateReady || (t.ID != ref && t.Name != ref) {
			continue
		}
		if cur := ready[t.Host]; cur == nil || t.CreatedAt.After(cur.CreatedAt) {
			ready[t.Host] = t
		}
	}
	var best *api.HostRecord
	bestFree, inPool := 0, 0
	for i := range hosts {
		h := &hosts[i]
		if !h.Alive || !servesPool(h.Pools, pool) {
			continue
		}
		inPool++
		t := ready[h.ID]
		if t == nil {
			continue
		}
		mem := req.MemoryMB
		if mem == 0 {
			mem = t.MemoryMB
		}
		running := h.RunningMB
		if h.ID == m.host {
			m.mu.Lock()
			running = m.runningMem
			m.mu.Unlock()
		}
		free := h.CapacityMB - running - mem
		if best == nil || free > bestFree || (free == bestFree && h.ID == m.host) {
			best, bestFree = h, free
		}
	}
	switch {
	case best != nil:
		return best, nil
	case inPool == 0:
		return nil, errf(503, "no_host", "no live host runs pool %s", pool)
	default:
		return nil, errf(404, "template_not_found", "no live host in pool %s has template %q ready", pool, ref)
	}
}

// reconcileTemplates builds, on this host, every template spec it has no
// build of yet. A spec counts as built once this host has a build of that
// name started at or after the spec's last update, whatever its outcome:
// a failed build is retried only when the template is created again.
func (m *Manager) reconcileTemplates(ctx context.Context) {
	m.tplMu.Lock()
	defer m.tplMu.Unlock()
	specs, err := m.store.TemplateSpecs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Error("template specs", "err", err)
		}
		return
	}
	local, err := m.store.ListTemplates(ctx)
	if err != nil {
		return
	}
	for _, sp := range specs {
		built := false
		for _, t := range local {
			if t.Name == sp.Name && !t.CreatedAt.Before(sp.UpdatedAt) {
				built = true
				break
			}
		}
		if built {
			continue
		}
		m.log.Info("building template from cluster spec", "template", sp.Name)
		if _, err := m.startTemplate(ctx, sp.Request, false); err != nil {
			m.log.Error("build template from spec", "template", sp.Name, "err", err)
		}
	}
}
