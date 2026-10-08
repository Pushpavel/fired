// Package fake is a development driver: each "VM" is the fired-agent
// running as an ordinary host process with its filesystem mapped under the
// workdir. It provides NO isolation — never run untrusted code with it.
//
// Suspend kills the process (running commands are lost, files survive);
// resume starts it again. This exercises the whole lifecycle, API and
// tiering logic without KVM.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Pushpavel/fired/internal/fsutil"
	"github.com/Pushpavel/fired/internal/vmm"
	"github.com/Pushpavel/fired/pkg/api"
)

type Driver struct {
	agentBin string
	hot      string
	log      *slog.Logger
}

func New(agentBin, hotDir string, log *slog.Logger) (*Driver, error) {
	p, err := exec.LookPath(agentBin)
	if err != nil {
		return nil, fmt.Errorf("fake driver: agent binary %q: %w", agentBin, err)
	}
	if err := os.MkdirAll(filepath.Join(hotDir, "vms"), 0o755); err != nil {
		return nil, err
	}
	return &Driver{agentBin: p, hot: hotDir, log: log}, nil
}

func (d *Driver) Name() string               { return "fake" }
func (d *Driver) Workdir(id string) string   { return filepath.Join(d.hot, "vms", id) }
func (d *Driver) PersistentFiles() []string  { return []string{"fs", "vmstate"} }
func (d *Driver) HasSnapshot(id string) bool { return exists(filepath.Join(d.Workdir(id), "vmstate")) }
func (d *Driver) DiscardSnapshot(id string) error {
	return os.Remove(filepath.Join(d.Workdir(id), "vmstate"))
}
func (d *Driver) Remove(id string) error { return os.RemoveAll(d.Workdir(id)) }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (d *Driver) BuildTemplate(ctx context.Context, req api.CreateTemplateRequest, t *vmm.Template, logw io.Writer) error {
	root := filepath.Join(t.Dir, "rootfs")
	if err := os.MkdirAll(filepath.Join(root, "etc/fired"), 0o755); err != nil {
		return err
	}
	var env []byte
	for k, v := range req.Env {
		env = append(env, []byte(k+"="+v+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/fired/env"), env, 0o644); err != nil {
		return err
	}
	wd := req.Workdir
	if wd == "" {
		wd = "/workspace"
	}
	meta, _ := json.Marshal(map[string]string{"workdir": wd})
	if err := os.WriteFile(filepath.Join(root, "etc/fired/template.json"), meta, 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, wd), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(logw, "fake template %s ready (image %q is ignored by the fake driver)\n", t.ID, req.Image)
	return nil
}

func (d *Driver) SaveTemplateSnapshot(vmID string, t *vmm.Template) error {
	src := d.Workdir(vmID)
	dst := filepath.Join(t.Dir, "base")
	_ = os.RemoveAll(dst)
	if err := copyTree(filepath.Join(src, "fs"), filepath.Join(dst, "fs")); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func (d *Driver) Start(ctx context.Context, spec vmm.StartSpec, mode vmm.Mode) (vmm.VM, vmm.StartInfo, error) {
	var info vmm.StartInfo
	w := d.Workdir(spec.ID)
	fs := filepath.Join(w, "fs")
	switch mode {
	case vmm.ModeNew:
		if exists(w) {
			return nil, info, fmt.Errorf("workdir %s already exists", w)
		}
		src := filepath.Join(spec.Template.Dir, "rootfs")
		if spec.Template.HasSnapshot {
			src = filepath.Join(spec.Template.Dir, "base", "fs")
			info.Restored = true
		}
		if err := copyTree(src, fs); err != nil {
			return nil, info, fmt.Errorf("prepare fs: %w", err)
		}
	case vmm.ModeResume:
		if !exists(fs) {
			return nil, info, fmt.Errorf("sandbox filesystem missing at %s", fs)
		}
		if exists(filepath.Join(w, "vmstate")) {
			info.Restored = true
		}
	}
	vm := &fakeVM{dir: w, done: make(chan struct{}), log: d.log}
	if spec.Egress != nil {
		sock := filepath.Join(w, "v.sock_3128")
		_ = os.Remove(sock)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return nil, info, err
		}
		vm.egress = ln
		go acceptLoop(ln, spec.Egress)
	}
	agentSock := filepath.Join(w, "agent.sock")
	_ = os.Remove(agentSock)
	args := []string{"serve", "--listen", "unix:" + agentSock, "--root-prefix", fs}
	if spec.Egress != nil {
		args = append(args, "--egress-upstream", "unix:"+filepath.Join(w, "v.sock_3128"), "--egress-listen", "127.0.0.1:0")
	}
	cmd := exec.Command(d.agentBin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "FIRED_FAKE=1")
	if spec.LogPath != "" {
		if f, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
			defer f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		vm.closeEgress()
		return nil, info, err
	}
	vm.cmd = cmd
	go func() {
		_ = cmd.Wait()
		vm.closeEgress()
		close(vm.done)
	}()
	// A restored fake VM has consumed its "snapshot".
	_ = os.Remove(filepath.Join(w, "vmstate"))
	return vm, info, nil
}

func acceptLoop(ln net.Listener, h func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h(c)
	}
}

type fakeVM struct {
	dir    string
	cmd    *exec.Cmd
	egress net.Listener
	done   chan struct{}
	once   sync.Once
	log    *slog.Logger
}

func (v *fakeVM) closeEgress() {
	v.once.Do(func() {
		if v.egress != nil {
			v.egress.Close()
		}
	})
}

func (v *fakeVM) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", filepath.Join(v.dir, "agent.sock"))
}

func (v *fakeVM) Done() <-chan struct{} { return v.done }

func (v *fakeVM) kill() error {
	if v.cmd == nil || v.cmd.Process == nil {
		return nil
	}
	// SIGTERM lets the agent kill the process groups it started.
	_ = syscall.Kill(v.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-v.done:
		return nil
	case <-time.After(3 * time.Second):
	}
	_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-v.done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("fake vm did not exit")
	}
}

func (v *fakeVM) Snapshot(ctx context.Context) error {
	if err := v.kill(); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"driver": "fake", "at": time.Now()})
	return os.WriteFile(filepath.Join(v.dir, "vmstate"), b, 0o644)
}

func (v *fakeVM) Stop() error { return v.kill() }

// copyTree copies a directory tree (files, dirs, symlinks), preserving modes.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm()|0o700)
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, target)
		case fi.Mode().IsRegular():
			if err := fsutil.SparseCopy(p, target); err != nil {
				return err
			}
			return os.Chmod(target, fi.Mode().Perm())
		}
		return nil
	})
}
