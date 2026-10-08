package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Pushpavel/fired/internal/fsutil"
	"github.com/Pushpavel/fired/internal/rootfs"
	"github.com/Pushpavel/fired/internal/vmm"
	"github.com/Pushpavel/fired/pkg/api"
)

// SetInsecureRegistries configures registries pulled over plain HTTP.
func (d *Driver) SetInsecureRegistries(hosts []string) { d.insecure = hosts }

func (d *Driver) BuildTemplate(ctx context.Context, req api.CreateTemplateRequest, t *vmm.Template, logw io.Writer) error {
	if _, err := os.Stat(d.cfg.AgentBinary); err != nil {
		return fmt.Errorf("agent binary: %w", err)
	}
	if _, err := os.Stat(d.cfg.Kernel); err != nil {
		return fmt.Errorf("guest kernel %s: %w (see scripts/build-kernel.sh)", d.cfg.Kernel, err)
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	_, err := rootfs.Build(ctx, rootfs.Options{
		Image:              req.Image,
		InsecureRegistries: d.insecure,
		AgentBinary:        d.cfg.AgentBinary,
		SizeMB:             req.RootfsMB,
		Env:                req.Env,
		Workdir:            req.Workdir,
		Log:                logw,
	}, filepath.Join(t.Dir, "staging"), filepath.Join(t.Dir, "rootfs.ext4"))
	return err
}

// SaveTemplateSnapshot moves disk + memory snapshot of the template build VM
// into t.Dir/base. The memory file is shared read-only by all clones.
func (d *Driver) SaveTemplateSnapshot(vmID string, t *vmm.Template) error {
	w := d.Workdir(vmID)
	base := filepath.Join(t.Dir, "base")
	_ = os.RemoveAll(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	for _, n := range []string{fDisk, fVMState, fMemory} {
		src, dst := filepath.Join(w, n), filepath.Join(base, n)
		if err := os.Rename(src, dst); err != nil {
			// Different filesystems (hot tmpfs → templates on disk).
			if err := fsutil.SparseCopy(src, dst); err != nil {
				return fmt.Errorf("save %s: %w", n, err)
			}
		}
		if err := os.Chmod(dst, 0o644); err != nil {
			return err
		}
	}
	return d.Remove(vmID)
}
