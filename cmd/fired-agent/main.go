// Command fired-agent runs inside every sandbox VM. As PID 1 it prepares the
// filesystem (overlay root) and supervises "fired-agent serve", which serves
// the agent API on vsock.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/Pushpavel/fired/internal/agent"
	"github.com/Pushpavel/fired/internal/guest"
)

var version = "dev"

func main() {
	agent.Version = version
	if os.Getpid() == 1 {
		self, err := os.Executable()
		if err != nil {
			self = "/sbin/fired-agent"
		}
		agent.RunInit(self, []string{"serve",
			"--listen", fmt.Sprintf("vsock:%d", guest.AgentPort),
			"--egress-upstream", fmt.Sprintf("vsock:%d:%d", guest.HostCID, guest.EgressPort),
			"--egress-listen", "127.0.0.1:3128",
		})
		return
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		fmt.Println(version)
		return
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", fmt.Sprintf("vsock:%d", guest.AgentPort), "agent API listen spec (vsock:PORT, unix:PATH, tcp:ADDR)")
	egressUp := fs.String("egress-upstream", "", "where to forward egress connections (vsock:CID:PORT, unix:PATH); empty disables")
	egressListen := fs.String("egress-listen", "127.0.0.1:3128", "in-guest proxy listen address")
	rootPrefix := fs.String("root-prefix", "", "map guest paths under this host directory (fake driver only)")
	envFile := fs.String("env-file", "/etc/fired/env", "template environment file")
	tmplFile := fs.String("template-file", "/etc/fired/template.json", "template metadata file")
	maxOutput := fs.Int("max-output", 1<<20, "bytes of output retained per stream")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "fired-agent")
	upperFD := -1
	if v := os.Getenv("FIRED_UPPER_FD"); v != "" {
		upperFD, _ = strconv.Atoi(v)
	}
	a := agent.New(agent.Options{
		RootPrefix: *rootPrefix, EnvFile: *envFile, TemplateFile: *tmplFile,
		UpperFD: upperFD, Overlay: os.Getenv("FIRED_OVERLAY") == "1",
		MaxOutput: *maxOutput, Log: log,
	})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if *egressUp != "" {
		dial, err := agent.Dialer(*egressUp)
		if err != nil {
			log.Error("egress upstream", "err", err)
			os.Exit(2)
		}
		eln, err := agent.Listen("tcp:" + *egressListen)
		if err != nil {
			log.Error("egress listen", "err", err)
		} else {
			a.SetEgress(eln.Addr().String())
			go agent.RunEgressForwarder(ctx, eln, dial, log)
		}
	}

	ln, err := agent.Listen(*listen)
	if err != nil {
		log.Error("listen", "spec", *listen, "err", err)
		os.Exit(1)
	}
	log.Info("agent listening", "addr", *listen, "version", version)
	err = a.Serve(ctx, ln)
	// Don't leave commands running when the agent stops (matters for the
	// fake driver; in a VM everything stops with the VM anyway).
	a.KillAll()
	if err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
