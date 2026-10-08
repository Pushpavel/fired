// Package vmm abstracts the virtual machine backend. The firecracker
// driver runs real microVMs; the fake driver runs the guest agent as a host
// process for development and tests.
package vmm

import (
	"context"
	"io"
	"net"

	"github.com/Pushpavel/fired/pkg/api"
)

// Template describes a built template on disk.
type Template struct {
	ID string
	// Dir is the template directory (rootfs, optional base/ snapshot).
	Dir string
	// HasSnapshot means Dir/base holds a booted snapshot new sandboxes can clone.
	HasSnapshot bool
	VCPUs       int
	MemoryMB    int
	DiskMB      int
}

type Mode int

const (
	// ModeNew creates a new sandbox (cloning the template snapshot if it
	// has one and the requested shape matches).
	ModeNew Mode = iota
	// ModeResume restarts an existing sandbox from its workdir: restores
	// the memory snapshot if present, otherwise cold-boots its disk.
	ModeResume
)

type StartSpec struct {
	ID       string
	Template Template
	VCPUs    int
	MemoryMB int
	DiskMB   int
	// Egress handles outbound proxy connections from the guest (nil = none).
	Egress func(net.Conn)
	// LogPath receives VMM / serial console output.
	LogPath string
}

// StartInfo reports what Start did.
type StartInfo struct {
	// Restored is true when a memory snapshot was loaded (running processes
	// were preserved); false for a cold boot.
	Restored bool
	// GrowDisk is true when the disk was enlarged and the guest should grow
	// its filesystem.
	GrowDisk bool
}

type Driver interface {
	Name() string
	// Workdir is where the sandbox's runtime files live (on the hot tier).
	Workdir(id string) string
	// PersistentFiles are the top-level workdir entries that make up a
	// suspended sandbox (archived to the cold tier).
	PersistentFiles() []string
	Start(ctx context.Context, spec StartSpec, mode Mode) (VM, StartInfo, error)
	// HasSnapshot reports whether the workdir holds a memory snapshot.
	HasSnapshot(id string) bool
	// DiscardSnapshot drops the memory snapshot (keeping the disk), e.g.
	// when it can't be restored after a VMM upgrade.
	DiscardSnapshot(id string) error
	// Remove deletes the workdir and any driver state for id.
	Remove(id string) error
	// BuildTemplate creates the template root filesystem in t.Dir.
	BuildTemplate(ctx context.Context, req api.CreateTemplateRequest, t *Template, logw io.Writer) error
	// SaveTemplateSnapshot moves the snapshot of a stopped VM (built from
	// the template with id vmID) into t.Dir/base.
	SaveTemplateSnapshot(vmID string, t *Template) error
}

type VM interface {
	// Dial opens a connection to the guest agent.
	Dial(ctx context.Context) (net.Conn, error)
	// Snapshot pauses the VM, writes a memory snapshot into the workdir and
	// stops the VMM process.
	Snapshot(ctx context.Context) error
	// Stop kills the VM without saving memory (the disk is kept).
	Stop() error
	// Done is closed when the VMM process exits.
	Done() <-chan struct{}
}
