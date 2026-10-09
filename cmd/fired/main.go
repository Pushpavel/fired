// Command fired is the sandbox daemon.
//
//	fired serve  [-config /etc/fired/config.yaml]
//	fired check  [-config ...]   preflight checks for the host
//	fired version
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/internal/events"
	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/internal/server"
	"github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/internal/vmm"
	"github.com/Pushpavel/fired/internal/vmm/fake"
	"github.com/Pushpavel/fired/internal/vmm/firecracker"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		os.Exit(serve(args))
	case "check":
		os.Exit(check(args))
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fired serve|check|version [-config path]")
}

func loadConfig(args []string) (config.Config, error) {
	fs := flag.NewFlagSet("fired", flag.ExitOnError)
	path := fs.String("config", os.Getenv("FIRED_CONFIG"), "config file (YAML)")
	_ = fs.Parse(args)
	return config.Load(*path)
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func newDriver(cfg config.Config, log *slog.Logger) (vmm.Driver, error) {
	switch cfg.Driver {
	case "fake":
		log.Warn("using the FAKE driver: commands run directly on this host without isolation")
		return fake.New(cfg.Fake.AgentBinary, cfg.Data.HotDir, log)
	default:
		d, err := firecracker.New(cfg.Firecracker, cfg.Data.HotDir, log)
		if err != nil {
			return nil, err
		}
		d.SetInsecureRegistries(cfg.Firecracker.InsecureRegistries)
		return d, nil
	}
}

func serve(args []string) int {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	log := newLogger(cfg.LogLevel)
	if cfg.Auth.Token == "" && cfg.TLS.ClientCA == "" && !strings.HasPrefix(cfg.Listen, "unix:") {
		log.Error("refusing to start without authentication: set auth.token / FIRED_API_TOKEN, tls.client_ca, or listen on a unix socket")
		return 2
	}
	for _, d := range []string{cfg.Data.StateDir, cfg.Data.HotDir, cfg.Data.ColdDir, cfg.Data.TemplatesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Error("create data dir", "dir", d, "err", err)
			return 1
		}
	}
	st, err := store.Open(cfg.Data.StateDir)
	if err != nil {
		log.Error("open state db", "err", err)
		return 1
	}
	defer st.Close()
	drv, err := newDriver(cfg, log)
	if err != nil {
		log.Error("driver", "err", err)
		return 1
	}
	mgr, err := sandbox.New(sandbox.Options{
		Config: cfg, Store: st, Driver: drv, Bus: events.NewBus(), Secrets: redact.NewRegistry(),
		Log: log, Version: version,
	})
	if err != nil {
		log.Error("manager", "err", err)
		return 1
	}
	if err := mgr.Load(context.Background()); err != nil {
		log.Error("load state", "err", err)
		return 1
	}
	mgr.Start()

	api := server.New(mgr, cfg.Auth.Token, log)
	if cfg.UI {
		api.EnableUI()
	}
	srv := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := listen(cfg)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	if cfg.TLS.Cert != "" {
		tc, err := tlsConfig(cfg.TLS)
		if err != nil {
			log.Error("tls", "err", err)
			return 1
		}
		ln = tls.NewListener(ln, tc)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("fired listening", "addr", cfg.Listen, "driver", drv.Name(), "version", version, "tls", cfg.TLS.Cert != "", "ui", cfg.UI)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String(), "action", cfg.ShutdownAction)
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(sctx)
	cancel()
	// Snapshotting many VMs can take a while; Docker's stop_grace_period
	// must cover it.
	mgr.Shutdown(context.Background())
	log.Info("bye")
	return 0
}

func listen(cfg config.Config) (net.Listener, error) {
	if p, ok := strings.CutPrefix(cfg.Listen, "unix:"); ok {
		_ = os.Remove(p)
		ln, err := net.Listen("unix", p)
		if err == nil {
			_ = os.Chmod(p, 0o660)
		}
		return ln, err
	}
	return net.Listen("tcp", cfg.Listen)
}

func tlsConfig(c config.TLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if c.ClientCA != "" {
		pem, err := os.ReadFile(c.ClientCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", c.ClientCA)
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tc, nil
}

// check runs host preflight checks and prints a report.
func check(args []string) int {
	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	failed := 0
	report := func(ok bool, what, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %-28s %s\n", mark, what, detail)
	}
	exists := func(p string) (bool, string) {
		st, err := os.Stat(p)
		if err != nil {
			return false, err.Error()
		}
		return true, fmt.Sprintf("%s (%d bytes)", p, st.Size())
	}
	if cfg.Driver == "fake" {
		_, err := exec.LookPath(cfg.Fake.AgentBinary)
		report(err == nil, "fake agent binary", cfg.Fake.AgentBinary)
	} else {
		ok, d := exists("/dev/kvm")
		report(ok, "/dev/kvm", d)
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
			f.Close()
			report(true, "/dev/kvm read/write", "")
		} else {
			report(false, "/dev/kvm read/write", err.Error())
		}
		ok, d = exists(cfg.Firecracker.Binary)
		report(ok, "firecracker binary", d)
		if cfg.Firecracker.Jailer.Enabled {
			ok, d = exists(cfg.Firecracker.Jailer.Binary)
			report(ok, "jailer binary", d)
		}
		ok, d = exists(cfg.Firecracker.Kernel)
		report(ok, "guest kernel", d)
		ok, d = exists(cfg.Firecracker.AgentBinary)
		report(ok, "guest agent binary", d)
		_, err := exec.LookPath("mkfs.ext4")
		report(err == nil, "mkfs.ext4 (e2fsprogs)", "")
		if out, err := exec.Command("mkfs.ext4", "-V").CombinedOutput(); err == nil {
			report(true, "mkfs.ext4 version", strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
		}
		_, err = os.Stat("/sys/fs/cgroup/cgroup.controllers")
		report(err == nil, "cgroup v2", "")
	}
	for name, d := range map[string]string{"hot_dir": cfg.Data.HotDir, "cold_dir": cfg.Data.ColdDir, "templates_dir": cfg.Data.TemplatesDir, "state_dir": cfg.Data.StateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			report(false, name, err.Error())
			continue
		}
		var fsType string
		if b, err := exec.Command("stat", "-f", "-c", "%T", d).Output(); err == nil {
			fsType = strings.TrimSpace(string(b))
		}
		report(true, name, d+" ("+fsType+")")
	}
	if cfg.Driver == "firecracker" && cfg.Firecracker.Jailer.Enabled {
		// The jailer copies firecracker into each jail and creates /dev/kvm
		// there, so the hot tier must allow both.
		var st unix.Statfs_t
		if err := unix.Statfs(cfg.Data.HotDir, &st); err == nil {
			report(st.Flags&unix.ST_NODEV == 0, "hot_dir allows devices", "mount without nodev (docker --tmpfs needs \"dev\")")
			report(st.Flags&unix.ST_NOEXEC == 0, "hot_dir allows exec", "mount without noexec (docker --tmpfs needs \"exec\")")
		}
	}
	report(cfg.Auth.Token != "" || cfg.TLS.ClientCA != "" || strings.HasPrefix(cfg.Listen, "unix:"), "API authentication", "token, mTLS or unix socket")
	if failed > 0 {
		fmt.Printf("\n%d check(s) failed\n", failed)
		return 1
	}
	fmt.Println("\nall checks passed")
	return 0
}
