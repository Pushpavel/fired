package artifacts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pushpavel/fired/pkg/api"
)

// Config is the fired-artifacts service configuration.
type Config struct {
	// Listen is the API address, e.g. ":8444". Env FIRED_ARTIFACTS_LISTEN.
	Listen string `yaml:"listen"`
	// Token authenticates callers (X-Fired-Token or Bearer). Env
	// FIRED_ARTIFACTS_TOKEN. Required.
	Token string `yaml:"token"`
	// DatabaseURL is the Postgres database for artifacts, versions, jobs and
	// runs, normally fired's: the tables go in their own schema, "artifacts".
	// Env FIRED_ARTIFACTS_DATABASE_URL. Required.
	DatabaseURL string `yaml:"database_url"`
	// DataDir holds the blob store (file contents). Put it on durable
	// (ideally replicated) storage: artifacts are kept work, unlike
	// sandboxes. Env FIRED_ARTIFACTS_DATA_DIR.
	DataDir string `yaml:"data_dir"`
	// Fired is the sandbox daemon jobs run on. Env FIRED_URL, FIRED_API_TOKEN.
	Fired FiredConfig `yaml:"fired"`
	// Broker is your backend's credentials endpoint, called before every
	// run. Empty URL runs jobs without credentials.
	// Env FIRED_ARTIFACTS_BROKER_URL, FIRED_ARTIFACTS_BROKER_TOKEN.
	Broker BrokerConfig `yaml:"broker"`
	Limits LimitsConfig `yaml:"limits"`
	Runner RunnerConfig `yaml:"runner"`
	// SchedulerInterval is how often schedules, queues and limits are checked.
	SchedulerInterval api.Duration `yaml:"scheduler_interval"`
	// GCInterval is how often expired artifacts, old run versions and
	// unreferenced blobs are removed.
	GCInterval api.Duration `yaml:"gc_interval"`
	LogLevel   string       `yaml:"log_level"`
	// UI serves a browser console at /ui/ for trying the API by hand.
	// Login uses the API token. Env FIRED_ARTIFACTS_UI=1.
	UI bool `yaml:"ui"`
	// PreviewDomain is a wildcard DNS name pointing at this service; the
	// console shows artifact files on {id}-{version}-{sig}.<domain>. Empty
	// uses nip.io for the IP the console is reached on.
	// Env FIRED_ARTIFACTS_PREVIEW_DOMAIN.
	PreviewDomain string `yaml:"preview_domain"`
}

type FiredConfig struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type BrokerConfig struct {
	URL     string       `yaml:"url"`
	Token   string       `yaml:"token"`
	Timeout api.Duration `yaml:"timeout"`
}

type LimitsConfig struct {
	MaxFilesPerVersion int   `yaml:"max_files_per_version"`
	MaxVersionBytes    int64 `yaml:"max_version_bytes"`
	// MaxInlineBytes caps content / content_base64 in a publish request.
	MaxInlineBytes int64 `yaml:"max_inline_bytes"`
	// DefaultActiveFor is how long a schedule stays active after a publish
	// without approval (like a sandbox's idle timer, publishing re-arms it).
	DefaultActiveFor api.Duration `yaml:"default_active_for"`
	// DefaultMaxRun is the longest run allowed without approval.
	DefaultMaxRun api.Duration `yaml:"default_max_run"`
	// MaxConcurrentRuns caps runs across the whole service.
	MaxConcurrentRuns int `yaml:"max_concurrent_runs"`
	// MinInterval is the shortest schedule interval for anyone.
	MinInterval api.Duration `yaml:"min_interval"`
	// ServeRPS / ServeBurst rate-limit file reads per artifact.
	ServeRPS   float64 `yaml:"serve_rps"`
	ServeBurst int     `yaml:"serve_burst"`
	// RetainOutputs is how many of the job's outputs each artifact keeps,
	// the current one included.
	RetainOutputs int `yaml:"retain_outputs"`
	// DeleteAfter deletes artifacts with no reads, publishes or runs for
	// this long. 0 disables it.
	DeleteAfter api.Duration `yaml:"delete_after"`
	// QueueMax / QueueMaxAge are defaults for on_busy "queue" jobs.
	QueueMax    int          `yaml:"queue_max"`
	QueueMaxAge api.Duration `yaml:"queue_max_age"`
	// MaxPayloadBytes caps a trigger payload.
	MaxPayloadBytes int `yaml:"max_payload_bytes"`
}

type RunnerConfig struct {
	// Template is the fired template for jobs that don't name one.
	Template string `yaml:"template"`
	// Pool is the fired pool runner sandboxes run in. They carry the
	// artifact's quota key, so fired enforces each user's compute limits
	// in this pool (separately from their other sandboxes).
	Pool string `yaml:"pool"`
	// QuotaBackoff is how long a quota key's runs wait after fired refused
	// one for being over that key's limits.
	QuotaBackoff api.Duration `yaml:"quota_backoff"`
	// Workdir is where the artifact's files are placed in the sandbox.
	Workdir  string `yaml:"workdir"`
	VCPUs    int    `yaml:"vcpus"`
	MemoryMB int    `yaml:"memory_mb"`
	DiskMB   int    `yaml:"disk_mb"`
	// Egress is the allowlist for runner sandboxes.
	Egress []string `yaml:"egress"`
	// TTL for runner sandboxes. They are only a cache: the service
	// recreates one from the stored files when it's gone.
	TTL api.TTL `yaml:"ttl"`
	// LogBytes is how much of each stream's output a run keeps.
	LogBytes int `yaml:"log_bytes"`
}

func DefaultConfig() Config {
	return Config{
		Listen:  ":8444",
		DataDir: "/data/artifacts",
		Fired:   FiredConfig{URL: "http://127.0.0.1:8443"},
		Broker:  BrokerConfig{Timeout: api.Duration(10 * time.Second)},
		Limits: LimitsConfig{
			MaxFilesPerVersion: 5000,
			MaxVersionBytes:    256 << 20,
			MaxInlineBytes:     4 << 20,
			DefaultActiveFor:   api.Duration(12 * time.Hour),
			DefaultMaxRun:      api.Duration(30 * time.Minute),
			MaxConcurrentRuns:  8,
			MinInterval:        api.Duration(time.Minute),
			ServeRPS:           50,
			ServeBurst:         100,
			RetainOutputs:      24,
			DeleteAfter:        0,
			QueueMax:           100,
			QueueMaxAge:        api.Duration(24 * time.Hour),
			MaxPayloadBytes:    1 << 20,
		},
		Runner: RunnerConfig{
			Template:     "base",
			Pool:         "artifacts",
			QuotaBackoff: api.Duration(time.Minute),
			Workdir:      "/artifact",
			TTL: api.TTL{
				IdleSuspend:  api.Duration(time.Minute),
				ArchiveAfter: api.Duration(time.Hour),
				DeleteAfter:  api.Duration(48 * time.Hour),
			},
			LogBytes: 256 << 10,
		},
		SchedulerInterval: api.Duration(5 * time.Second),
		GCInterval:        api.Duration(time.Hour),
		LogLevel:          "info",
	}
}

// LoadConfig reads path (if set) over the defaults and applies env overrides.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	env := func(k string, dst *string) {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
	env("FIRED_ARTIFACTS_LISTEN", &cfg.Listen)
	env("FIRED_ARTIFACTS_TOKEN", &cfg.Token)
	env("FIRED_ARTIFACTS_DATA_DIR", &cfg.DataDir)
	env("FIRED_ARTIFACTS_DATABASE_URL", &cfg.DatabaseURL)
	env("FIRED_URL", &cfg.Fired.URL)
	env("FIRED_API_TOKEN", &cfg.Fired.Token)
	env("FIRED_ARTIFACTS_BROKER_URL", &cfg.Broker.URL)
	env("FIRED_ARTIFACTS_BROKER_TOKEN", &cfg.Broker.Token)
	env("FIRED_ARTIFACTS_PREVIEW_DOMAIN", &cfg.PreviewDomain)
	if v := os.Getenv("FIRED_ARTIFACTS_UI"); v != "" {
		cfg.UI = v == "1" || v == "true"
	}
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("database_url is required (FIRED_ARTIFACTS_DATABASE_URL)")
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir must be an absolute path")
	}
	if c.Limits.DefaultActiveFor <= 0 || c.Limits.DefaultMaxRun <= 0 {
		return fmt.Errorf("limits.default_active_for and default_max_run must be positive")
	}
	if c.Limits.MaxFilesPerVersion <= 0 || c.Limits.MaxVersionBytes <= 0 || c.Limits.MaxInlineBytes <= 0 {
		return fmt.Errorf("limits.max_files_per_version, max_version_bytes and max_inline_bytes must be positive")
	}
	if c.Limits.MaxConcurrentRuns <= 0 {
		return fmt.Errorf("limits.max_concurrent_runs must be positive")
	}
	if c.Runner.Workdir == "" || !filepath.IsAbs(c.Runner.Workdir) || c.Runner.Workdir == "/" {
		return fmt.Errorf("runner.workdir must be an absolute path other than /")
	}
	if c.Limits.RetainOutputs <= 0 {
		c.Limits.RetainOutputs = 24
	}
	if c.SchedulerInterval <= 0 {
		c.SchedulerInterval = api.Duration(5 * time.Second)
	}
	if c.GCInterval <= 0 {
		c.GCInterval = api.Duration(time.Hour)
	}
	if c.Runner.Pool == "" {
		c.Runner.Pool = "artifacts"
	}
	if c.Runner.QuotaBackoff <= 0 {
		c.Runner.QuotaBackoff = api.Duration(time.Minute)
	}
	if c.Runner.LogBytes <= 0 {
		c.Runner.LogBytes = 256 << 10
	}
	if c.Broker.Timeout <= 0 {
		c.Broker.Timeout = api.Duration(10 * time.Second)
	}
	return nil
}
