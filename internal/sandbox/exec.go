package sandbox

import (
	"context"
	"net/url"
	"time"

	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/pkg/api"
)

// PrepareExec validates an exec request and merges sandbox-level env.
// Secret values are registered for redaction (in memory only).
func (m *Manager) PrepareExec(l *Lease, req *api.ExecRequest) error {
	if req.Cmd == "" && len(req.Args) == 0 {
		return badRequest("cmd or args is required")
	}
	def := m.cfg.Exec.DefaultTimeout.D()
	max := m.cfg.Exec.MaxTimeout.D()
	if req.TimeoutS <= 0 {
		req.TimeoutS = int(def / time.Second)
	}
	if max > 0 && time.Duration(req.TimeoutS)*time.Second > max {
		req.TimeoutS = int(max / time.Second)
	}
	if len(l.Sandbox.Env) > 0 {
		env := make(map[string]string, len(l.Sandbox.Env)+len(req.Env))
		for k, v := range l.Sandbox.Env {
			env[k] = v
		}
		for k, v := range req.Env {
			env[k] = v
		}
		req.Env = env
	}
	if l.Redact != nil {
		for k, v := range req.SecretEnv {
			l.Redact.Add(k, v)
		}
		for k, v := range req.SecretFiles {
			l.Redact.Add(k, v)
		}
	}
	return nil
}

// Exec runs a command (foreground or background).
func (m *Manager) Exec(ctx context.Context, id string, req api.ExecRequest) (*api.ExecResult, error) {
	l, err := m.Acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	if err := m.PrepareExec(l, &req); err != nil {
		return nil, err
	}
	req.PTY = false
	res, err := l.Agent.Exec(ctx, &req)
	if err != nil {
		return nil, agentErr(err)
	}
	res.Stdout = l.Redact.String(res.Stdout)
	res.Stderr = l.Redact.String(res.Stderr)
	return res, nil
}

func (m *Manager) Processes(ctx context.Context, id string) (*api.ProcessList, error) {
	l, err := m.Acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	r, err := l.Agent.Processes(ctx)
	if err != nil {
		return nil, agentErr(err)
	}
	for i := range r.Processes {
		r.Processes[i].Cmd = l.Redact.String(r.Processes[i].Cmd)
	}
	return r, nil
}

func (m *Manager) Process(ctx context.Context, id string, pid int) (*api.Process, error) {
	l, err := m.Acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	r, err := l.Agent.Process(ctx, pid)
	if err != nil {
		return nil, agentErr(err)
	}
	r.Cmd = l.Redact.String(r.Cmd)
	return r, nil
}

func (m *Manager) ProcessOutput(ctx context.Context, id string, pid int, q url.Values) (*api.ProcessOutput, error) {
	l, err := m.Acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	r, err := l.Agent.ProcessOutput(ctx, pid, q)
	if err != nil {
		return nil, agentErr(err)
	}
	r.Stdout = l.Redact.String(r.Stdout)
	r.Stderr = l.Redact.String(r.Stderr)
	return r, nil
}

func (m *Manager) KillProcess(ctx context.Context, id string, pid int, signal string) (*api.Process, error) {
	l, err := m.Acquire(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	r, err := l.Agent.Kill(ctx, pid, signal)
	if err != nil {
		return nil, agentErr(err)
	}
	r.Cmd = l.Redact.String(r.Cmd)
	return r, nil
}

// RedactSet returns the redaction set for a sandbox (nil if disabled).
func (m *Manager) RedactSet(id string) *redact.Set {
	e := m.get(id)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	on := e.rec.Redact
	e.mu.Unlock()
	if !on {
		return nil
	}
	return m.secrets.For(id)
}
