package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Pushpavel/fired/internal/guest"
	"github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/internal/vmm"
	"github.com/Pushpavel/fired/pkg/api"
)

var templateNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// BuildLogPath returns the build log of a template.
func (m *Manager) BuildLogPath(id string) string {
	return filepath.Join(m.templateDir(id), "build.log")
}

// CreateTemplate validates the request, records the template and builds it
// asynchronously. Poll GET /v1/templates/{id} or watch events.
func (m *Manager) CreateTemplate(ctx context.Context, req api.CreateTemplateRequest) (*api.Template, error) {
	if !templateNameRe.MatchString(req.Name) {
		return nil, badRequest("name must match %s", templateNameRe)
	}
	if req.Image == "" && m.drv.Name() != "fake" {
		return nil, badRequest("image is required")
	}
	d := m.cfg.Defaults
	t := &api.Template{
		ID: req.Name + "-" + newID("")[:8], Name: req.Name, Image: req.Image, Status: api.TemplateBuilding,
		RootfsMB: req.RootfsMB, DiskMB: req.DiskMB, VCPUs: req.VCPUs, MemoryMB: req.MemoryMB,
		Env: req.Env, Workdir: req.Workdir, CreatedAt: time.Now().UTC(),
	}
	if t.DiskMB == 0 {
		t.DiskMB = d.DiskMB
	}
	if t.VCPUs == 0 {
		t.VCPUs = d.VCPUs
	}
	if t.MemoryMB == 0 {
		t.MemoryMB = d.MemoryMB
	}
	if t.Workdir == "" {
		t.Workdir = "/workspace"
	}
	if t.VCPUs > d.MaxVCPUs || t.MemoryMB > d.MaxMemoryMB || t.DiskMB > d.MaxDiskMB {
		return nil, badRequest("template shape exceeds defaults.max_* limits")
	}
	if err := m.store.PutTemplate(ctx, t); err != nil {
		return nil, err
	}
	m.bus.Publish(api.Event{Type: "template.status", Template: t.ID, Status: string(t.Status), At: time.Now().UTC()})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.buildTemplate(t, req)
	}()
	cp := *t
	return &cp, nil
}

func (m *Manager) buildTemplate(t *api.Template, req api.CreateTemplateRequest) {
	dir := m.templateDir(t.ID)
	_ = os.MkdirAll(dir, 0o755)
	logf, err := os.Create(m.BuildLogPath(t.ID))
	var logw io.Writer = io.Discard
	if err == nil {
		defer logf.Close()
		logw = logf
	}
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Hour)
	defer cancel()
	start := time.Now()
	fail := func(err error) {
		fmt.Fprintf(logw, "BUILD FAILED: %v\n", err)
		m.log.Error("template build failed", "template", t.ID, "err", err)
		t.Status, t.Error = api.TemplateFailed, err.Error()
		_ = m.store.PutTemplate(context.Background(), t)
		m.bus.Publish(api.Event{Type: "template.status", Template: t.ID, Status: string(t.Status), Error: t.Error, At: time.Now().UTC()})
	}
	vt := &vmm.Template{ID: t.ID, Dir: dir, VCPUs: t.VCPUs, MemoryMB: t.MemoryMB, DiskMB: t.DiskMB}
	m.log.Info("building template", "template", t.ID, "image", t.Image)
	if err := m.drv.BuildTemplate(ctx, req, vt, logw); err != nil {
		fail(err)
		return
	}
	snapshot := req.Snapshot == nil || *req.Snapshot
	if snapshot || len(req.Run) > 0 {
		if err := m.bootTemplate(ctx, t, vt, req.Run, snapshot, logw); err != nil {
			fail(err)
			return
		}
		t.HasSnapshot = snapshot
	}
	now := time.Now().UTC()
	t.Status, t.ReadyAt = api.TemplateReady, &now
	if err := m.store.PutTemplate(context.Background(), t); err != nil {
		fail(err)
		return
	}
	fmt.Fprintf(logw, "template ready in %s\n", time.Since(start).Round(time.Second))
	m.log.Info("template ready", "template", t.ID, "snapshot", t.HasSnapshot, "took", time.Since(start).Round(time.Second))
	m.bus.Publish(api.Event{Type: "template.status", Template: t.ID, Status: string(t.Status), At: now})
}

// bootTemplate boots the fresh template, runs setup commands and stores a
// base snapshot that new sandboxes clone.
func (m *Manager) bootTemplate(ctx context.Context, t *api.Template, vt *vmm.Template, run []string, snapshot bool, logw io.Writer) error {
	if len(run) > 0 && !snapshot {
		return errors.New("run commands require snapshot=true (their effects live in the base snapshot)")
	}
	vmID := "tmpl-" + t.ID
	if len(vmID) > 60 {
		vmID = vmID[:60]
	}
	if err := m.reserve(ctx, nil, t.MemoryMB); err != nil {
		return fmt.Errorf("reserve memory for template boot: %w", err)
	}
	defer m.releaseMB(t.MemoryMB)
	_ = m.drv.Remove(vmID)
	spec := vmm.StartSpec{
		ID: vmID, Template: *vt, VCPUs: t.VCPUs, MemoryMB: t.MemoryMB, DiskMB: t.DiskMB,
		Egress: m.egressHandler(vmID), LogPath: filepath.Join(m.templateDir(t.ID), "vm.log"),
	}
	fmt.Fprintf(logw, "booting template VM\n")
	vm, _, err := m.drv.Start(ctx, spec, vmm.ModeNew)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = vm.Stop()
			_ = m.drv.Remove(vmID)
		}
	}()
	agent := guest.NewClient(vm.Dial)
	defer agent.Close()
	h, err := agent.WaitReady(ctx, m.bootTimeout(), vm.Done())
	if err != nil {
		return err
	}
	fmt.Fprintf(logw, "agent %s ready (overlay=%v)\n", h.Version, h.Overlay)
	if !h.Overlay && m.drv.Name() == "firecracker" {
		fmt.Fprintf(logw, "WARNING: guest kernel has no overlayfs; only /workspace, /root, /home, /tmp are writable\n")
	}
	for i, c := range run {
		fmt.Fprintf(logw, "$ %s\n", c)
		res, err := agent.Exec(ctx, &api.ExecRequest{Cmd: c, TimeoutS: 3600})
		if err != nil {
			return fmt.Errorf("run[%d]: %w", i, err)
		}
		fmt.Fprint(logw, res.Stdout)
		fmt.Fprint(logw, res.Stderr)
		if res.ExitCode != 0 {
			return fmt.Errorf("run[%d] %q exited with %d", i, c, res.ExitCode)
		}
	}
	// Flush guest page cache to the disk image before snapshotting.
	_, _ = agent.Exec(ctx, &api.ExecRequest{Cmd: "sync", TimeoutS: 120})
	agent.Close()
	fmt.Fprintf(logw, "creating base snapshot\n")
	if err := vm.Snapshot(ctx); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := m.drv.SaveTemplateSnapshot(vmID, vt); err != nil {
		return err
	}
	ok = true
	return nil
}

func (m *Manager) ListTemplates(ctx context.Context) ([]api.Template, error) {
	ts, err := m.store.ListTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.Template, 0, len(ts))
	for _, t := range ts {
		out = append(out, *t)
	}
	return out, nil
}

// GetTemplate looks up by id, or by name (latest ready build).
func (m *Manager) GetTemplate(ctx context.Context, ref string) (*api.Template, error) {
	t, err := m.store.GetTemplate(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		t, err = m.store.LatestReadyTemplate(ctx, ref)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, errf(404, "template_not_found", "template %q not found", ref)
	}
	return t, err
}

// DeleteTemplate deletes one build (by id) or every unused build of a name.
func (m *Manager) DeleteTemplate(ctx context.Context, ref string) ([]string, error) {
	var targets []*api.Template
	if t, err := m.store.GetTemplate(ctx, ref); err == nil {
		targets = append(targets, t)
	} else {
		all, err := m.store.ListTemplates(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range all {
			if t.Name == ref {
				targets = append(targets, t)
			}
		}
	}
	if len(targets) == 0 {
		return nil, errf(404, "template_not_found", "template %q not found", ref)
	}
	var deleted, inUse []string
	for _, t := range targets {
		if t.Status == api.TemplateBuilding {
			inUse = append(inUse, t.ID+" (building)")
			continue
		}
		n, err := m.store.CountSandboxesByTemplate(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			inUse = append(inUse, fmt.Sprintf("%s (%d sandboxes)", t.ID, n))
			continue
		}
		if err := os.RemoveAll(m.templateDir(t.ID)); err != nil {
			return deleted, err
		}
		if err := m.store.DeleteTemplate(ctx, t.ID); err != nil {
			return deleted, err
		}
		deleted = append(deleted, t.ID)
		m.bus.Publish(api.Event{Type: "template.status", Template: t.ID, Status: "deleted", At: time.Now().UTC()})
	}
	if len(deleted) == 0 && len(inUse) > 0 {
		return nil, conflict("template in use: %s", strings.Join(inUse, ", "))
	}
	return deleted, nil
}
