// Command fired-artifacts is the artifact service: versioned file trees
// with views and scheduled jobs that run in fired sandboxes.
//
//	fired-artifacts serve [-config /etc/fired/artifacts.yaml]
//	fired-artifacts version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // job schedules name IANA timezones

	"github.com/Pushpavel/fired/internal/artifacts"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fired-artifacts serve|version [-config path]")
}

func serve(args []string) int {
	fs := flag.NewFlagSet("fired-artifacts", flag.ExitOnError)
	path := fs.String("config", os.Getenv("FIRED_ARTIFACTS_CONFIG"), "config file (YAML)")
	_ = fs.Parse(args)
	cfg, err := artifacts.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	if cfg.Token == "" {
		log.Error("refusing to start without authentication: set token / FIRED_ARTIFACTS_TOKEN")
		return 2
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Error("create data dir", "err", err)
		return 1
	}
	svc, err := artifacts.New(cfg, log)
	if err != nil {
		log.Error("open", "err", err)
		return 1
	}
	if err := svc.Start(context.Background()); err != nil {
		log.Error("start", "err", err)
		return 1
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: svc.Handler(), ReadHeaderTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("fired-artifacts listening", "addr", cfg.Listen, "version", version, "fired", cfg.Fired.URL)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if err := svc.Close(); err != nil {
		log.Error("close", "err", err)
		return 1
	}
	return 0
}
