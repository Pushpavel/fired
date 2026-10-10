// Package api contains the wire types of the fired HTTP API. It is shared by
// the daemon and the Go client in pkg/client.
package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// SandboxState is the lifecycle state of a sandbox.
//
//	creating ─▶ running ─▶ suspending ─▶ suspended ─▶ archiving ─▶ archived
//	              ▲                          │                        │
//	              └──────── resuming ◀───────┴────────────────────────┘
//
// "suspended" is the hot tier (memory snapshot on fast storage), "archived" is
// the cold tier (compressed snapshot on the cold disk). "stopped" means the VM
// exited without a snapshot; the disk survives and the next resume cold-boots.
type SandboxState string

const (
	StateCreating   SandboxState = "creating"
	StateRunning    SandboxState = "running"
	StateSuspending SandboxState = "suspending"
	StateSuspended  SandboxState = "suspended"
	StateArchiving  SandboxState = "archiving"
	StateArchived   SandboxState = "archived"
	StateResuming   SandboxState = "resuming"
	StateStopped    SandboxState = "stopped"
	StateDeleting   SandboxState = "deleting"
	StateDeleted    SandboxState = "deleted"
	StateError      SandboxState = "error"
)

// Duration is a time.Duration that marshals to/from JSON as a Go duration
// string ("10m", "2h30m"). Plain numbers are accepted as seconds. "0" or ""
// means disabled.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("duration must be a string like \"10m\" or a number of seconds")
		}
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	return d.parse(s)
}

func (d *Duration) UnmarshalText(b []byte) error { return d.parse(string(b)) }

func (d *Duration) parse(s string) error {
	if s == "" {
		*d = 0
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// TTL controls automatic lifecycle transitions. Zero values mean "use the
// daemon default"; a negative value ("-1s") disables that transition.
type TTL struct {
	// IdleSuspend: suspend (hot snapshot) after this long without activity.
	IdleSuspend Duration `json:"idle_suspend,omitempty" yaml:"idle_suspend"`
	// ArchiveAfter: move a suspended sandbox to cold storage after this long suspended.
	ArchiveAfter Duration `json:"archive_after,omitempty" yaml:"archive_after"`
	// DeleteAfter: delete the sandbox after this long without activity.
	DeleteAfter Duration `json:"delete_after,omitempty" yaml:"delete_after"`
}

// EgressPolicy lists outbound destinations the sandbox may reach through the
// egress proxy. Patterns: "github.com", "*.github.com", "host:port", "*".
type EgressPolicy struct {
	Allow []string `json:"allow"`
}

type CreateSandboxRequest struct {
	// ID is optional; when set it must be unique and match [a-z0-9-]{3,48}.
	ID       string            `json:"id,omitempty"`
	Template string            `json:"template"`
	VCPUs    int               `json:"vcpus,omitempty"`
	MemoryMB int               `json:"memory_mb,omitempty"`
	DiskMB   int               `json:"disk_mb,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	TTL      TTL               `json:"ttl,omitempty"`
	Egress   *EgressPolicy     `json:"egress,omitempty"`
	// Redact controls whether injected secret values are scrubbed from
	// command output. Defaults to the daemon setting.
	Redact *bool `json:"redact,omitempty"`
}

type UpdateSandboxRequest struct {
	Labels map[string]string `json:"labels,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
	TTL    *TTL              `json:"ttl,omitempty"`
	Egress *EgressPolicy     `json:"egress,omitempty"`
	Redact *bool             `json:"redact,omitempty"`
}

type Sandbox struct {
	ID             string            `json:"id"`
	Template       string            `json:"template"`
	TemplateID     string            `json:"template_id"`
	State          SandboxState      `json:"state"`
	VCPUs          int               `json:"vcpus"`
	MemoryMB       int               `json:"memory_mb"`
	DiskMB         int               `json:"disk_mb"`
	Env            map[string]string `json:"env,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	TTL            TTL               `json:"ttl"`
	Egress         EgressPolicy      `json:"egress"`
	Redact         bool              `json:"redact"`
	Ports          []int             `json:"ports"`
	CreatedAt      time.Time         `json:"created_at"`
	LastActiveAt   time.Time         `json:"last_active_at"`
	StateChangedAt time.Time         `json:"state_changed_at"`
	Error          string            `json:"error,omitempty"`
}

type SandboxList struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

// ExecRequest runs a command inside the sandbox.
type ExecRequest struct {
	// Cmd is run with /bin/sh -c. Either Cmd or Args must be set.
	Cmd  string            `json:"cmd,omitempty"`
	Args []string          `json:"args,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// SecretEnv is merged into the environment but never logged or persisted,
	// and its values are redacted from output when redaction is enabled.
	SecretEnv map[string]string `json:"secret_env,omitempty"`
	// SecretFiles are written to a per-exec directory exposed as $SECRET_DIR.
	// "$SECRET_DIR" inside Env/SecretEnv values is expanded to that directory.
	SecretFiles map[string]string `json:"secret_files,omitempty"`
	User        string            `json:"user,omitempty"`
	Stdin       string            `json:"stdin,omitempty"`
	// TimeoutS applies to foreground commands (default 300, 0 = default).
	TimeoutS int `json:"timeout_s,omitempty"`
	// Background starts the process and returns immediately with its PID.
	Background bool `json:"background,omitempty"`
	// PTY/Cols/Rows are used by the streaming endpoint only.
	PTY  bool `json:"pty,omitempty"`
	Cols int  `json:"cols,omitempty"`
	Rows int  `json:"rows,omitempty"`
}

type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	// PID is set for background executions (and Stdout/Stderr are empty).
	PID int `json:"pid,omitempty"`
}

type Process struct {
	PID        int        `json:"pid"`
	Cmd        string     `json:"cmd"`
	Background bool       `json:"background"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	ExitedAt   *time.Time `json:"exited_at,omitempty"`
}

type ProcessList struct {
	Processes []Process `json:"processes"`
}

// ProcessOutput is the retained output of a process. Offsets are absolute
// byte counts; pass them back as since_stdout/since_stderr to read only new
// output. If the requested offset has already been dropped from the buffer,
// output starts at the oldest retained byte and Dropped is true.
type ProcessOutput struct {
	PID          int    `json:"pid"`
	Running      bool   `json:"running"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	StdoutOffset int64  `json:"stdout_offset"`
	StderrOffset int64  `json:"stderr_offset"`
	Dropped      bool   `json:"dropped,omitempty"`
}

type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"is_dir"`
	IsLink  bool      `json:"is_link,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

type DirListing struct {
	Path    string     `json:"path"`
	Entries []FileInfo `json:"entries"`
}

// FileManifest lists the regular files under a path with their SHA-256,
// returned by GET /files/manifest.
type FileManifest struct {
	Path  string          `json:"path"`
	Files []ManifestEntry `json:"files"`
}

type ManifestEntry struct {
	// Path is relative to FileManifest.Path, slash-separated.
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

// FileScan is a manifest of the regular files under a path whose hashes were
// computed over the same bytes that were checked for the sandbox's secret
// values. Returned by GET /files/scan. A caller that downloads the files and
// checks each against its SHA256 gets exactly what was scanned.
type FileScan struct {
	// Path is the scanned path as resolved in the guest.
	Path string `json:"path"`
	// Base is the directory entry paths are relative to: Path itself, or
	// its parent when Path is a file.
	Base  string          `json:"base"`
	Files []ManifestEntry `json:"files"`
	// SecretsChecked is how many secrets (by name) the files were checked
	// against: those passed to this sandbox's commands since the daemon
	// started. 0 means nothing was checked, e.g. redaction is off.
	SecretsChecked int           `json:"secrets_checked"`
	Found          []SecretMatch `json:"found"`
}

// SecretMatch is a file that contains a secret (or an encoding of it). Only
// the secret's name is reported, never its value.
type SecretMatch struct {
	Path   string `json:"path"`
	Secret string `json:"secret"`
}

type MoveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ExposePortRequest struct {
	Port int `json:"port"`
}

type PortList struct {
	Ports []int `json:"ports"`
}

type TemplateStatus string

const (
	TemplateBuilding TemplateStatus = "building"
	TemplateReady    TemplateStatus = "ready"
	TemplateFailed   TemplateStatus = "failed"
)

type CreateTemplateRequest struct {
	Name string `json:"name"`
	// Image is an OCI image reference, e.g. "node:22-bookworm".
	Image string `json:"image"`
	// RootfsMB is the size of the read-only root filesystem image (default: image size + 30%).
	RootfsMB int `json:"rootfs_mb,omitempty"`
	// DiskMB is the default writable disk size for sandboxes of this template.
	DiskMB int `json:"disk_mb,omitempty"`
	// Snapshot boots the template once and stores a memory snapshot so new
	// sandboxes start by restoring it (much faster than a cold boot). Default true.
	Snapshot *bool `json:"snapshot,omitempty"`
	// VCPUs/MemoryMB used for the base snapshot. Sandboxes created from a
	// snapshotted template always get these values.
	VCPUs    int               `json:"vcpus,omitempty"`
	MemoryMB int               `json:"memory_mb,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Workdir  string            `json:"workdir,omitempty"`
	// Run is an optional list of shell commands executed inside the booted
	// template before the base snapshot (e.g. warming caches).
	Run []string `json:"run,omitempty"`
}

type Template struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Status      TemplateStatus    `json:"status"`
	Error       string            `json:"error,omitempty"`
	RootfsMB    int               `json:"rootfs_mb"`
	DiskMB      int               `json:"disk_mb"`
	VCPUs       int               `json:"vcpus"`
	MemoryMB    int               `json:"memory_mb"`
	HasSnapshot bool              `json:"has_snapshot"`
	Env         map[string]string `json:"env,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	ReadyAt     *time.Time        `json:"ready_at,omitempty"`
}

type TemplateList struct {
	Templates []Template `json:"templates"`
}

type Event struct {
	Type      string       `json:"type"` // sandbox.state | sandbox.deleted | template.status
	SandboxID string       `json:"sandbox_id,omitempty"`
	State     SandboxState `json:"state,omitempty"`
	PrevState SandboxState `json:"prev_state,omitempty"`
	Template  string       `json:"template,omitempty"`
	Status    string       `json:"status,omitempty"`
	Error     string       `json:"error,omitempty"`
	At        time.Time    `json:"at"`
}

type HostInfo struct {
	Driver           string               `json:"driver"`
	Version          string               `json:"version"`
	MemoryBudgetMB   int                  `json:"memory_budget_mb"`
	MemoryRunningMB  int                  `json:"memory_running_mb"`
	MaxSandboxes     int                  `json:"max_sandboxes"`
	Counts           map[SandboxState]int `json:"counts"`
	HotDirUsedBytes  int64                `json:"hot_dir_used_bytes"`
	ColdDirUsedBytes int64                `json:"cold_dir_used_bytes"`
	HotDirFreeBytes  int64                `json:"hot_dir_free_bytes"`
	ColdDirFreeBytes int64                `json:"cold_dir_free_bytes"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Err ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Err.Code + ": " + e.Err.Message }

// Stream protocol (WebSocket at /v1/sandboxes/{id}/exec/stream):
//
//	client → server, first message: text frame containing an ExecRequest.
//	client → server, afterwards:
//	    binary frame: stdin bytes
//	    text frame:   StreamControl ({"type":"resize","cols":..,"rows":..},
//	                  {"type":"signal","signal":"INT"}, {"type":"eof"})
//	server → client:
//	    binary frame: first byte is the channel (1 = stdout, 2 = stderr),
//	                  followed by output bytes
//	    text frame:   StreamEvent ({"type":"started","pid":..} then
//	                  {"type":"exit","exit_code":..} or {"type":"error","message":..})
const (
	StreamStdout byte = 1
	StreamStderr byte = 2
)

type StreamControl struct {
	Type   string `json:"type"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Signal string `json:"signal,omitempty"`
}

type StreamEvent struct {
	Type     string `json:"type"`
	PID      int    `json:"pid,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Message  string `json:"message,omitempty"`
}
