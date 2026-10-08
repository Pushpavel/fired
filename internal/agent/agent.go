// Package agent is the guest agent that runs inside each sandbox VM. It
// executes commands, serves files, tunnels ports and forwards egress
// traffic to the host over vsock.
package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pushpavel/fired/internal/guest"
	"github.com/Pushpavel/fired/pkg/api"
)

var Version = "dev"

type Options struct {
	// RootPrefix maps guest paths onto a host directory (fake driver only).
	RootPrefix string
	// EnvFile holds KEY=VALUE lines baked into the template (/etc/fired/env).
	EnvFile string
	// TemplateFile is /etc/fired/template.json.
	TemplateFile string
	// UpperFD is an open fd on the writable filesystem (for online resize).
	UpperFD   int
	Overlay   bool
	MaxOutput int
	Log       *slog.Logger
}

type templateMeta struct {
	Workdir string `json:"workdir"`
}

type Agent struct {
	opt        Options
	log        *slog.Logger
	started    time.Time
	baseEnv    map[string]string
	workdir    string
	procs      *procTable
	egressAddr string
	mu         sync.Mutex
}

func New(opt Options) *Agent {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.MaxOutput <= 0 {
		opt.MaxOutput = 1 << 20
	}
	a := &Agent{opt: opt, log: opt.Log, started: time.Now(), procs: newProcTable()}
	a.baseEnv = map[string]string{
		"PATH":               "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME":               "/root",
		"LANG":               "C.UTF-8",
		"NODE_USE_ENV_PROXY": "1",
	}
	if opt.EnvFile != "" {
		for k, v := range readEnvFile(a.hostPath(opt.EnvFile)) {
			a.baseEnv[k] = v
		}
	}
	a.workdir = "/workspace"
	if opt.TemplateFile != "" {
		if b, err := os.ReadFile(a.hostPath(opt.TemplateFile)); err == nil {
			var m templateMeta
			if json.Unmarshal(b, &m) == nil && m.Workdir != "" {
				a.workdir = m.Workdir
			}
		}
	}
	_ = os.MkdirAll(a.hostPath(a.workdir), 0o755)
	if opt.RootPrefix != "" {
		// Commands run directly on the host in fake mode; point HOME at the
		// sandbox so tools don't write into the real home directory.
		home := filepath.Join(opt.RootPrefix, "root")
		_ = os.MkdirAll(home, 0o755)
		a.baseEnv["HOME"] = home
	}
	return a
}

// SetEgress records the in-guest proxy address and exports it to commands.
func (a *Agent) SetEgress(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.egressAddr = addr
	u := "http://" + addr
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		a.baseEnv[k] = u
	}
	a.baseEnv["NO_PROXY"] = "localhost,127.0.0.1,::1"
	a.baseEnv["no_proxy"] = "localhost,127.0.0.1,::1"
}

func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// hostPath maps an absolute guest path to the local filesystem.
func (a *Agent) hostPath(p string) string {
	clean := filepath.Clean("/" + p)
	if a.opt.RootPrefix == "" {
		return clean
	}
	return filepath.Join(a.opt.RootPrefix, clean)
}

// guestPath is the inverse of hostPath.
func (a *Agent) guestPath(p string) string {
	if a.opt.RootPrefix == "" {
		return p
	}
	rel, err := filepath.Rel(a.opt.RootPrefix, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return filepath.Clean("/" + rel)
}

// resolve turns a request path (absolute, or relative to the workdir) into
// a guest path and a host path.
func (a *Agent) resolve(p string) (string, string, error) {
	if p == "" {
		return "", "", errors.New("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		p = filepath.Join(a.workdir, p)
	}
	g := filepath.Clean(p)
	return g, a.hostPath(g), nil
}

func (a *Agent) env() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.baseEnv))
	for k, v := range a.baseEnv {
		out[k] = v
	}
	return out
}

// Handler returns the agent's HTTP handler.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /sys/sync", a.handleSync)
	mux.HandleFunc("POST /exec", a.handleExec)
	mux.HandleFunc("GET /exec/stream", a.handleExecStream)
	mux.HandleFunc("GET /processes", a.handleListProcs)
	mux.HandleFunc("GET /processes/{pid}", a.handleGetProc)
	mux.HandleFunc("GET /processes/{pid}/output", a.handleProcOutput)
	mux.HandleFunc("DELETE /processes/{pid}", a.handleKillProc)
	mux.HandleFunc("GET /files", a.handleReadFile)
	mux.HandleFunc("PUT /files", a.handleWriteFile)
	mux.HandleFunc("DELETE /files", a.handleDeleteFile)
	mux.HandleFunc("POST /files/mkdir", a.handleMkdir)
	mux.HandleFunc("POST /files/move", a.handleMove)
	mux.HandleFunc("GET /archive", a.handleGetArchive)
	mux.HandleFunc("PUT /archive", a.handlePutArchive)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			a.handleConnect(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	eg := a.egressAddr
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, guest.Health{Version: Version, StartedAt: a.started, EgressAddr: eg, Overlay: a.opt.Overlay})
}

// handleConnect tunnels to a TCP port on the guest's loopback interface.
func (a *Agent) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		http.Error(w, "only loopback targets are allowed", http.StatusForbidden)
		return
	}
	var c net.Conn
	for _, h := range []string{"127.0.0.1", "::1"} {
		c, err = net.DialTimeout("tcp", net.JoinHostPort(h, port), 3*time.Second)
		if err == nil {
			break
		}
	}
	if err != nil {
		http.Error(w, "nothing is listening on port "+port+" in the sandbox", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		c.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		c.Close()
		return
	}
	defer conn.Close()
	defer c.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
		return
	}
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		if _, err := c.Write(b); err != nil {
			return
		}
	}
	pipe(conn, c)
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind string, err error) {
	writeJSON(w, code, api.Error{Err: api.ErrorBody{Code: kind, Message: err.Error()}})
}

func fsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "not_found", err)
	case errors.Is(err, os.ErrExist):
		writeErr(w, http.StatusConflict, "exists", err)
	case errors.Is(err, os.ErrPermission):
		writeErr(w, http.StatusForbidden, "permission_denied", err)
	default:
		writeErr(w, http.StatusBadRequest, "fs_error", err)
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
