// Package artifacts contains the wire types of the fired-artifacts HTTP API
// and a Go client for it. See docs/artifacts.md.
package artifacts

import (
	"encoding/json"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
)

// Duration is a Go duration string on the wire ("10m", "12h").
type Duration = api.Duration

// ---- artifact.json

// SpecFile is the path of the config file inside an artifact's file tree.
const SpecFile = "artifact.json"

// Spec is the content of artifact.json: how the files are viewed and which
// jobs run against them. It is versioned with the files.
type Spec struct {
	Views []View         `json:"views,omitempty"`
	Jobs  map[string]Job `json:"jobs,omitempty"`
}

// View says how one file is presented. The service serves raw files; the
// view tells the viewer (your frontend) how to render them.
type View struct {
	// Type is one of: html, react, md, csv, json, text, image, svg, pdf.
	Type  string `json:"type"`
	Entry string `json:"entry"`
}

// Job is a command run against the artifact's files on a schedule or on
// demand. A successful run's changes under Writes become a new version.
type Job struct {
	// Run is executed with /bin/sh -c in the artifact's directory.
	Run string `json:"run"`
	// Writes lists the paths (files or directories) the job may change.
	// Everything else in the tree is read-only to the job.
	Writes []string `json:"writes,omitempty"`
	// State is a directory carried from one run to the next. It is not part
	// of the artifact's files and is not copied when the artifact is forked.
	State string `json:"state,omitempty"`
	// Schedule is a 5-field cron expression (or @hourly, @daily, @weekly,
	// @monthly, @yearly). Empty means the job only runs when triggered.
	Schedule string `json:"schedule,omitempty"`
	// Timezone is the IANA zone the schedule is evaluated in (default UTC).
	Timezone string `json:"timezone,omitempty"`
	// Timeout bounds one run. Above the service's default max run it needs
	// approval (a grant) before the job can run.
	Timeout Duration `json:"timeout,omitempty"`
	// ActiveFor is how long the schedule stays active after a publish.
	// Above the service default it needs approval; until then the default
	// window applies.
	ActiveFor Duration `json:"active_for,omitempty"`
	// OnBusy decides what happens to a trigger that can't start yet:
	// "latest" keeps only the newest pending run, "queue" keeps them all.
	OnBusy string     `json:"on_busy,omitempty"`
	Queue  *QueueSpec `json:"queue,omitempty"`
	// Template is the fired template the runner sandbox is created from.
	Template string `json:"template,omitempty"`
}

type QueueSpec struct {
	Max    int      `json:"max,omitempty"`
	MaxAge Duration `json:"max_age,omitempty"`
}

const (
	OnBusyLatest = "latest"
	OnBusyQueue  = "queue"
)

// ---- artifacts and versions

type VersionRef struct {
	Artifact string `json:"artifact"`
	Version  int    `json:"version"`
}

type Artifact struct {
	ID       string            `json:"id"`
	QuotaKey string            `json:"quota_key"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Meta is opaque JSON owned by the caller (title, description, …).
	Meta json.RawMessage `json:"meta,omitempty"`
	// CurrentVersion is 0 until the first publish.
	CurrentVersion int `json:"current_version"`
	// DeleteAfter deletes the artifact after this long without reads,
	// publishes or runs. 0 = service default, negative = never.
	DeleteAfter Duration `json:"delete_after"`
	// RetainRuns is how many run-produced versions to keep. 0 = service default.
	RetainRuns   int         `json:"retain_runs"`
	ForkedFrom   *VersionRef `json:"forked_from,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	LastActiveAt time.Time   `json:"last_active_at"`
}

type ArtifactList struct {
	Artifacts []Artifact `json:"artifacts"`
}

// File is one entry of a version's tree. Mode is 0644 or 0755.
type File struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

const (
	OriginEdit    = "edit"
	OriginRun     = "run"
	OriginRestore = "restore"
	OriginFork    = "fork"
)

type Version struct {
	Artifact  string            `json:"artifact"`
	Version   int               `json:"version"`
	Parent    int               `json:"parent,omitempty"`
	Origin    string            `json:"origin"`
	Label     string            `json:"label,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	RunID     string            `json:"run_id,omitempty"`
	Job       string            `json:"job,omitempty"`
	FileCount int               `json:"file_count"`
	SizeBytes int64             `json:"size_bytes"`
	// Spec is the effective artifact.json (defaults applied when absent).
	Spec      Spec      `json:"spec"`
	Files     []File    `json:"files,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type VersionList struct {
	Versions []Version `json:"versions"`
}

// FileOp is one change in a publish. Exactly one of Hash, Content,
// ContentBase64 or Delete is set.
type FileOp struct {
	Path string `json:"path"`
	// Hash refers to a blob uploaded with PUT /v1/blobs/{hash}.
	Hash          string  `json:"hash,omitempty"`
	Content       *string `json:"content,omitempty"`
	ContentBase64 string  `json:"content_base64,omitempty"`
	Delete        bool    `json:"delete,omitempty"`
	// Mode is 0644 (default) or 0755.
	Mode uint32 `json:"mode,omitempty"`
}

// PublishRequest creates a new version on top of BaseVersion, which must be
// the artifact's current version (0 for the first publish).
type PublishRequest struct {
	BaseVersion int `json:"base_version"`
	// Replace starts from an empty tree instead of the base version's files.
	Replace bool `json:"replace,omitempty"`
	// ReplaceDirs empties these directories before Files are applied, so a
	// folder can be mirrored exactly (files that disappeared are removed).
	ReplaceDirs []string `json:"replace_dirs,omitempty"`
	// Files are applied in order; later entries win for the same path.
	Files []FileOp `json:"files,omitempty"`
	// Spec, when set, is written to artifact.json. Passing both Spec and an
	// artifact.json file is an error.
	Spec   *Spec             `json:"spec,omitempty"`
	Label  string            `json:"label,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	// RunNow triggers these jobs as soon as the version is published.
	RunNow []string `json:"run_now,omitempty"`
	// Notify is an opaque target (e.g. an agent session). The first run of
	// this version emits its run.finished event with this value set.
	Notify string `json:"notify,omitempty"`
}

type PublishResult struct {
	Version Version     `json:"version"`
	Jobs    []JobStatus `json:"jobs,omitempty"`
}

type CreateArtifactRequest struct {
	// ID is optional; when set it must match [a-z0-9-]{3,48} and be unique.
	ID          string            `json:"id,omitempty"`
	QuotaKey    string            `json:"quota_key"`
	Labels      map[string]string `json:"labels,omitempty"`
	Meta        json.RawMessage   `json:"meta,omitempty"`
	DeleteAfter Duration          `json:"delete_after,omitempty"`
	RetainRuns  int               `json:"retain_runs,omitempty"`
	// Publish, when set, publishes the first version in the same call.
	Publish *PublishRequest `json:"publish,omitempty"`
}

type CreateArtifactResult struct {
	Artifact Artifact       `json:"artifact"`
	Publish  *PublishResult `json:"publish,omitempty"`
}

type UpdateArtifactRequest struct {
	QuotaKey    *string           `json:"quota_key,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Meta        json.RawMessage   `json:"meta,omitempty"`
	DeleteAfter *Duration         `json:"delete_after,omitempty"`
	RetainRuns  *int              `json:"retain_runs,omitempty"`
}

type RestoreRequest struct {
	Version     int    `json:"version"`
	BaseVersion int    `json:"base_version"`
	Label       string `json:"label,omitempty"`
}

type ForkRequest struct {
	// Version defaults to the current version.
	Version  int               `json:"version,omitempty"`
	ID       string            `json:"id,omitempty"`
	QuotaKey string            `json:"quota_key"`
	Labels   map[string]string `json:"labels,omitempty"`
	Meta     json.RawMessage   `json:"meta,omitempty"`
}

type MissingBlobsRequest struct {
	Hashes []string `json:"hashes"`
}

type MissingBlobsResult struct {
	Missing []string `json:"missing"`
}

// ---- jobs and runs

// Grant is what the owner approved for a job beyond the service defaults.
type Grant struct {
	// ActiveUntil keeps the schedule active until this time.
	ActiveUntil time.Time `json:"active_until"`
	// MaxRun allows runs up to this long.
	MaxRun Duration `json:"max_run,omitempty"`
}

type JobStatus struct {
	Name string `json:"name"`
	// State: "scheduled" (schedule active), "manual" (no schedule, can be
	// triggered), "expired" (schedule window passed), "blocked" (needs
	// approval before it can run at all).
	State       string     `json:"state"`
	Schedule    string     `json:"schedule,omitempty"`
	ActiveUntil *time.Time `json:"active_until,omitempty"`
	MaxRun      Duration   `json:"max_run"`
	NextRun     *time.Time `json:"next_run,omitempty"`
	Pending     int        `json:"pending"`
	// NeedsApproval lists what exceeds the defaults and has no grant yet.
	NeedsApproval []string `json:"needs_approval,omitempty"`
}

type JobList struct {
	Jobs []JobStatus `json:"jobs"`
}

const (
	JobScheduled = "scheduled"
	JobManual    = "manual"
	JobExpired   = "expired"
	JobBlocked   = "blocked"
)

type TriggerRequest struct {
	// Payload is handed to the run in $TRIGGER_FILE.
	Payload json.RawMessage `json:"payload,omitempty"`
}

const (
	RunPending    = "pending"
	RunRunning    = "running"
	RunSucceeded  = "succeeded"
	RunFailed     = "failed"
	RunTimedOut   = "timed_out"
	RunDropped    = "dropped"
	RunSuperseded = "superseded"
)

const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

type Run struct {
	ID       string `json:"id"`
	Artifact string `json:"artifact"`
	Job      string `json:"job"`
	Status   string `json:"status"`
	Trigger  string `json:"trigger"`
	// Version is the source version the run used (set when it starts).
	Version int `json:"version,omitempty"`
	// ResultVersion is the version the run produced, if its writes changed
	// anything.
	ResultVersion int        `json:"result_version,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMS    int64      `json:"duration_ms,omitempty"`
}

type RunList struct {
	Runs []Run `json:"runs"`
}

type RunLogs struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ---- quotas

// Quota limits everything that shares a quota key (typically one user).
// Zero fields use the service default; negative disables the limit.
type Quota struct {
	MaxConcurrentRuns int      `json:"max_concurrent_runs,omitempty" yaml:"max_concurrent_runs"`
	RunMinutesPerDay  int      `json:"run_minutes_per_day,omitempty" yaml:"run_minutes_per_day"`
	MinInterval       Duration `json:"min_interval,omitempty" yaml:"min_interval"`
	StorageBytes      int64    `json:"storage_bytes,omitempty" yaml:"storage_bytes"`
}

type QuotaStatus struct {
	Key          string `json:"key"`
	Quota        Quota  `json:"quota"`
	Effective    Quota  `json:"effective"`
	StorageUsed  int64  `json:"storage_used"`
	RunsActive   int    `json:"runs_active"`
	RunMinutes24 int    `json:"run_minutes_24h"`
}

// ---- credentials broker (implemented by your backend)

// CredentialsRequest is POSTed to the broker before every run.
type CredentialsRequest struct {
	Artifact string            `json:"artifact"`
	QuotaKey string            `json:"quota_key"`
	Labels   map[string]string `json:"labels,omitempty"`
	Version  int               `json:"version"`
	Job      string            `json:"job"`
	RunID    string            `json:"run_id"`
}

type CredentialsResponse struct {
	SecretEnv   map[string]string `json:"secret_env,omitempty"`
	SecretFiles map[string]string `json:"secret_files,omitempty"`
}

// ---- events

type Event struct {
	// Type: artifact.created, artifact.version, artifact.deleted,
	// run.started, run.finished, run.dropped.
	Type     string `json:"type"`
	Artifact string `json:"artifact"`
	Version  int    `json:"version,omitempty"`
	Origin   string `json:"origin,omitempty"`
	Job      string `json:"job,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Status   string `json:"status,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Notify is copied from the publish that asked to hear about this
	// version's first run.
	Notify string    `json:"notify,omitempty"`
	At     time.Time `json:"at"`
}
