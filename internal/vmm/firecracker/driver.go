// Package firecracker runs sandboxes as Firecracker microVMs, optionally
// confined by the Firecracker jailer.
//
// Workdir layout (relative paths are used everywhere so snapshots stay
// valid when restored in another workdir, e.g. template clones):
//
//	rootfs.ext4   template root filesystem (read-only; bind mount or symlink)
//	vmlinux       guest kernel (bind mount or symlink)
//	disk.ext4     per-sandbox writable disk (sparse)
//	vmstate       \ memory snapshot of a suspended sandbox
//	memory        /
//	base.vmstate  \ template snapshot exposed while cloning
//	base.memory   /
//	v.sock        vsock: host → guest connections (agent API, port tunnels)
//	v.sock_3128   vsock: guest → host connections (egress proxy)
//	fc.sock       Firecracker API socket
//
// With the jailer enabled the workdir IS the jail root
// (<hot>/jail/firecracker/<id>/root), so the same relative paths work.
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/internal/fsutil"
	"github.com/Pushpavel/fired/internal/guest"
	"github.com/Pushpavel/fired/internal/vmm"
)

const (
	fRootfs   = "rootfs.ext4"
	fKernel   = "vmlinux"
	fDisk     = "disk.ext4"
	fVMState  = "vmstate"
	fMemory   = "memory"
	fBaseVM   = "base.vmstate"
	fBaseMem  = "base.memory"
	fVsock    = "v.sock"
	fAPISock  = "fc.sock"
	fEgress   = "v.sock_3128"
	agentInit = "/sbin/fired-agent"
)

type Driver struct {
	cfg       config.FirecrackerConfig
	hot       string
	log       *slog.Logger
	execName  string
	cgroupsOK bool
	insecure  []string
}

func New(cfg config.FirecrackerConfig, hotDir string, log *slog.Logger) (*Driver, error) {
	d := &Driver{cfg: cfg, hot: hotDir, log: log, execName: filepath.Base(cfg.Binary)}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("firecracker driver needs /dev/kvm: %w", err)
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, fmt.Errorf("firecracker binary: %w", err)
	}
	if cfg.Jailer.Enabled {
		if _, err := os.Stat(cfg.Jailer.Binary); err != nil {
			return nil, fmt.Errorf("jailer binary: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(hotDir, "jail"), 0o755); err != nil {
			return nil, err
		}
		if cfg.Jailer.Cgroups {
			if err := setupCgroupV2(cfg.Jailer.ParentCgroup); err != nil {
				log.Warn("per-VM cgroups disabled", "err", err)
			} else {
				d.cgroupsOK = true
			}
		}
	} else {
		log.Warn("jailer disabled: Firecracker runs unconfined as the daemon user")
	}
	if err := os.MkdirAll(filepath.Join(hotDir, "vms"), 0o755); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Driver) Name() string { return "firecracker" }

func (d *Driver) Workdir(id string) string {
	if d.cfg.Jailer.Enabled {
		return filepath.Join(d.hot, "jail", d.execName, id, "root")
	}
	return filepath.Join(d.hot, "vms", id)
}

func (d *Driver) PersistentFiles() []string { return []string{fDisk, fVMState, fMemory} }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (d *Driver) HasSnapshot(id string) bool {
	w := d.Workdir(id)
	return exists(filepath.Join(w, fVMState)) && exists(filepath.Join(w, fMemory))
}

func (d *Driver) DiscardSnapshot(id string) error {
	w := d.Workdir(id)
	for _, n := range []string{fVMState, fMemory} {
		if err := os.Remove(filepath.Join(w, n)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// removeCgroup deletes the (empty) per-VM cgroup the jailer created.
func (d *Driver) removeCgroup(id string) {
	if d.cgroupsOK {
		_ = os.Remove(filepath.Join(cgroupRoot, d.cfg.Jailer.ParentCgroup, id))
	}
}

func (d *Driver) Remove(id string) error {
	w := d.Workdir(id)
	d.unexposeAll(w)
	d.removeCgroup(id)
	if d.cfg.Jailer.Enabled {
		return os.RemoveAll(filepath.Dir(w)) // <hot>/jail/firecracker/<id>
	}
	return os.RemoveAll(w)
}

// expose makes src available at w/name: a read-only bind mount when jailed
// (symlinks can't point outside the chroot), otherwise a symlink.
func (d *Driver) expose(src, w, name string) error {
	dst := filepath.Join(w, name)
	if !d.cfg.Jailer.Enabled {
		_ = os.Remove(dst)
		return os.Symlink(src, dst)
	}
	_ = unix.Unmount(dst, unix.MNT_DETACH)
	_ = os.Remove(dst)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	f.Close()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", src, err)
	}
	if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount ro %s: %w", dst, err)
	}
	return nil
}

func (d *Driver) unexposeAll(w string) {
	for _, n := range []string{fRootfs, fKernel, fBaseVM, fBaseMem} {
		p := filepath.Join(w, n)
		if d.cfg.Jailer.Enabled {
			_ = unix.Unmount(p, unix.MNT_DETACH)
		}
		_ = os.Remove(p)
	}
}

func (d *Driver) chown(paths ...string) {
	if !d.cfg.Jailer.Enabled {
		return
	}
	for _, p := range paths {
		_ = os.Lchown(p, d.cfg.Jailer.UID, d.cfg.Jailer.GID)
	}
}

// Start boots or restores a VM. See vmm.Driver.
func (d *Driver) Start(ctx context.Context, spec vmm.StartSpec, mode vmm.Mode) (vmm.VM, vmm.StartInfo, error) {
	var info vmm.StartInfo
	w := d.Workdir(spec.ID)
	if err := os.MkdirAll(w, 0o755); err != nil {
		return nil, info, err
	}
	// Leftovers from a previous run of this jail id would make the jailer fail.
	// That includes the pid file the jailer writes (<exec name>.pid), which
	// survives a suspend and blocks the next resume in the same jail.
	for _, n := range []string{fVsock, fEgress, fAPISock, "dev", "run", d.execName, d.execName + ".pid"} {
		_ = os.RemoveAll(filepath.Join(w, n))
	}
	d.removeCgroup(spec.ID)
	tdir := spec.Template.Dir
	if err := d.expose(filepath.Join(tdir, "rootfs.ext4"), w, fRootfs); err != nil {
		return nil, info, err
	}
	if err := d.expose(d.cfg.Kernel, w, fKernel); err != nil {
		return nil, info, err
	}

	var snapState, snapMem string
	disk := filepath.Join(w, fDisk)
	wantDisk := int64(spec.DiskMB) << 20
	switch {
	case mode == vmm.ModeNew && spec.Template.HasSnapshot:
		if err := fsutil.SparseCopy(filepath.Join(tdir, "base", fDisk), disk); err != nil {
			return nil, info, fmt.Errorf("clone template disk: %w", err)
		}
		if err := d.expose(filepath.Join(tdir, "base", fVMState), w, fBaseVM); err != nil {
			return nil, info, err
		}
		if err := d.expose(filepath.Join(tdir, "base", fMemory), w, fBaseMem); err != nil {
			return nil, info, err
		}
		snapState, snapMem = fBaseVM, fBaseMem
		info.Restored = true
	case mode == vmm.ModeNew:
		_ = os.Remove(disk)
		if err := makeDisk(ctx, disk, wantDisk); err != nil {
			return nil, info, err
		}
	case mode == vmm.ModeResume && d.HasSnapshot(spec.ID):
		snapState, snapMem = fVMState, fMemory
		info.Restored = true
	case mode == vmm.ModeResume:
		if !exists(disk) {
			return nil, info, fmt.Errorf("sandbox disk missing at %s", disk)
		}
	}
	if st, err := os.Stat(disk); err == nil && st.Size() < wantDisk {
		if err := os.Truncate(disk, wantDisk); err != nil {
			return nil, info, fmt.Errorf("grow disk: %w", err)
		}
		info.GrowDisk = true
	}
	d.chown(w, disk, filepath.Join(w, fVMState), filepath.Join(w, fMemory))

	vm := &fcVM{d: d, dir: w, id: spec.ID, done: make(chan struct{})}
	if spec.Egress != nil {
		ln, err := net.Listen("unix", filepath.Join(w, fEgress))
		if err != nil {
			return nil, info, fmt.Errorf("egress socket: %w", err)
		}
		d.chown(filepath.Join(w, fEgress))
		vm.egress = ln
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go spec.Egress(c)
			}
		}()
	}
	if err := d.launch(vm, spec); err != nil {
		vm.cleanup()
		return nil, info, err
	}
	fail := func(err error) (vmm.VM, vmm.StartInfo, error) {
		_ = vm.Stop()
		return nil, info, err
	}
	api := newAPIClient(filepath.Join(w, fAPISock))
	vm.api = api
	if err := waitForSocket(ctx, filepath.Join(w, fAPISock), vm.done, 10*time.Second); err != nil {
		return fail(err)
	}
	if snapState != "" {
		err := api.do(ctx, "PUT", "/snapshot/load", snapshotLoad{
			SnapshotPath: snapState,
			MemBackend:   memBackend{BackendType: "File", BackendPath: snapMem},
			ResumeVM:     true,
		})
		if err != nil {
			return fail(err)
		}
		if info.GrowDisk {
			// Make the guest re-read the (enlarged) disk size.
			if err := api.do(ctx, "PATCH", "/drives/disk", drivePatch{DriveID: "disk", PathOnHost: fDisk}); err != nil {
				d.log.Warn("disk rescan failed", "sandbox", spec.ID, "err", err)
				info.GrowDisk = false
			}
		}
		if snapState == fVMState {
			// The memory file is mapped privately; unlinking keeps it valid
			// for this VM and avoids restoring a stale snapshot later.
			_ = os.Remove(filepath.Join(w, fVMState))
			_ = os.Remove(filepath.Join(w, fMemory))
		}
		return vm, info, nil
	}
	steps := []struct {
		method, path string
		body         any
	}{
		{"PUT", "/machine-config", machineConfig{VCPUCount: spec.VCPUs, MemSizeMib: spec.MemoryMB}},
		{"PUT", "/boot-source", bootSource{KernelImagePath: fKernel, BootArgs: strings.TrimSpace(d.cfg.BootArgs + " init=" + agentInit)}},
		{"PUT", "/drives/rootfs", drive{DriveID: "rootfs", PathOnHost: fRootfs, IsRootDevice: true, IsReadOnly: true}},
		{"PUT", "/drives/disk", drive{DriveID: "disk", PathOnHost: fDisk}},
		{"PUT", "/vsock", vsockDev{GuestCID: guest.GuestCID, UDSPath: fVsock}},
	}
	for _, s := range steps {
		if err := api.do(ctx, s.method, s.path, s.body); err != nil {
			return fail(err)
		}
	}
	// virtio-rng gives the guest entropy early in boot; ignore if unsupported.
	if err := api.do(ctx, "PUT", "/entropy", struct{}{}); err != nil {
		d.log.Debug("entropy device unavailable", "err", err)
	}
	if err := api.do(ctx, "PUT", "/actions", action{ActionType: "InstanceStart"}); err != nil {
		return fail(err)
	}
	info.GrowDisk = false // a fresh boot sees the full disk anyway
	return vm, info, nil
}

func makeDisk(ctx context.Context, path string, size int64) error {
	if err := fsutil.CreateSparse(path, size); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", "-L", "sandbox",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard", path).CombinedOutput()
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return nil
}

func (d *Driver) launch(vm *fcVM, spec vmm.StartSpec) error {
	var cmd *exec.Cmd
	if d.cfg.Jailer.Enabled {
		j := d.cfg.Jailer
		args := []string{
			"--id", spec.ID,
			"--exec-file", d.cfg.Binary,
			"--uid", strconv.Itoa(j.UID),
			"--gid", strconv.Itoa(j.GID),
			"--chroot-base-dir", filepath.Join(d.hot, "jail"),
		}
		if d.cgroupsOK {
			memMax := int64(spec.MemoryMB+j.MemoryOverheadMB) << 20
			args = append(args,
				"--cgroup-version", "2",
				"--parent-cgroup", j.ParentCgroup,
				"--cgroup", "memory.max="+strconv.FormatInt(memMax, 10),
				"--cgroup", fmt.Sprintf("cpu.max=%d 100000", spec.VCPUs*100000),
			)
		}
		args = append(args, "--", "--api-sock", "/"+fAPISock)
		cmd = exec.Command(j.Binary, args...)
	} else {
		cmd = exec.Command(d.cfg.Binary, "--api-sock", fAPISock)
		cmd.Dir = vm.dir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if spec.LogPath != "" {
		f, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err == nil {
			fmt.Fprintf(f, "\n==== %s start %s ====\n", time.Now().Format(time.RFC3339), spec.ID)
			cmd.Stdout, cmd.Stderr = f, f
			defer f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	vm.cmd = cmd
	go func() {
		_ = cmd.Wait()
		vm.cleanup()
		close(vm.done)
	}()
	return nil
}

func waitForSocket(ctx context.Context, path string, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errors.New("firecracker exited during startup (see the sandbox log)")
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker API socket %s did not appear", path)
}

type fcVM struct {
	d      *Driver
	id     string
	dir    string
	cmd    *exec.Cmd
	api    *apiClient
	egress net.Listener
	done   chan struct{}
	once   sync.Once
}

func (v *fcVM) cleanup() {
	v.once.Do(func() {
		if v.egress != nil {
			v.egress.Close()
		}
		_ = os.Remove(filepath.Join(v.dir, fAPISock))
	})
}

func (v *fcVM) Done() <-chan struct{} { return v.done }

// Dial connects to the guest agent through Firecracker's vsock UDS:
// write "CONNECT <port>\n", expect "OK <hostport>\n".
func (v *fcVM) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", filepath.Join(v.dir, fVsock))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", guest.AgentPort); err != nil {
		c.Close()
		return nil, err
	}
	line, err := readLine(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock handshake: unexpected %q", line)
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// readLine reads up to '\n' one byte at a time so no payload is consumed.
func readLine(r io.Reader) (string, error) {
	var b [1]byte
	var sb strings.Builder
	for sb.Len() < 64 {
		if _, err := r.Read(b[:]); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b[0])
	}
	return "", errors.New("handshake line too long")
}

func (v *fcVM) Snapshot(ctx context.Context) error {
	if err := v.api.do(ctx, "PATCH", "/vm", vmState{State: "Paused"}); err != nil {
		return err
	}
	tmpState, tmpMem := fVMState+".tmp", fMemory+".tmp"
	_ = os.Remove(filepath.Join(v.dir, tmpState))
	_ = os.Remove(filepath.Join(v.dir, tmpMem))
	err := v.api.do(ctx, "PUT", "/snapshot/create", snapshotCreate{
		SnapshotType: "Full", SnapshotPath: tmpState, MemFilePath: tmpMem,
	})
	if err != nil {
		// Leave the VM running rather than losing it.
		_ = v.api.do(context.Background(), "PATCH", "/vm", vmState{State: "Resumed"})
		return err
	}
	if err := v.Stop(); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(v.dir, tmpMem), filepath.Join(v.dir, fMemory)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(v.dir, tmpState), filepath.Join(v.dir, fVMState))
}

func (v *fcVM) Stop() error {
	if v.cmd != nil && v.cmd.Process != nil {
		_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
		_ = v.cmd.Process.Kill()
	}
	select {
	case <-v.done:
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("firecracker did not exit")
	}
}
