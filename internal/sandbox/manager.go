// Package sandbox implements the sandbox lifecycle: create, resume,
// suspend (hot snapshot), archive (cold tier), delete, idle reaping and the
// memory budget.
package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/internal/egress"
	"github.com/Pushpavel/fired/internal/events"
	"github.com/Pushpavel/fired/internal/fsutil"
	"github.com/Pushpavel/fired/internal/guest"
	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/internal/vmm"
	"github.com/Pushpavel/fired/pkg/api"
)

type Manager struct {
	cfg     config.Config
	store   *store.Store
	drv     vmm.Driver
	bus     *events.Bus
	secrets *redact.Registry
	proxy   *egress.Proxy
	global  egress.Policy
	log     *slog.Logger
	version string
	// host is this host's ID in the cluster (cluster.host_id).
	host      string
	startedAt time.Time

	mu         sync.Mutex
	entries    map[string]*entry
	runningMem int
	// tplMu serializes starting template builds (API and cluster specs).
	tplMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// entry is the in-memory state of one sandbox.
//
// Locking: op serializes lifecycle transitions (create/resume/suspend/
// archive/delete) and may be held across slow I/O. mu protects the fields
// below and is only held briefly. Order: op → Manager.mu → mu.
type entry struct {
	id string
	op sync.Mutex

	mu        sync.Mutex
	rec       api.Sandbox
	vm        vmm.VM
	agent     *guest.Client
	transport *portTransport
	policy    egress.Policy
	memMB     int // reserved while running
	// admitted: the quota key's limits were checked and this sandbox counts
	// as running against them (store.Admit) until release.
	admitted bool
	gone     bool

	lastActive atomic.Int64 // unix nanos
	busy       atomic.Int32
}

func (e *entry) snapshot() api.Sandbox {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rec
	r.Env = maps.Clone(e.rec.Env)
	r.Labels = maps.Clone(e.rec.Labels)
	r.Ports = slices.Clone(e.rec.Ports)
	r.Egress.Allow = slices.Clone(e.rec.Egress.Allow)
	r.LastActiveAt = time.Unix(0, e.lastActive.Load()).UTC()
	return r
}

func (e *entry) touch() { e.lastActive.Store(time.Now().UnixNano()) }

type Options struct {
	Config  config.Config
	Store   *store.Store
	Driver  vmm.Driver
	Bus     *events.Bus
	Secrets *redact.Registry
	Log     *slog.Logger
	Version string
}

func New(o Options) (*Manager, error) {
	m := &Manager{
		cfg: o.Config, store: o.Store, drv: o.Driver, bus: o.Bus, secrets: o.Secrets,
		log: o.Log, version: o.Version, entries: map[string]*entry{},
		host: o.Store.Host(), startedAt: time.Now().UTC(),
	}
	o.Store.HostTimeout = o.Config.Cluster.HostTimeout.D()
	o.Store.Defaults = o.Config.Quotas
	if m.bus == nil {
		m.bus = events.NewBus()
	}
	if m.secrets == nil {
		m.secrets = redact.NewRegistry()
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	var err error
	m.global, err = egress.Compile(o.Config.Egress.GlobalAllow)
	if err != nil {
		return nil, fmt.Errorf("egress.global_allow: %w", err)
	}
	if o.Config.Egress.Enabled {
		m.proxy, err = egress.New(m.egressPolicy, egress.Options{
			AllowedPorts:      o.Config.Egress.AllowedPorts,
			AllowPrivateCIDRs: o.Config.Egress.AllowPrivateCIDRs,
			UpstreamProxy:     o.Config.Egress.UpstreamProxy,
			Log:               m.log.With("component", "egress"),
			OnActivity:        func(id string) { m.Touch(id) },
		})
		if err != nil {
			return nil, err
		}
	}
	for _, d := range []string{o.Config.Data.HotDir, o.Config.Data.ColdDir, o.Config.Data.TemplatesDir, m.logDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Unix socket paths are limited to 107 bytes; check the longest one.
	if p := filepath.Join(m.drv.Workdir(strings.Repeat("x", 46)), "v.sock_3128"); len(p) > 107 {
		return nil, fmt.Errorf("data.hot_dir path is too long: sandbox socket paths would exceed 107 bytes (%s)", p)
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

func (m *Manager) Bus() *events.Bus      { return m.bus }
func (m *Manager) Store() *store.Store   { return m.store }
func (m *Manager) HostID() string        { return m.host }
func (m *Manager) Config() config.Config { return m.cfg }

// publish sends an event from this host (to every host, see relayEvents).
func (m *Manager) publish(e api.Event) {
	e.Host = m.host
	m.bus.Publish(e)
}

// Has reports whether the sandbox lives on this host.
func (m *Manager) Has(id string) bool        { return m.get(id) != nil }
func (m *Manager) Secrets() *redact.Registry { return m.secrets }
func (m *Manager) DriverName() string        { return m.drv.Name() }

func (m *Manager) logDir() string { return filepath.Join(m.cfg.Data.HotDir, "logs") }

// LogPath is the VMM/serial console log of a sandbox.
func (m *Manager) LogPath(id string) string { return filepath.Join(m.logDir(), id+".log") }

func (m *Manager) archivePath(id string) string {
	return filepath.Join(m.cfg.Data.ColdDir, id+".fired.zst")
}

func (m *Manager) templateDir(id string) string { return filepath.Join(m.cfg.Data.TemplatesDir, id) }

func (m *Manager) egressPolicy(id string) (egress.Policy, bool) {
	e := m.get(id)
	if e == nil {
		// Template build VMs ("tmpl-*") may reach any public destination.
		if strings.HasPrefix(id, "tmpl-") {
			p, _ := egress.Compile([]string{"*"})
			return p, true
		}
		return egress.Policy{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return egress.Merge(m.global, e.policy), true
}

func (m *Manager) egressHandler(id string) func(net.Conn) {
	if m.proxy == nil {
		return nil
	}
	return func(c net.Conn) { m.proxy.ServeConn(id, c) }
}

func (m *Manager) get(id string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[id]
}

func (m *Manager) mustGet(id string) (*entry, error) {
	e := m.get(id)
	if e == nil {
		return nil, ErrNotFound
	}
	return e, nil
}

// Touch records activity (keeps the sandbox from idling out).
func (m *Manager) Touch(id string) {
	if e := m.get(id); e != nil {
		e.touch()
	}
}

// setState persists a state change and publishes an event. Caller holds e.op.
func (m *Manager) setState(e *entry, st api.SandboxState, errMsg string) {
	e.mu.Lock()
	prev := e.rec.State
	e.rec.State = st
	e.rec.Error = errMsg
	e.rec.StateChangedAt = time.Now().UTC()
	e.rec.LastActiveAt = time.Unix(0, e.lastActive.Load()).UTC()
	rec := e.rec
	e.mu.Unlock()
	if err := m.store.PutSandbox(context.Background(), &rec); err != nil {
		m.log.Error("persist sandbox", "sandbox", e.id, "err", err)
	}
	if prev != st {
		m.log.Info("sandbox state", "sandbox", e.id, "from", prev, "to", st, "error", errMsg)
	}
	m.publish(api.Event{Type: "sandbox.state", SandboxID: e.id, State: st, PrevState: prev, Error: errMsg, At: time.Now().UTC()})
}

func (m *Manager) persist(e *entry) {
	e.mu.Lock()
	e.rec.LastActiveAt = time.Unix(0, e.lastActive.Load()).UTC()
	rec := e.rec
	e.mu.Unlock()
	if err := m.store.PutSandbox(context.Background(), &rec); err != nil {
		m.log.Error("persist sandbox", "sandbox", e.id, "err", err)
	}
}

// ---------------------------------------------------------------- load

// Load restores in-memory state from the database after a (re)start and
// reconciles it with what is on disk.
func (m *Manager) Load(ctx context.Context) error {
	// Nothing runs yet after a (re)start.
	if err := m.store.ResetRunning(ctx); err != nil {
		return err
	}
	recs, err := m.store.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, r := range recs {
		e := &entry{id: r.ID, rec: *r}
		e.lastActive.Store(r.LastActiveAt.UnixNano())
		if pol, err := egress.Compile(r.Egress.Allow); err == nil {
			e.policy = pol
		}
		m.entries[r.ID] = e
		hasSnap := m.drv.HasSnapshot(r.ID)
		_, wdErr := os.Stat(m.drv.Workdir(r.ID))
		hasWork := wdErr == nil
		_, arErr := os.Stat(m.archivePath(r.ID))
		hasArchive := arErr == nil
		next := r.State
		msg := r.Error
		switch r.State {
		case api.StateDeleting:
			m.log.Info("finishing interrupted delete", "sandbox", r.ID)
			e.op.Lock()
			m.deleteLocked(e)
			e.op.Unlock()
			continue
		case api.StateArchived:
			if !hasArchive {
				next, msg = api.StateError, "archive file missing"
			}
		case api.StateArchiving:
			if hasArchive {
				next = api.StateArchived
				_ = m.drv.Remove(r.ID)
			} else if hasSnap {
				next = api.StateSuspended
			} else if hasWork {
				next = api.StateStopped
			} else {
				next, msg = api.StateError, "sandbox data lost"
			}
		default: // running, resuming, creating, suspending, suspended, stopped, error
			switch {
			case hasSnap:
				next = api.StateSuspended
			case hasWork:
				next = api.StateStopped
			case hasArchive:
				next = api.StateArchived
			default:
				next, msg = api.StateError, "sandbox data lost (hot storage was cleared, e.g. by a host reboot, before it was archived)"
			}
		}
		if next != r.State || msg != r.Error {
			m.setState(e, next, msg)
		}
	}
	tpls, err := m.store.ListTemplates(ctx)
	if err != nil {
		return err
	}
	for _, t := range tpls {
		if t.Status == api.TemplateBuilding {
			t.Status, t.Error = api.TemplateFailed, "daemon restarted during build"
			_ = m.store.PutTemplate(ctx, t)
		}
	}
	m.log.Info("state loaded", "sandboxes", len(recs), "templates", len(tpls))
	return nil
}

// Start records this host in the cluster and launches background loops.
func (m *Manager) Start() {
	m.wg.Add(1)
	go m.reaper()
	m.StartCluster()
}

// StartCluster records this host and starts the loops that keep the
// cluster's shared view current, without the idle reaper (tests drive that
// with ReapOnce).
func (m *Manager) StartCluster() {
	if err := m.heartbeat(m.ctx); err != nil {
		m.log.Error("heartbeat", "err", err)
	}
	m.wg.Add(3)
	listening := make(chan struct{})
	go func() {
		defer m.wg.Done()
		m.store.Listen(m.ctx, eventsChannel, listening, m.receiveEvent)
	}()
	// Don't miss what other hosts announce right after we start.
	select {
	case <-listening:
	case <-time.After(5 * time.Second):
		m.log.Warn("not yet listening for other hosts' events; continuing")
	}
	go m.clusterLoop()
	go m.relayEvents()
}

// Shutdown stops background loops and applies the shutdown action to
// running sandboxes.
func (m *Manager) Shutdown(ctx context.Context) {
	m.cancel()
	m.wg.Wait()
	m.mu.Lock()
	var list []*entry
	for _, e := range m.entries {
		list = append(list, e)
	}
	m.mu.Unlock()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, e := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(e *entry) {
			defer wg.Done()
			defer func() { <-sem }()
			e.op.Lock()
			defer e.op.Unlock()
			st := e.snapshot().State
			switch m.cfg.ShutdownAction {
			case "suspend", "archive":
				if st == api.StateRunning {
					if err := m.suspendLocked(ctx, e); err != nil {
						m.log.Error("suspend on shutdown", "sandbox", e.id, "err", err)
					}
				}
				if m.cfg.ShutdownAction == "archive" {
					if s := e.snapshot().State; s == api.StateSuspended || s == api.StateStopped {
						if err := m.archiveLocked(ctx, e); err != nil {
							m.log.Error("archive on shutdown", "sandbox", e.id, "err", err)
						}
					}
				}
			default:
				if st == api.StateRunning {
					m.stopLocked(e, "")
				}
			}
		}(e)
	}
	wg.Wait()
}

// ---------------------------------------------------------------- memory

// reserve admits the sandbox against its quota key's limits (fail closed:
// no database, no start), then reserves mb of this host's memory budget.
func (m *Manager) reserve(ctx context.Context, self *entry, mb int) error {
	if self != nil {
		if err := m.admit(ctx, self, mb); err != nil {
			return err
		}
	}
	if err := m.reserveMem(ctx, self, mb); err != nil {
		if self != nil {
			m.unadmit(self)
		}
		return err
	}
	return nil
}

func (m *Manager) admit(ctx context.Context, e *entry, mb int) error {
	rec := e.snapshot()
	if rec.QuotaKey == "" {
		return nil
	}
	err := m.store.Admit(ctx, e.id, rec.QuotaKey, rec.Pool, mb)
	var qe *store.QuotaError
	switch {
	case errors.As(err, &qe):
		return errf(429, "quota_exceeded", "%s", qe.Msg)
	case err != nil:
		m.log.Error("admission", "sandbox", e.id, "err", err)
		return errf(503, "admission_unavailable", "can't check quota for %s: the database is unavailable", rec.QuotaKey)
	}
	e.mu.Lock()
	e.admitted = true
	e.mu.Unlock()
	return nil
}

func (m *Manager) unadmit(e *entry) {
	e.mu.Lock()
	was := e.admitted
	e.admitted = false
	e.mu.Unlock()
	if was {
		if err := m.store.Release(context.Background(), e.id); err != nil {
			m.log.Error("release quota", "sandbox", e.id, "err", err)
		}
	}
}

func (m *Manager) reserveMem(ctx context.Context, self *entry, mb int) error {
	for attempt := 0; attempt < 8; attempt++ {
		m.mu.Lock()
		if m.runningMem+mb <= m.cfg.Capacity.MaxRunningMemoryMB {
			m.runningMem += mb
			m.mu.Unlock()
			if self != nil {
				self.mu.Lock()
				self.memMB = mb
				self.mu.Unlock()
			}
			return nil
		}
		need := m.runningMem + mb - m.cfg.Capacity.MaxRunningMemoryMB
		var cands []*entry
		if m.cfg.Capacity.EvictIdle {
			for _, e := range m.entries {
				if e == self || e.busy.Load() > 0 {
					continue
				}
				e.mu.Lock()
				ok := e.rec.State == api.StateRunning && e.memMB > 0
				e.mu.Unlock()
				if ok {
					cands = append(cands, e)
				}
			}
		}
		m.mu.Unlock()
		evictable := 0
		for _, c := range cands {
			c.mu.Lock()
			evictable += c.memMB
			c.mu.Unlock()
		}
		// Don't suspend anything if it still wouldn't fit.
		if len(cands) == 0 || evictable < need {
			return ErrCapacity
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].lastActive.Load() < cands[j].lastActive.Load() })
		freed := 0
		for _, c := range cands {
			if freed >= need {
				break
			}
			if !c.op.TryLock() {
				continue
			}
			c.mu.Lock()
			mem := c.memMB
			c.mu.Unlock()
			err := m.suspendIf(ctx, c, func(x *entry) bool { return x.busy.Load() == 0 })
			if err == nil {
				m.log.Info("evicted idle sandbox for capacity", "sandbox", c.id, "for", idOf(self))
				freed += mem
			}
			c.op.Unlock()
		}
		if freed == 0 {
			return ErrCapacity
		}
	}
	return ErrCapacity
}

func idOf(e *entry) string {
	if e == nil {
		return ""
	}
	return e.id
}

func (m *Manager) release(e *entry) {
	e.mu.Lock()
	mb := e.memMB
	e.memMB = 0
	e.mu.Unlock()
	m.releaseMB(mb)
	m.unadmit(e)
}

func (m *Manager) releaseMB(mb int) {
	m.mu.Lock()
	m.runningMem -= mb
	if m.runningMem < 0 {
		m.runningMem = 0
	}
	m.mu.Unlock()
}

// ---------------------------------------------------------------- create

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,39}$`)

// NewSandboxID returns a fresh sandbox ID (placement picks one before it
// forwards a create, so a retry can't create two).
func NewSandboxID() string { return newID("sb-") }

func newID(prefix string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return prefix + string(b)
}

func (m *Manager) resolveTemplate(ctx context.Context, ref string) (*api.Template, error) {
	if ref == "" {
		ref = m.cfg.Defaults.Template
	}
	if ref == "" {
		return nil, badRequest("template is required (no defaults.template configured)")
	}
	if t, err := m.store.GetTemplate(ctx, ref); err == nil && t.Host == m.host {
		if t.Status != api.TemplateReady {
			return nil, conflict("template %s is %s", ref, t.Status)
		}
		return t, nil
	}
	t, err := m.store.LatestReadyTemplate(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errf(404, "template_not_found", "no ready template named %q", ref)
	}
	return t, err
}

func (m *Manager) Create(ctx context.Context, req api.CreateSandboxRequest) (*api.Sandbox, error) {
	t, err := m.resolveTemplate(ctx, req.Template)
	if err != nil {
		return nil, err
	}
	id := req.ID
	if id == "" {
		id = newID("sb-")
	} else if !idRe.MatchString(id) || strings.HasPrefix(id, "tmpl-") {
		return nil, badRequest("id must match %s and not start with tmpl-", idRe)
	}
	d := m.cfg.Defaults
	vcpus, mem, disk := req.VCPUs, req.MemoryMB, req.DiskMB
	if vcpus == 0 {
		vcpus = t.VCPUs
	}
	if mem == 0 {
		mem = t.MemoryMB
	}
	if disk == 0 {
		disk = t.DiskMB
	}
	if vcpus <= 0 || vcpus > d.MaxVCPUs {
		return nil, badRequest("vcpus must be between 1 and %d", d.MaxVCPUs)
	}
	if mem < 128 || mem > d.MaxMemoryMB {
		return nil, badRequest("memory_mb must be between 128 and %d", d.MaxMemoryMB)
	}
	if disk < 512 || disk > d.MaxDiskMB {
		return nil, badRequest("disk_mb must be between 512 and %d", d.MaxDiskMB)
	}
	allow := []string{}
	if req.Egress != nil {
		allow = req.Egress.Allow
	}
	pol, err := egress.Compile(allow)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	redactOn := d.Redact
	if req.Redact != nil {
		redactOn = *req.Redact
	}
	pool := req.Pool
	if pool == "" {
		pool = m.cfg.Cluster.DefaultPool
	}
	if !m.cfg.Cluster.ServesPool(pool) {
		return nil, errf(409, "wrong_pool", "host %s doesn't run pool %s", m.host, pool)
	}
	now := time.Now().UTC()
	rec := api.Sandbox{
		ID: id, Template: t.Name, TemplateID: t.ID, State: api.StateCreating,
		VCPUs: vcpus, MemoryMB: mem, DiskMB: disk,
		Env: req.Env, Labels: req.Labels, TTL: req.TTL,
		Egress: api.EgressPolicy{Allow: allow}, Redact: redactOn, Ports: []int{},
		CreatedAt: now, LastActiveAt: now, StateChangedAt: now,
		Host: m.host, QuotaKey: req.QuotaKey, Pool: pool,
	}
	e := &entry{id: id, rec: rec, policy: pol}
	e.touch()

	m.mu.Lock()
	if _, exists := m.entries[id]; exists {
		m.mu.Unlock()
		return nil, conflict("sandbox %s already exists", id)
	}
	if max := m.cfg.Capacity.MaxSandboxes; max > 0 && len(m.entries) >= max {
		m.mu.Unlock()
		return nil, errf(503, "capacity", "sandbox limit (%d) reached", max)
	}
	m.entries[id] = e
	e.op.Lock()
	m.mu.Unlock()
	defer e.op.Unlock()

	if err := m.store.InsertSandbox(ctx, &rec); err != nil {
		m.mu.Lock()
		delete(m.entries, id)
		m.mu.Unlock()
		return nil, conflict("sandbox %s: %v", id, err)
	}
	m.publish(api.Event{Type: "sandbox.state", SandboxID: id, State: api.StateCreating, At: now})

	if err := m.reserve(ctx, e, mem); err != nil {
		// Nothing was created; don't leave a record behind.
		m.deleteLocked(e)
		return nil, err
	}
	if err := m.startLocked(ctx, e, vmm.ModeNew, t); err != nil {
		m.release(e)
		_ = m.drv.Remove(id)
		m.setState(e, api.StateError, err.Error())
		return nil, fmt.Errorf("create sandbox %s: %w", id, err)
	}
	m.setState(e, api.StateRunning, "")
	s := e.snapshot()
	return &s, nil
}

// startLocked boots/restores the VM, waits for the agent and syncs the
// guest. Caller holds e.op and has reserved memory.
func (m *Manager) startLocked(ctx context.Context, e *entry, mode vmm.Mode, t *api.Template) error {
	err := m.startOnce(ctx, e, mode, t, false)
	if err == nil {
		return nil
	}
	// Snapshots don't survive every change (e.g. a Firecracker upgrade).
	// Fall back to a cold boot rather than leaving the sandbox unusable.
	switch {
	case mode == vmm.ModeNew && t != nil && t.HasSnapshot:
		m.log.Warn("start from template snapshot failed; cold-booting", "sandbox", e.id, "err", err)
		_ = m.drv.Remove(e.id)
		return m.startOnce(ctx, e, mode, t, true)
	case mode == vmm.ModeResume && m.drv.HasSnapshot(e.id):
		m.log.Warn("restoring memory snapshot failed; cold-booting from disk (running processes are lost)", "sandbox", e.id, "err", err)
		if derr := m.drv.DiscardSnapshot(e.id); derr != nil {
			return err
		}
		return m.startOnce(ctx, e, mode, t, true)
	}
	return err
}

func (m *Manager) startOnce(ctx context.Context, e *entry, mode vmm.Mode, t *api.Template, cold bool) error {
	if t == nil {
		var err error
		rec := e.snapshot()
		t, err = m.store.GetTemplate(ctx, rec.TemplateID)
		if err != nil {
			return fmt.Errorf("template %s: %w", rec.TemplateID, err)
		}
	}
	rec := e.snapshot()
	vt := vmm.Template{
		ID: t.ID, Dir: m.templateDir(t.ID), HasSnapshot: t.HasSnapshot,
		VCPUs: t.VCPUs, MemoryMB: t.MemoryMB, DiskMB: t.DiskMB,
	}
	// A snapshot fixes vCPU count and memory size; other shapes cold-boot.
	if cold || (vt.HasSnapshot && (rec.VCPUs != t.VCPUs || rec.MemoryMB != t.MemoryMB)) {
		vt.HasSnapshot = false
	}
	spec := vmm.StartSpec{
		ID: e.id, Template: vt, VCPUs: rec.VCPUs, MemoryMB: rec.MemoryMB, DiskMB: rec.DiskMB,
		Egress: m.egressHandler(e.id), LogPath: m.LogPath(e.id),
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.bootTimeout()+30*time.Second)
	defer cancel()
	vm, info, err := m.drv.Start(bctx, spec, mode)
	if err != nil {
		return err
	}
	agent := guest.NewClient(vm.Dial)
	if _, err := agent.WaitReady(bctx, m.bootTimeout(), vm.Done()); err != nil {
		_ = vm.Stop()
		return err
	}
	entropy := make([]byte, 64)
	_, _ = rand.Read(entropy)
	sr, err := agent.Sync(bctx, guest.SyncRequest{UnixNanos: time.Now().UnixNano(), Entropy: entropy, GrowDisk: info.GrowDisk})
	if err != nil {
		m.log.Warn("guest sync failed", "sandbox", e.id, "err", err)
	} else if sr.Warning != "" {
		m.log.Warn("guest sync", "sandbox", e.id, "warning", sr.Warning)
	}
	e.mu.Lock()
	e.vm = vm
	e.agent = agent
	e.transport = newPortTransport(agent)
	e.mu.Unlock()
	e.touch()
	m.log.Info("vm started", "sandbox", e.id, "restored", info.Restored, "mode", mode)
	go m.watch(e, vm)
	return nil
}

func (m *Manager) bootTimeout() time.Duration {
	if d := m.cfg.Firecracker.BootTimeout.D(); d > 0 {
		return d
	}
	return 30 * time.Second
}

// watch handles a VM that exits on its own (crash, guest reboot).
func (m *Manager) watch(e *entry, vm vmm.VM) {
	<-vm.Done()
	e.op.Lock()
	defer e.op.Unlock()
	e.mu.Lock()
	if e.vm != vm || e.gone {
		e.mu.Unlock()
		return // expected (suspend/stop/delete)
	}
	e.mu.Unlock()
	m.log.Warn("vm exited unexpectedly", "sandbox", e.id)
	m.detach(e)
	m.release(e)
	m.setState(e, api.StateStopped, "vm exited unexpectedly; it will cold-boot on next use (see log)")
}

// detach clears the running VM handles. Returns the VM (may be nil).
func (m *Manager) detach(e *entry) vmm.VM {
	e.mu.Lock()
	vm := e.vm
	agent := e.agent
	tr := e.transport
	e.vm, e.agent, e.transport = nil, nil, nil
	e.mu.Unlock()
	if agent != nil {
		agent.Close()
	}
	if tr != nil {
		tr.Close()
	}
	return vm
}

// ---------------------------------------------------------------- use

// Lease is a handle on a running sandbox. Release must be called.
type Lease struct {
	ID        string
	Agent     *guest.Client
	Transport *portTransport
	Redact    *redact.Set
	Sandbox   api.Sandbox
	e         *entry
	once      sync.Once
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.e.touch()
		l.e.busy.Add(-1)
	})
}

// ErrNotRunning is returned by Acquire with wake=false.
var ErrNotRunning = errf(503, "not_running", "sandbox is not running")

// Acquire returns a lease on a running sandbox, resuming it first if it is
// suspended/archived/stopped (unless wake is false).
func (m *Manager) Acquire(ctx context.Context, id string, wake bool) (*Lease, error) {
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	if l := m.tryLease(e); l != nil {
		return l, nil
	}
	if !wake {
		return nil, &Error{Status: 503, Code: "not_running", Msg: "sandbox is " + string(e.snapshot().State)}
	}
	e.op.Lock()
	defer e.op.Unlock()
	if l := m.tryLease(e); l != nil {
		return l, nil
	}
	if err := m.resumeLocked(ctx, e); err != nil {
		return nil, err
	}
	if l := m.tryLease(e); l != nil {
		return l, nil
	}
	return nil, errf(503, "not_running", "sandbox did not reach running state")
}

func (m *Manager) tryLease(e *entry) *Lease {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gone || e.rec.State != api.StateRunning || e.agent == nil {
		return nil
	}
	e.busy.Add(1)
	e.lastActive.Store(time.Now().UnixNano())
	var set *redact.Set
	if e.rec.Redact {
		set = m.secrets.For(e.id)
	}
	rec := e.rec
	rec.Env = maps.Clone(e.rec.Env)
	rec.Ports = slices.Clone(e.rec.Ports)
	return &Lease{ID: e.id, Agent: e.agent, Transport: e.transport, Redact: set, Sandbox: rec, e: e}
}

// Resume brings a sandbox to running.
func (m *Manager) Resume(ctx context.Context, id string) (*api.Sandbox, error) {
	l, err := m.Acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	l.Release()
	return m.Get(id)
}

func (m *Manager) resumeLocked(ctx context.Context, e *entry) error {
	st := e.snapshot().State
	switch st {
	case api.StateRunning:
		return nil
	case api.StateDeleting, api.StateDeleted:
		return ErrNotFound
	case api.StateCreating:
		return conflict("sandbox is still being created")
	}
	if st == api.StateArchived || (st == api.StateError && !m.workdirExists(e.id)) {
		if _, err := os.Stat(m.archivePath(e.id)); err != nil {
			return errf(409, "unrecoverable", "sandbox is in state %s and has no data to resume from", st)
		}
		m.setState(e, api.StateResuming, "")
		start := time.Now()
		if err := fsutil.Unpack(m.archivePath(e.id), m.drv.Workdir(e.id)); err != nil {
			_ = m.drv.Remove(e.id)
			m.setState(e, api.StateArchived, "restore from archive failed: "+err.Error())
			return fmt.Errorf("unarchive: %w", err)
		}
		m.log.Info("unarchived", "sandbox", e.id, "took", time.Since(start).Round(time.Millisecond))
	}
	prev := e.snapshot().State
	if prev != api.StateResuming {
		m.setState(e, api.StateResuming, "")
	}
	rec := e.snapshot()
	if err := m.reserve(ctx, e, rec.MemoryMB); err != nil {
		m.setState(e, restingState(m, e.id), "")
		return err
	}
	if err := m.startLocked(ctx, e, vmm.ModeResume, nil); err != nil {
		m.release(e)
		m.setState(e, restingState(m, e.id), "resume failed: "+err.Error())
		return fmt.Errorf("resume %s: %w", e.id, err)
	}
	// The cold copy is now stale.
	_ = os.Remove(m.archivePath(e.id))
	m.setState(e, api.StateRunning, "")
	return nil
}

func (m *Manager) workdirExists(id string) bool {
	_, err := os.Stat(m.drv.Workdir(id))
	return err == nil
}

// restingState is where a sandbox lands after a failed resume.
func restingState(m *Manager, id string) api.SandboxState {
	switch {
	case m.drv.HasSnapshot(id):
		return api.StateSuspended
	case m.workdirExists(id):
		return api.StateStopped
	default:
		if _, err := os.Stat(m.archivePath(id)); err == nil {
			return api.StateArchived
		}
		return api.StateError
	}
}

// ---------------------------------------------------------------- suspend / archive / stop

func (m *Manager) Suspend(ctx context.Context, id string) (*api.Sandbox, error) {
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	e.op.Lock()
	err = m.suspendLocked(ctx, e)
	e.op.Unlock()
	if err != nil {
		return nil, err
	}
	return m.Get(id)
}

func (m *Manager) suspendLocked(ctx context.Context, e *entry) error {
	return m.suspendIf(ctx, e, nil)
}

// errSkipped means a conditional suspend did not apply.
var errSkipped = errors.New("skipped")

// suspendIf snapshots a running sandbox. cond (evaluated under e.mu,
// atomically with the state change) can veto the suspend, e.g. when a
// request started using the sandbox in the meantime.
func (m *Manager) suspendIf(ctx context.Context, e *entry, cond func(*entry) bool) error {
	ok := m.casState(e, func() bool {
		return e.rec.State == api.StateRunning && (cond == nil || cond(e))
	}, api.StateSuspending)
	if !ok {
		if cond != nil {
			return errSkipped
		}
		return nil
	}
	vm := m.detach(e)
	if vm == nil {
		m.release(e)
		m.setState(e, restingState(m, e.id), "")
		return nil
	}
	start := time.Now()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if err := vm.Snapshot(sctx); err != nil {
		m.log.Error("snapshot failed; stopping VM (disk is kept)", "sandbox", e.id, "err", err)
		_ = vm.Stop()
		m.release(e)
		m.setState(e, api.StateStopped, "snapshot failed: "+err.Error())
		return err
	}
	m.release(e)
	m.log.Info("suspended", "sandbox", e.id, "took", time.Since(start).Round(time.Millisecond))
	m.setState(e, api.StateSuspended, "")
	return nil
}

// casState sets the state to `to` if cond (called with e.mu held) holds.
func (m *Manager) casState(e *entry, cond func() bool, to api.SandboxState) bool {
	e.mu.Lock()
	if !cond() {
		e.mu.Unlock()
		return false
	}
	prev := e.rec.State
	e.rec.State = to
	e.rec.Error = ""
	e.rec.StateChangedAt = time.Now().UTC()
	e.rec.LastActiveAt = time.Unix(0, e.lastActive.Load()).UTC()
	rec := e.rec
	e.mu.Unlock()
	if err := m.store.PutSandbox(context.Background(), &rec); err != nil {
		m.log.Error("persist sandbox", "sandbox", e.id, "err", err)
	}
	m.log.Info("sandbox state", "sandbox", e.id, "from", prev, "to", to)
	m.publish(api.Event{Type: "sandbox.state", SandboxID: e.id, State: to, PrevState: prev, At: time.Now().UTC()})
	return true
}

// stopLocked kills the VM without a memory snapshot.
func (m *Manager) stopLocked(e *entry, msg string) {
	if vm := m.detach(e); vm != nil {
		_ = vm.Stop()
	}
	m.release(e)
	m.setState(e, restingState(m, e.id), msg)
}

func (m *Manager) Archive(ctx context.Context, id string) (*api.Sandbox, error) {
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	e.op.Lock()
	err = m.archiveLocked(ctx, e)
	e.op.Unlock()
	if err != nil {
		return nil, err
	}
	return m.Get(id)
}

func (m *Manager) archiveLocked(ctx context.Context, e *entry) error {
	st := e.snapshot().State
	if st == api.StateArchived {
		return nil
	}
	if st == api.StateRunning {
		if err := m.suspendLocked(ctx, e); err != nil {
			return err
		}
		st = e.snapshot().State
	}
	if st != api.StateSuspended && st != api.StateStopped {
		return conflict("cannot archive a sandbox in state %s", st)
	}
	m.setState(e, api.StateArchiving, "")
	keep := map[string]bool{}
	for _, f := range m.drv.PersistentFiles() {
		keep[f] = true
	}
	start := time.Now()
	err := fsutil.Pack(m.drv.Workdir(e.id), m.archivePath(e.id), func(rel string, _ os.DirEntry) bool {
		top, _, _ := strings.Cut(rel, "/")
		return !keep[top]
	})
	if err != nil {
		m.setState(e, st, "archive failed: "+err.Error())
		return fmt.Errorf("archive: %w", err)
	}
	if err := m.drv.Remove(e.id); err != nil {
		m.log.Warn("remove workdir after archive", "sandbox", e.id, "err", err)
	}
	st2, _ := os.Stat(m.archivePath(e.id))
	var size int64
	if st2 != nil {
		size = st2.Size()
	}
	m.log.Info("archived", "sandbox", e.id, "bytes", size, "took", time.Since(start).Round(time.Millisecond))
	m.setState(e, api.StateArchived, "")
	return nil
}

// ---------------------------------------------------------------- delete

func (m *Manager) Delete(ctx context.Context, id string) error {
	e, err := m.mustGet(id)
	if err != nil {
		return err
	}
	e.op.Lock()
	defer e.op.Unlock()
	m.deleteLocked(e)
	return nil
}

func (m *Manager) deleteLocked(e *entry) {
	e.mu.Lock()
	if e.gone {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	m.setState(e, api.StateDeleting, "")
	e.mu.Lock()
	e.gone = true
	e.mu.Unlock()
	if vm := m.detach(e); vm != nil {
		_ = vm.Stop()
	}
	m.release(e)
	if err := m.drv.Remove(e.id); err != nil {
		m.log.Warn("remove workdir", "sandbox", e.id, "err", err)
	}
	_ = os.Remove(m.archivePath(e.id))
	_ = os.Remove(m.LogPath(e.id))
	if err := m.store.DeleteSandbox(context.Background(), e.id); err != nil {
		m.log.Error("delete record", "sandbox", e.id, "err", err)
	}
	m.secrets.Drop(e.id)
	m.mu.Lock()
	delete(m.entries, e.id)
	m.mu.Unlock()
	m.log.Info("deleted", "sandbox", e.id)
	m.publish(api.Event{Type: "sandbox.deleted", SandboxID: e.id, State: api.StateDeleted, At: time.Now().UTC()})
}

// ---------------------------------------------------------------- queries & updates

func (m *Manager) Get(id string) (*api.Sandbox, error) {
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	s := e.snapshot()
	return &s, nil
}

// List returns sandboxes matching all label selectors (key=value).
func (m *Manager) List(labels map[string]string, state string) []api.Sandbox {
	m.mu.Lock()
	list := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e)
	}
	m.mu.Unlock()
	out := []api.Sandbox{}
	for _, e := range list {
		s := e.snapshot()
		if state != "" && string(s.State) != state {
			continue
		}
		match := true
		for k, v := range labels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Manager) Update(ctx context.Context, id string, req api.UpdateSandboxRequest) (*api.Sandbox, error) {
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	var pol *egress.Policy
	if req.Egress != nil {
		p, err := egress.Compile(req.Egress.Allow)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		pol = &p
	}
	e.mu.Lock()
	if req.Labels != nil {
		e.rec.Labels = req.Labels
	}
	if req.Env != nil {
		e.rec.Env = req.Env
	}
	if req.TTL != nil {
		e.rec.TTL = *req.TTL
	}
	if pol != nil {
		e.rec.Egress = api.EgressPolicy{Allow: req.Egress.Allow}
		e.policy = *pol
	}
	if req.Redact != nil {
		e.rec.Redact = *req.Redact
	}
	e.mu.Unlock()
	m.persist(e)
	return m.Get(id)
}

func (m *Manager) ExposePort(ctx context.Context, id string, port int) (*api.Sandbox, error) {
	if port <= 0 || port > 65535 {
		return nil, badRequest("port must be 1-65535")
	}
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if !slices.Contains(e.rec.Ports, port) {
		e.rec.Ports = append(e.rec.Ports, port)
		slices.Sort(e.rec.Ports)
	}
	e.mu.Unlock()
	m.persist(e)
	return m.Get(id)
}

func (m *Manager) UnexposePort(ctx context.Context, id string, port int) (*api.Sandbox, error) {
	e, err := m.mustGet(id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.rec.Ports = slices.DeleteFunc(e.rec.Ports, func(p int) bool { return p == port })
	e.mu.Unlock()
	m.persist(e)
	return m.Get(id)
}

func (m *Manager) PortExposed(id string, port int) bool {
	e := m.get(id)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.rec.Ports, port)
}

// Host returns capacity information.
func (m *Manager) Host() api.HostInfo {
	m.mu.Lock()
	used := m.runningMem
	list := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e)
	}
	m.mu.Unlock()
	counts := map[api.SandboxState]int{}
	for _, e := range list {
		counts[e.snapshot().State]++
	}
	return api.HostInfo{
		Driver: m.drv.Name(), Version: m.version,
		MemoryBudgetMB: m.cfg.Capacity.MaxRunningMemoryMB, MemoryRunningMB: used,
		MaxSandboxes: m.cfg.Capacity.MaxSandboxes, Counts: counts,
		HotDirUsedBytes:  fsutil.AllocatedBytes(m.cfg.Data.HotDir),
		ColdDirUsedBytes: fsutil.AllocatedBytes(m.cfg.Data.ColdDir),
		HotDirFreeBytes:  fsutil.FreeBytes(m.cfg.Data.HotDir),
		ColdDirFreeBytes: fsutil.FreeBytes(m.cfg.Data.ColdDir),
	}
}
