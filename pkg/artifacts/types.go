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

// OutputDir is the job's folder. Runs read and write it; whatever a run
// leaves there is the artifact's output, what viewers see at /.output/ and
// what the next run starts from. It's never part of a version: a publish
// skips files under it.
const OutputDir = ".output"

// SpecFile is the path of the config file inside an artifact's file tree.
// Every version has one: the service writes an empty one when a create
// doesn't bring one.
const SpecFile = "artifact.json"

// Spec is the content of artifact.json: what the artifact is called, how
// it is viewed and the job that keeps its data fresh. It is versioned with
// the files, so it travels with exports, restores and forks.
type Spec struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// View is what viewers see: one entry file and its type. Without a
	// view, there is nothing to view.
	View *View `json:"view,omitempty"`
	// Job is the artifact's one job, if it has one.
	Job *Job `json:"job,omitempty"`
}

// View is an artifact's view. Its Type says who renders it:
//
//   - ViewPage: Entry is an HTML page the agent wrote; it renders itself.
//   - every other type: Entry is one file of that kind (Markdown, a CSV, …)
//     and your frontend renders it, with its own UX (copy, download, …).
//
// Viewers reach the entry and the other files of its folder (a page's
// scripts and other pages, a Markdown file's images), nothing else.
type View struct {
	Type  string `json:"type"`
	Entry string `json:"entry"`
}

// View types.
const (
	ViewPage     = "page"     // .html, .htm
	ViewMarkdown = "markdown" // .md, .markdown
	ViewCSV      = "csv"      // .csv
	ViewJSON     = "json"     // .json
	ViewCode     = "code"     // any text file, shown as source
	ViewSVG      = "svg"      // .svg
	ViewImage    = "image"    // .png, .jpg, .jpeg, .gif, .webp
)

// Job is a command run against the artifact's files on a schedule or on
// demand. It works in OutputDir: each run starts with the latest output
// there, and a successful run that changes it records the next output (not
// a version). Its changes anywhere else are discarded.
type Job struct {
	// Run is executed with /bin/sh -c in the artifact's directory.
	Run string `json:"run"`
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
	// Title and Description are the current version's, from its
	// artifact.json. Change them by publishing.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Meta is opaque JSON owned by your backend. It isn't exported, forked
	// or shown to jobs.
	Meta json.RawMessage `json:"meta,omitempty"`
	// CurrentVersion is the version files are served from. An artifact
	// is created with version 1, so it is never 0.
	CurrentVersion int `json:"current_version"`
	// DeleteAfter deletes the artifact after this long without reads,
	// publishes or runs. 0 = service default, negative = never.
	DeleteAfter Duration `json:"delete_after"`
	// RetainOutputs is how many of the job's outputs to keep, the current
	// one included. 0 = service default.
	RetainOutputs int `json:"retain_outputs"`
	// Output is the current output (0: none yet, .output/ is empty).
	Output       int         `json:"output,omitempty"`
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

// Versions are what the author made; runs record outputs instead.
const (
	OriginEdit    = "edit"
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
// ContentBase64, Delete or FromSandbox is set.
type FileOp struct {
	Path string `json:"path"`
	// Hash refers to a blob uploaded with PUT /v1/blobs/{hash}.
	Hash          string  `json:"hash,omitempty"`
	Content       *string `json:"content,omitempty"`
	ContentBase64 string  `json:"content_base64,omitempty"`
	Delete        bool    `json:"delete,omitempty"`
	// FromSandbox copies a folder (or one file) from a fired sandbox and
	// mirrors it at Path ("" or "." is the artifact's root): files under
	// Path that the source doesn't have are removed. Modes come from the
	// sandbox. The source is checked for the sandbox's secret values first.
	FromSandbox *SandboxSource `json:"from_sandbox,omitempty"`
	// Mode is 0644 (default) or 0755.
	Mode uint32 `json:"mode,omitempty"`
}

// PublishRequest creates a new version on top of BaseVersion, which must be
// the artifact's current version (required, except in a create). Every
// version has at least one file, and an artifact.json: when the files leave
// it out, the current one is kept (on a create, an empty one is written). A publish that leaves the files as they are makes no
// version (PublishResult.Unchanged).
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
	Spec *Spec `json:"spec,omitempty"`
	// Title and Description, when set, change those fields in
	// artifact.json and keep the rest of it (unlike Spec, which replaces
	// it). They win over the files' artifact.json and Spec.
	Title       *string           `json:"title,omitempty"`
	Description *string           `json:"description,omitempty"`
	Label       string            `json:"label,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	// RunNow triggers the job as soon as the version is published.
	RunNow bool `json:"run_now,omitempty"`
	// Notify is an opaque target (e.g. an agent session). The first run of
	// this version emits its run.finished event with this value set.
	Notify string `json:"notify,omitempty"`
}

// SandboxSource is where a from_sandbox op reads from.
type SandboxSource struct {
	// FiredURL is the fired host the sandbox runs on. Empty uses the
	// service's own fired URL. The service's fired token is sent to it, so
	// never pass a URL your backend didn't choose.
	FiredURL string `json:"fired_url,omitempty"`
	ID       string `json:"id"`
	// Path is a folder (or file) in the sandbox, e.g. /workspace/dist.
	Path string `json:"path"`
	// Exclude skips matching base names or relative paths (globs), e.g.
	// node_modules, .git, *.log.
	Exclude []string `json:"exclude,omitempty"`
}

type PublishResult struct {
	Version Version `json:"version"`
	// Unchanged is true when the publish (or restore) would have produced
	// the files of the current version: no version was made, and Version is
	// the current one.
	Unchanged bool `json:"unchanged,omitempty"`
	// Skipped lists files the publish gave under .output/: the job's
	// folder is never part of a version (an exported zip carries it).
	Skipped []string   `json:"skipped,omitempty"`
	Job     *JobStatus `json:"job,omitempty"`
}

type CreateArtifactRequest struct {
	// ID is optional; when set it must match [a-z0-9-]{3,48} and be unique.
	ID            string            `json:"id,omitempty"`
	QuotaKey      string            `json:"quota_key"`
	Labels        map[string]string `json:"labels,omitempty"`
	Meta          json.RawMessage   `json:"meta,omitempty"`
	DeleteAfter   Duration          `json:"delete_after,omitempty"`
	RetainOutputs int               `json:"retain_outputs,omitempty"`
	// Publish is the first version, required: an artifact never exists
	// without files. Its BaseVersion is 0 (or left out).
	Publish *PublishRequest `json:"publish"`
}

type CreateArtifactResult struct {
	Artifact Artifact      `json:"artifact"`
	Publish  PublishResult `json:"publish"`
}

// UpdateArtifactRequest changes an artifact's settings. Omitted fields are
// left alone. Labels and Meta are merged (JSON merge patch, RFC 7386): keys
// given are set, keys set to null are removed, the rest are kept.
type UpdateArtifactRequest struct {
	QuotaKey      *string            `json:"quota_key,omitempty"`
	Labels        map[string]*string `json:"labels,omitempty"`
	Meta          json.RawMessage    `json:"meta,omitempty"`
	DeleteAfter   *Duration          `json:"delete_after,omitempty"`
	RetainOutputs *int               `json:"retain_outputs,omitempty"`
}

type RestoreRequest struct {
	Version     int    `json:"version"`
	BaseVersion int    `json:"base_version"`
	Label       string `json:"label,omitempty"`
}

type ForkRequest struct {
	// Version defaults to the current version.
	Version int `json:"version,omitempty"`
	// Title, when set, is the fork's title in its artifact.json.
	Title    string            `json:"title,omitempty"`
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
	TriggerPublish  = "publish" // run_now
)

type Run struct {
	ID       string `json:"id"`
	Artifact string `json:"artifact"`
	Status   string `json:"status"`
	Trigger  string `json:"trigger"`
	// Version is the version the run used (set when it starts).
	Version int `json:"version,omitempty"`
	// Output is the output the run recorded, if its writes changed anything.
	Output     int        `json:"output,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
}

type RunList struct {
	Runs []Run `json:"runs"`
}

// Output is what one run left in .output/. The latest one is what
// everyone sees there and what the next run starts from; older ones are
// kept for a while (retain_outputs) to look back at.
type Output struct {
	Output    int       `json:"output"`
	RunID     string    `json:"run_id"`
	Version   int       `json:"version"` // the version the run used
	FileCount int       `json:"file_count"`
	SizeBytes int64     `json:"size_bytes"`
	Files     []File    `json:"files,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// OutputList is an artifact's outputs: the current one (0: none) and the
// ones kept, newest first.
type OutputList struct {
	Current int      `json:"current"`
	Outputs []Output `json:"outputs"`
}

type RunLogs struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ---- quotas

// Quota limits what artifacts sharing a quota key (typically one user)
// may store, and how often their schedules fire. Zero fields fall through
// (key override, then its tier, then the default tier); negative disables
// the limit.
//
// Compute (memory, running time) is limited by fired, per quota key in the
// runner pool: runner sandboxes carry the artifact's quota key.
type Quota struct {
	MinInterval  Duration `json:"min_interval,omitempty" yaml:"min_interval"`
	StorageBytes int64    `json:"storage_bytes,omitempty" yaml:"storage_bytes"`
}

// Tier is what a plan means for artifacts: its storage and schedule
// limits. Which tier a quota key is in is fired's single mapping
// (PUT /v1/quotas/{key} on fired); keys without one are in "default".
type Tier struct {
	Name   string `json:"name"`
	Limits Quota  `json:"limits"`
}

type TierList struct {
	Tiers []Tier `json:"tiers"`
}

// DefaultTier applies to quota keys fired has no tier for.
const DefaultTier = "default"

type QuotaStatus struct {
	Key string `json:"key"`
	// Tier is the key's tier, from fired.
	Tier string `json:"tier"`
	// Quota is this key's own override; Effective is what applies.
	Quota     Quota `json:"quota"`
	Effective Quota `json:"effective"`
	// StorageUsed is what counts against storage_bytes: the distinct blobs
	// and chunks the key's versions and outputs reference. StorageLogical is what they'd
	// take with no sharing (every file of every version in full); the
	// difference is what deduplication saves.
	StorageUsed    int64 `json:"storage_used"`
	StorageLogical int64 `json:"storage_logical"`
}

// ---- credentials broker (implemented by your backend)

// CredentialsRequest is POSTed to the broker before every run.
type CredentialsRequest struct {
	Artifact string            `json:"artifact"`
	QuotaKey string            `json:"quota_key"`
	Labels   map[string]string `json:"labels,omitempty"`
	Version  int               `json:"version"`
	RunID    string            `json:"run_id"`
}

type CredentialsResponse struct {
	SecretEnv   map[string]string `json:"secret_env,omitempty"`
	SecretFiles map[string]string `json:"secret_files,omitempty"`
}

// ---- events

type Event struct {
	// Type: artifact.created, artifact.version, artifact.output (a run
	// recorded an output, or the outputs were cleared), artifact.deleted,
	// run.started, run.finished, run.dropped.
	Type     string `json:"type"`
	Artifact string `json:"artifact"`
	Version  int    `json:"version,omitempty"`
	Origin   string `json:"origin,omitempty"`
	Output   int    `json:"output,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Status   string `json:"status,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Notify is copied from the publish that asked to hear about this
	// version's first run.
	Notify string    `json:"notify,omitempty"`
	At     time.Time `json:"at"`
}
