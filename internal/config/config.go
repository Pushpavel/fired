// Package config loads the daemon configuration from a YAML file and
// environment overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pushpavel/fired/pkg/api"
)

type Config struct {
	// Listen is the API listen address, e.g. ":8443" or "unix:/run/fired.sock".
	Listen string     `yaml:"listen"`
	Auth   AuthConfig `yaml:"auth"`
	TLS    TLSConfig  `yaml:"tls"`
	// DatabaseURL is the Postgres database fired keeps its state in
	// (sandboxes, templates, hosts, quotas). Every host of a cluster uses
	// the same one; only fired reads it. Env FIRED_DATABASE_URL. Required.
	DatabaseURL string        `yaml:"database_url"`
	Cluster     ClusterConfig `yaml:"cluster"`
	// Quotas are per-pool defaults for each quota key (user): e.g.
	// {"chat": {"max_memory_mb": 4096, "gb_hours_per_day": 48}}. A pool
	// without defaults has no limits unless set per key
	// (PUT /v1/quotas/{key}/{pool}).
	Quotas map[string]api.Quota `yaml:"quotas"`
	Data   DataConfig           `yaml:"data"`
	// Driver selects the VM backend: "firecracker" or "fake" (processes on
	// the host, for development and tests only — no isolation).
	Driver      string            `yaml:"driver"`
	Firecracker FirecrackerConfig `yaml:"firecracker"`
	Fake        FakeConfig        `yaml:"fake"`
	Capacity    CapacityConfig    `yaml:"capacity"`
	Defaults    DefaultsConfig    `yaml:"defaults"`
	Egress      EgressConfig      `yaml:"egress"`
	Exec        ExecConfig        `yaml:"exec"`
	// ReaperInterval is how often idle/TTL transitions are evaluated.
	ReaperInterval api.Duration `yaml:"reaper_interval"`
	// ShutdownAction on SIGTERM: "suspend" (hot snapshot; survives container
	// restarts if hot_dir is a host mount) or "archive" (also compress to the
	// cold dir; survives host reboots, slower).
	ShutdownAction string `yaml:"shutdown_action"`
	LogLevel       string `yaml:"log_level"`
	// UI serves a browser console at /ui/ for poking at the API by hand.
	// Login uses the API token; env FIRED_UI=1 enables it.
	UI bool `yaml:"ui"`
	// PreviewDomain is a wildcard DNS name (*.<domain> → fired) for console
	// port previews. Empty uses nip.io for the IP the console is reached on.
	// Env FIRED_PREVIEW_DOMAIN.
	PreviewDomain string `yaml:"preview_domain"`
}

type AuthConfig struct {
	// Token required as "Authorization: Bearer <token>" or "X-Fired-Token".
	// Env FIRED_API_TOKEN overrides. Empty disables token auth (only do
	// this with mTLS or a unix socket).
	Token string `yaml:"token"`
}

type TLSConfig struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	// ClientCA enables mTLS: clients must present a cert signed by this CA.
	ClientCA string `yaml:"client_ca"`
}

// ClusterConfig places this host among the fired hosts sharing the
// database. A single host needs none of it.
type ClusterConfig struct {
	// HostID names this host; it must stay the same across restarts (its
	// sandboxes are recorded under it). Env FIRED_HOST_ID. Default: the
	// machine's hostname. On Kubernetes, the node name.
	HostID string `yaml:"host_id"`
	// AdvertiseURL is how other hosts reach this one's API, e.g.
	// http://10.0.1.5:8443. Env FIRED_ADVERTISE_URL. Default:
	// http://<hostname>:<listen port>.
	AdvertiseURL string `yaml:"advertise_url"`
	// Pools this host runs sandboxes for ("*" = any). Env FIRED_POOLS
	// (comma-separated).
	Pools []string `yaml:"pools"`
	// DefaultPool is the pool of sandboxes created without one.
	DefaultPool string `yaml:"default_pool"`
	// Heartbeat is how often this host records itself as alive; a host
	// silent for HostTimeout is down.
	Heartbeat   api.Duration `yaml:"heartbeat"`
	HostTimeout api.Duration `yaml:"host_timeout"`
}

type DataConfig struct {
	// HotDir holds running sandbox disks and hot snapshots. Put it on tmpfs
	// or SSD. Must be a host mount to survive container restarts.
	HotDir string `yaml:"hot_dir"`
	// ColdDir holds compressed archives of idle sandboxes (HDD is fine).
	ColdDir string `yaml:"cold_dir"`
	// TemplatesDir holds template root filesystems and base snapshots.
	TemplatesDir string `yaml:"templates_dir"`
}

type FirecrackerConfig struct {
	Binary string `yaml:"binary"`
	// Kernel is the uncompressed guest kernel (vmlinux). See scripts/build-kernel.sh.
	Kernel   string       `yaml:"kernel"`
	BootArgs string       `yaml:"boot_args"`
	Jailer   JailerConfig `yaml:"jailer"`
	// AgentBinary is the static fired-agent binary copied into every template rootfs.
	AgentBinary string `yaml:"agent_binary"`
	// BootTimeout bounds waiting for the guest agent after boot/restore.
	BootTimeout api.Duration `yaml:"boot_timeout"`
	// InsecureRegistries are registry hosts pulled over plain HTTP when
	// building templates (e.g. a local "registry.lan:5000").
	InsecureRegistries []string `yaml:"insecure_registries"`
}

type JailerConfig struct {
	Enabled bool   `yaml:"enabled"`
	Binary  string `yaml:"binary"`
	UID     int    `yaml:"uid"`
	GID     int    `yaml:"gid"`
	// Cgroups applies per-VM cpu.max / memory.max via the jailer (cgroup v2).
	Cgroups      bool   `yaml:"cgroups"`
	ParentCgroup string `yaml:"parent_cgroup"`
	// MemoryOverheadMB is added to the guest memory for the VMM process limit.
	MemoryOverheadMB int `yaml:"memory_overhead_mb"`
}

type FakeConfig struct {
	// AgentBinary is the fired-agent binary run as a host process.
	AgentBinary string `yaml:"agent_binary"`
}

type CapacityConfig struct {
	// MaxRunningMemoryMB caps the sum of memory_mb of running sandboxes.
	MaxRunningMemoryMB int `yaml:"max_running_memory_mb"`
	// MaxSandboxes caps the number of sandboxes in any state (0 = unlimited).
	MaxSandboxes int `yaml:"max_sandboxes"`
	// EvictIdle suspends the least recently used idle sandbox when a
	// create/resume would exceed MaxRunningMemoryMB.
	EvictIdle bool `yaml:"evict_idle"`
}

type DefaultsConfig struct {
	Template string  `yaml:"template"`
	VCPUs    int     `yaml:"vcpus"`
	MemoryMB int     `yaml:"memory_mb"`
	DiskMB   int     `yaml:"disk_mb"`
	TTL      api.TTL `yaml:"ttl"`
	Redact   bool    `yaml:"redact"`
	// MaxVCPUs/MaxMemoryMB/MaxDiskMB bound per-sandbox requests.
	MaxVCPUs    int `yaml:"max_vcpus"`
	MaxMemoryMB int `yaml:"max_memory_mb"`
	MaxDiskMB   int `yaml:"max_disk_mb"`
}

type EgressConfig struct {
	// Enabled starts the per-sandbox egress proxy. When disabled, sandboxes
	// have no outbound network at all.
	Enabled bool `yaml:"enabled"`
	// GlobalAllow is merged into every sandbox's allow list.
	GlobalAllow []string `yaml:"global_allow"`
	// AllowPrivateCIDRs permits destinations in private/loopback ranges.
	// By default those are blocked so sandboxes cannot reach the host or LAN.
	AllowPrivateCIDRs []string `yaml:"allow_private_cidrs"`
	// AllowedPorts for CONNECT tunnels (default 443, 80, 22 is NOT included).
	AllowedPorts []int `yaml:"allowed_ports"`
	// UpstreamProxy forwards all egress through another HTTP proxy (optional).
	UpstreamProxy string `yaml:"upstream_proxy"`
}

type ExecConfig struct {
	DefaultTimeout api.Duration `yaml:"default_timeout"`
	MaxTimeout     api.Duration `yaml:"max_timeout"`
	// MaxOutputBytes caps captured stdout/stderr per foreground exec (each).
	MaxOutputBytes int `yaml:"max_output_bytes"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Listen: ":8443",
		Cluster: ClusterConfig{
			Pools:       []string{"*"},
			DefaultPool: "default",
			Heartbeat:   api.Duration(5 * time.Second),
			HostTimeout: api.Duration(30 * time.Second),
		},
		Data: DataConfig{
			HotDir:       "/data/hot",
			ColdDir:      "/data/cold",
			TemplatesDir: "/data/templates",
		},
		Driver: "firecracker",
		Firecracker: FirecrackerConfig{
			Binary:      "/usr/local/bin/firecracker",
			Kernel:      "/data/kernels/vmlinux",
			BootArgs:    "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd",
			AgentBinary: "/usr/local/lib/fired/fired-agent",
			BootTimeout: api.Duration(30 * time.Second),
			Jailer: JailerConfig{
				Enabled:          true,
				Binary:           "/usr/local/bin/jailer",
				UID:              10000,
				GID:              10000,
				Cgroups:          true,
				ParentCgroup:     "fired",
				MemoryOverheadMB: 128,
			},
		},
		Fake: FakeConfig{AgentBinary: "fired-agent"},
		Capacity: CapacityConfig{
			MaxRunningMemoryMB: 16384,
			MaxSandboxes:       0,
			EvictIdle:          true,
		},
		Defaults: DefaultsConfig{
			VCPUs:    2,
			MemoryMB: 2048,
			DiskMB:   10240,
			TTL: api.TTL{
				IdleSuspend:  api.Duration(10 * time.Minute),
				ArchiveAfter: api.Duration(2 * time.Hour),
				DeleteAfter:  api.Duration(7 * 24 * time.Hour),
			},
			Redact:      true,
			MaxVCPUs:    8,
			MaxMemoryMB: 16384,
			MaxDiskMB:   102400,
		},
		Egress: EgressConfig{
			Enabled:      true,
			AllowedPorts: []int{443, 80},
		},
		Exec: ExecConfig{
			DefaultTimeout: api.Duration(5 * time.Minute),
			MaxTimeout:     api.Duration(2 * time.Hour),
			MaxOutputBytes: 1 << 20,
		},
		ReaperInterval: api.Duration(15 * time.Second),
		ShutdownAction: "suspend",
		LogLevel:       "info",
	}
}

// Load reads path (if non-empty) over the defaults and applies env overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		dec := yaml.NewDecoder(bytesReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, errEOF) {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if v := os.Getenv("FIRED_API_TOKEN"); v != "" {
		cfg.Auth.Token = v
	}
	if v := os.Getenv("FIRED_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("FIRED_DRIVER"); v != "" {
		cfg.Driver = v
	}
	if v := os.Getenv("FIRED_PREVIEW_DOMAIN"); v != "" {
		cfg.PreviewDomain = v
	}
	if v := os.Getenv("FIRED_UI"); v != "" {
		cfg.UI = v == "1" || v == "true"
	}
	if v := os.Getenv("FIRED_DATABASE_URL"); v != "" {
		cfg.DatabaseURL = v
	}
	if v := os.Getenv("FIRED_HOST_ID"); v != "" {
		cfg.Cluster.HostID = v
	}
	if v := os.Getenv("FIRED_ADVERTISE_URL"); v != "" {
		cfg.Cluster.AdvertiseURL = v
	}
	if v := os.Getenv("FIRED_POOLS"); v != "" {
		cfg.Cluster.Pools = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Cluster.Pools = append(cfg.Cluster.Pools, p)
			}
		}
	}
	if v := os.Getenv("FIRED_DATA_DIR"); v != "" {
		cfg.Data = DataConfig{
			HotDir:       filepath.Join(v, "hot"),
			ColdDir:      filepath.Join(v, "cold"),
			TemplatesDir: filepath.Join(v, "templates"),
		}
	}
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	switch c.Driver {
	case "firecracker", "fake":
	default:
		return fmt.Errorf("driver must be \"firecracker\" or \"fake\", got %q", c.Driver)
	}
	switch c.ShutdownAction {
	case "suspend", "archive", "none":
	default:
		return fmt.Errorf("shutdown_action must be suspend, archive or none")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("database_url (or FIRED_DATABASE_URL) is required: fired keeps its state in Postgres")
	}
	if c.Cluster.HostID == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return fmt.Errorf("cluster.host_id is required (the hostname is unknown)")
		}
		c.Cluster.HostID = h
	}
	if c.Cluster.AdvertiseURL == "" {
		port := "8443"
		if i := strings.LastIndex(c.Listen, ":"); i >= 0 && !strings.HasPrefix(c.Listen, "unix:") {
			port = c.Listen[i+1:]
		}
		scheme := "http"
		if c.TLS.Cert != "" {
			scheme = "https"
		}
		c.Cluster.AdvertiseURL = scheme + "://" + c.Cluster.HostID + ":" + port
	}
	c.Cluster.AdvertiseURL = strings.TrimRight(c.Cluster.AdvertiseURL, "/")
	if len(c.Cluster.Pools) == 0 {
		c.Cluster.Pools = []string{"*"}
	}
	if c.Cluster.DefaultPool == "" {
		c.Cluster.DefaultPool = "default"
	}
	if c.Cluster.Heartbeat <= 0 {
		c.Cluster.Heartbeat = api.Duration(5 * time.Second)
	}
	if c.Cluster.HostTimeout <= c.Cluster.Heartbeat {
		c.Cluster.HostTimeout = api.Duration(6 * c.Cluster.Heartbeat.D())
	}
	for name, d := range map[string]string{
		"data.hot_dir":  c.Data.HotDir,
		"data.cold_dir": c.Data.ColdDir, "data.templates_dir": c.Data.TemplatesDir,
	} {
		if d == "" || !filepath.IsAbs(d) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if c.Defaults.VCPUs <= 0 || c.Defaults.MemoryMB <= 0 || c.Defaults.DiskMB <= 0 {
		return fmt.Errorf("defaults.vcpus, memory_mb and disk_mb must be positive")
	}
	if c.Capacity.MaxRunningMemoryMB <= 0 {
		return fmt.Errorf("capacity.max_running_memory_mb must be positive")
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return fmt.Errorf("tls.cert and tls.key must be set together")
	}
	if c.TLS.ClientCA != "" && c.TLS.Cert == "" {
		return fmt.Errorf("tls.client_ca requires tls.cert and tls.key")
	}
	if c.ReaperInterval <= 0 {
		c.ReaperInterval = api.Duration(15 * time.Second)
	}
	return nil
}

// ServesPool reports whether this host runs sandboxes of pool.
func (c ClusterConfig) ServesPool(pool string) bool {
	for _, p := range c.Pools {
		if p == "*" || p == pool {
			return true
		}
	}
	return false
}
