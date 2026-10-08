package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
)

// Listen parses "vsock:PORT", "unix:/path" or "tcp:ADDR".
func Listen(spec string) (net.Listener, error) {
	kind, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("bad listen spec %q", spec)
	}
	switch kind {
	case "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, err
		}
		return vsock.Listen(uint32(port), nil)
	case "unix":
		_ = os.Remove(rest)
		return net.Listen("unix", rest)
	case "tcp":
		return net.Listen("tcp", rest)
	}
	return nil, fmt.Errorf("bad listen spec %q", spec)
}

// Dialer parses "vsock:CID:PORT", "unix:/path" or "tcp:ADDR".
func Dialer(spec string) (func() (net.Conn, error), error) {
	kind, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("bad dial spec %q", spec)
	}
	switch kind {
	case "vsock":
		cs, ps, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("vsock dial spec must be vsock:CID:PORT")
		}
		cid, err := strconv.ParseUint(cs, 10, 32)
		if err != nil {
			return nil, err
		}
		port, err := strconv.ParseUint(ps, 10, 32)
		if err != nil {
			return nil, err
		}
		return func() (net.Conn, error) { return vsock.Dial(uint32(cid), uint32(port), nil) }, nil
	case "unix", "tcp":
		return func() (net.Conn, error) { return net.DialTimeout(kind, rest, 10*time.Second) }, nil
	}
	return nil, fmt.Errorf("bad dial spec %q", spec)
}

// RunEgressForwarder accepts proxy connections inside the guest and relays
// them to the host's egress proxy.
func RunEgressForwarder(ctx context.Context, ln net.Listener, dial func() (net.Conn, error), log *slog.Logger) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("egress accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			defer c.Close()
			up, err := dial()
			if err != nil {
				log.Warn("egress dial host", "err", err)
				_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
				return
			}
			defer up.Close()
			pipe(c, up)
		}()
	}
}

// Serve runs the agent HTTP server until ctx is cancelled.
func (a *Agent) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
