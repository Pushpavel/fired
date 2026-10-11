package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
)

type proc struct {
	pid        int
	cmdline    string
	background bool
	started    time.Time
	cmd        *exec.Cmd

	mu       sync.Mutex
	running  bool
	exitCode *int
	exited   *time.Time
	done     chan struct{}

	stdout *ring
	stderr *ring
}

func (p *proc) info() api.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	return api.Process{PID: p.pid, Cmd: p.cmdline, Background: p.background, Running: p.running,
		ExitCode: p.exitCode, StartedAt: p.started, ExitedAt: p.exited}
}

func (p *proc) finish(code int) {
	p.mu.Lock()
	now := time.Now()
	p.running = false
	p.exitCode = &code
	p.exited = &now
	p.mu.Unlock()
	close(p.done)
}

type procTable struct {
	mu       sync.Mutex
	procs    map[int]*proc
	finished []int // finished background pids, oldest first
}

const keepFinished = 200

func newProcTable() *procTable { return &procTable{procs: map[int]*proc{}} }

func (t *procTable) add(p *proc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.procs[p.pid] = p
}

func (t *procTable) get(pid int) (*proc, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.procs[pid]
	return p, ok
}

// exited records that p finished; foreground processes are removed at once,
// background ones are kept (bounded) so their output can still be read.
func (t *procTable) exited(p *proc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !p.background {
		delete(t.procs, p.pid)
		return
	}
	t.finished = append(t.finished, p.pid)
	for len(t.finished) > keepFinished {
		old := t.finished[0]
		t.finished = t.finished[1:]
		if q, ok := t.procs[old]; ok && q != nil && !q.info().Running {
			delete(t.procs, old)
		}
	}
}

// KillAll kills every running process group started by the agent.
func (a *Agent) KillAll() {
	a.procs.mu.Lock()
	var pids []int
	for pid, p := range a.procs.procs {
		if p.info().Running {
			pids = append(pids, pid)
		}
	}
	a.procs.mu.Unlock()
	for _, pid := range pids {
		killGroup(pid, syscall.SIGKILL)
	}
}

func (t *procTable) list() []api.Process {
	t.mu.Lock()
	ps := make([]*proc, 0, len(t.procs))
	for _, p := range t.procs {
		ps = append(ps, p)
	}
	t.mu.Unlock()
	out := make([]api.Process, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.info())
	}
	return out
}

var execSeq atomic.Int64

// prepared is a command ready to start.
type prepared struct {
	cmd     *exec.Cmd
	cmdline string
}

// prepare builds an exec.Cmd from a request: env, cwd, user, secret files.
func (a *Agent) prepare(req *api.ExecRequest) (*prepared, error) {
	if req.Cmd == "" && len(req.Args) == 0 {
		return nil, errors.New("cmd or args is required")
	}
	env := a.env()

	var cred *syscall.Credential
	if req.User != "" && req.User != "root" && a.opt.RootPrefix == "" {
		u, err := user.Lookup(req.User)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", req.User, err)
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		cred = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		if gids, err := u.GroupIds(); err == nil {
			for _, g := range gids {
				if n, err := strconv.Atoi(g); err == nil {
					cred.Groups = append(cred.Groups, uint32(n))
				}
			}
		}
		env["HOME"] = u.HomeDir
		env["USER"] = u.Username
		env["LOGNAME"] = u.Username
	}

	secretDir := ""
	if len(req.SecretFiles) > 0 || len(req.SecretEnv) > 0 {
		secretDir = filepath.Join("/run/fired/secrets", strconv.FormatInt(time.Now().UnixNano(), 36)+"-"+strconv.FormatInt(execSeq.Add(1), 10))
		hostDir := a.hostPath(secretDir)
		if err := os.MkdirAll(hostDir, 0o700); err != nil {
			return nil, fmt.Errorf("secret dir: %w", err)
		}
		for name, content := range req.SecretFiles {
			if name == "" || strings.ContainsAny(name, "/\x00") || name == "." || name == ".." {
				return nil, fmt.Errorf("invalid secret file name %q", name)
			}
			if err := os.WriteFile(filepath.Join(hostDir, name), []byte(content), 0o600); err != nil {
				return nil, fmt.Errorf("secret file: %w", err)
			}
		}
		if cred != nil {
			_ = filepath.Walk(hostDir, func(p string, _ os.FileInfo, _ error) error {
				return os.Lchown(p, int(cred.Uid), int(cred.Gid))
			})
		}
		if a.opt.RootPrefix != "" {
			// Fake driver: commands run on the host and see host paths.
			secretDir = hostDir
		}
		env["SECRET_DIR"] = secretDir
	}
	expand := func(v string) string {
		if secretDir == "" {
			return v
		}
		v = strings.ReplaceAll(v, "${SECRET_DIR}", secretDir)
		return strings.ReplaceAll(v, "$SECRET_DIR", secretDir)
	}
	for k, v := range req.Env {
		env[k] = expand(v)
	}
	for k, v := range req.SecretEnv {
		env[k] = expand(v)
	}

	var cmd *exec.Cmd
	var cmdline string
	if len(req.Args) > 0 {
		path, err := lookPath(req.Args[0], env["PATH"], a.opt.RootPrefix)
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(path, req.Args[1:]...)
		cmd.Args[0] = req.Args[0]
		cmdline = strings.Join(req.Args, " ")
	} else {
		cmd = exec.Command("/bin/sh", "-c", req.Cmd)
		cmdline = req.Cmd
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = a.workdir
	}
	_, hostCwd, err := a.resolve(cwd)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(hostCwd); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("cwd %q is not a directory", cwd)
	}
	cmd.Dir = hostCwd
	env["PWD"] = hostCwd
	for _, k := range sortedKeys(env) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	return &prepared{cmd: cmd, cmdline: cmdline}, nil
}

func lookPath(name, pathEnv, prefix string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		// In fake mode (prefix != "") commands come from the host's PATH,
		// so no prefix mapping is applied here.
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", name)
}

func killGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, sig)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

func (a *Agent) handleExec(w http.ResponseWriter, r *http.Request) {
	var req api.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	p, err := a.prepare(&req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	if req.Background {
		a.startBackground(w, p, &req)
		return
	}
	a.runForeground(w, r.Context(), p, &req)
}

func (a *Agent) startBackground(w http.ResponseWriter, p *prepared, req *api.ExecRequest) {
	pr := &proc{cmdline: p.cmdline, background: true, started: time.Now(), cmd: p.cmd,
		running: true, done: make(chan struct{}), stdout: newRing(a.opt.MaxOutput), stderr: newRing(a.opt.MaxOutput)}
	p.cmd.Stdout = pr.stdout
	p.cmd.Stderr = pr.stderr
	if req.Stdin != "" {
		p.cmd.Stdin = strings.NewReader(req.Stdin)
	}
	// The process is done when it exits, not when the children that
	// inherited its output (a daemon it started) close it.
	p.cmd.WaitDelay = 2 * time.Second
	if err := p.cmd.Start(); err != nil {
		writeErr(w, http.StatusBadRequest, "exec_failed", err)
		return
	}
	pr.pid = p.cmd.Process.Pid
	a.procs.add(pr)
	go func() {
		err := p.cmd.Wait()
		code := exitCode(err)
		if errors.Is(err, exec.ErrWaitDelay) {
			code = p.cmd.ProcessState.ExitCode()
		}
		pr.finish(code)
		a.procs.exited(pr)
	}()
	writeJSON(w, http.StatusOK, api.ExecResult{PID: pr.pid, ExitCode: 0})
}

func (a *Agent) runForeground(w http.ResponseWriter, ctx context.Context, p *prepared, req *api.ExecRequest) {
	timeout := time.Duration(req.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	stdout, stderr := newHeadTail(a.opt.MaxOutput), newHeadTail(a.opt.MaxOutput)
	p.cmd.Stdout, p.cmd.Stderr = stdout, stderr
	if req.Stdin != "" {
		p.cmd.Stdin = strings.NewReader(req.Stdin)
	}
	// Don't wait forever for grandchildren that inherited the pipes
	// (e.g. "npm run dev &").
	p.cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	if err := p.cmd.Start(); err != nil {
		writeErr(w, http.StatusBadRequest, "exec_failed", err)
		return
	}
	pr := &proc{pid: p.cmd.Process.Pid, cmdline: p.cmdline, started: start, cmd: p.cmd, running: true, done: make(chan struct{})}
	a.procs.add(pr)
	waitErr := make(chan error, 1)
	go func() { waitErr <- p.cmd.Wait() }()
	timedOut := false
	var err error
	select {
	case err = <-waitErr:
	case <-time.After(timeout):
		timedOut = true
		killGroup(pr.pid, syscall.SIGKILL)
		err = <-waitErr
	case <-ctx.Done():
		killGroup(pr.pid, syscall.SIGKILL)
		err = <-waitErr
	}
	code := exitCode(err)
	if errors.Is(err, exec.ErrWaitDelay) {
		code = p.cmd.ProcessState.ExitCode()
	}
	pr.finish(code)
	a.procs.exited(pr)
	if ctx.Err() != nil {
		return
	}
	so, t1 := stdout.String()
	se, t2 := stderr.String()
	writeJSON(w, http.StatusOK, api.ExecResult{
		ExitCode: code, Stdout: so, Stderr: se, Truncated: t1 || t2, TimedOut: timedOut,
		DurationMS: time.Since(start).Milliseconds(),
	})
}

func pidParam(r *http.Request) (int, error) {
	return strconv.Atoi(r.PathValue("pid"))
}

func (a *Agent) handleListProcs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.ProcessList{Processes: a.procs.list()})
}

func (a *Agent) handleGetProc(w http.ResponseWriter, r *http.Request) {
	pid, err := pidParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	p, ok := a.procs.get(pid)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Errorf("process %d not found", pid))
		return
	}
	writeJSON(w, http.StatusOK, p.info())
}

func (a *Agent) handleProcOutput(w http.ResponseWriter, r *http.Request) {
	pid, err := pidParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	p, ok := a.procs.get(pid)
	if !ok || p.stdout == nil {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Errorf("background process %d not found", pid))
		return
	}
	so, _ := strconv.ParseInt(r.URL.Query().Get("since_stdout"), 10, 64)
	se, _ := strconv.ParseInt(r.URL.Query().Get("since_stderr"), 10, 64)
	// Optional long-poll: wait up to wait_ms for new output or exit.
	if ms, _ := strconv.Atoi(r.URL.Query().Get("wait_ms")); ms > 0 {
		deadline := time.Now().Add(time.Duration(min(ms, 60000)) * time.Millisecond)
		for time.Now().Before(deadline) {
			_, o1, _ := p.stdout.Since(so)
			_, o2, _ := p.stderr.Since(se)
			if o1 > so || o2 > se || !p.info().Running {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-p.done:
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	out, oOff, d1 := p.stdout.Since(so)
	errb, eOff, d2 := p.stderr.Since(se)
	info := p.info()
	writeJSON(w, http.StatusOK, api.ProcessOutput{PID: pid, Running: info.Running, ExitCode: info.ExitCode,
		Stdout: string(out), Stderr: string(errb), StdoutOffset: oOff, StderrOffset: eOff, Dropped: d1 || d2})
}

var signals = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "INT": syscall.SIGINT,
	"HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

func parseSignal(s string) (syscall.Signal, error) {
	if s == "" {
		return syscall.SIGTERM, nil
	}
	s = strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if sig, ok := signals[s]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

func (a *Agent) handleKillProc(w http.ResponseWriter, r *http.Request) {
	pid, err := pidParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	sig, err := parseSignal(r.URL.Query().Get("signal"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	p, ok := a.procs.get(pid)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Errorf("process %d not found", pid))
		return
	}
	// The group, even after its leader exited: children it left behind
	// (in its process group) are stopped too.
	killGroup(pid, sig)
	writeJSON(w, http.StatusOK, p.info())
}

var _ io.Writer = (*ring)(nil)
