package artifacts

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/pkg/api"
	pa "github.com/Pushpavel/fired/pkg/artifacts"
	"github.com/Pushpavel/fired/pkg/client"
)

// Labels put on runner sandboxes in fired.
const (
	labelRunner   = "fired-artifacts"
	labelArtifact = "fired-artifacts.artifact"
	labelTemplate = "fired-artifacts.template"
)

const triggerDir = "/tmp/fired-artifacts"

type startInfo struct {
	run      *runRow
	artifact pa.Artifact
	job      pa.Job
	maxRun   time.Duration
	tree     map[string]pa.File // the version's files
}

type outcome struct {
	status   string
	reason   string
	exitCode *int
	logs     pa.RunLogs
	// retry puts the run back to pending (fired had no capacity, or the
	// quota key is over its limits: overQuota).
	retry     bool
	overQuota bool
	// reconnecting: fired became unavailable while the job ran.
	reconnecting bool
}

// execute runs one job in the artifact's runner sandbox and records the
// result. The runner sandbox is only a cache: each run starts by replacing
// the work directory with the version's files. What the job keeps or
// produces is in the db.
func (s *Service) execute(si *startInfo) {
	defer s.runsWG.Done()
	lr := &liveRun{id: si.run.ID, secrets: redact.NewSet()}
	s.mu.Lock()
	s.live[lr.id] = lr
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, si.artifact.ID)
		delete(s.live, lr.id)
		s.mu.Unlock()
		s.kick()
	}()
	timeout := time.Duration(si.job.Timeout)
	if si.maxRun > 0 && timeout > si.maxRun {
		timeout = si.maxRun
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+10*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	out := s.runJob(ctx, si, lr, timeout)
	if err := s.finishRun(si, lr, out); err != nil {
		s.log.Error("finish run", "run", si.run.ID, "err", err)
	}
	if !out.retry {
		// Its runner is suspended: the key's waiting runs may fit now.
		s.mu.Lock()
		delete(s.quotaWait, si.artifact.QuotaKey)
		s.mu.Unlock()
	}
}

// unavailable reports fired being out of capacity or unreachable (down,
// restarting, a network blip): the run waits instead of failing.
func unavailable(err error) bool {
	var he *client.HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusServiceUnavailable || he.Status == http.StatusBadGateway || he.Status == http.StatusGatewayTimeout
	}
	var ne net.Error
	var oe *net.OpError
	return errors.As(err, &oe) || errors.As(err, &ne) && ne.Timeout()
}

// downBackoff is how long runs wait after fired or the broker was
// unavailable.
const downBackoff = 15 * time.Second

// overQuota reports fired refusing a start for the quota key's limits.
func overQuota(err error) bool {
	var he *client.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusTooManyRequests && he.Code() == "quota_exceeded"
}

// waitable turns fired refusing or failing to start the runner into a
// retry. Only steps before the job starts are retried, so a job never runs
// twice for one trigger.
func waitable(err error) *outcome {
	switch {
	case overQuota(err):
		return &outcome{retry: true, overQuota: true, reason: err.Error()}
	case unavailable(err):
		return &outcome{retry: true, reason: err.Error()}
	}
	return nil
}

// quotaWaiting reports whether key's runs are backing off after fired
// refused one for the key's limits.
func (s *Service) quotaWaiting(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.quotaWait[key]
	if ok && !now.Before(until) {
		delete(s.quotaWait, key)
		return false
	}
	return ok
}

// waitOr is a retry if err is fired being unavailable, else fail.
func waitOr(fail *outcome, err error) *outcome {
	if o := waitable(err); o != nil {
		return o
	}
	return fail
}

func failed(reason string, err error) *outcome {
	return &outcome{status: pa.RunFailed, reason: fmt.Sprintf("%s: %v", reason, err)}
}

func (s *Service) runJob(ctx context.Context, si *startInfo, lr *liveRun, timeout time.Duration) *outcome {
	r, j, a := si.run, si.job, si.artifact
	creds, err := s.broker.credentials(ctx, pa.CredentialsRequest{Artifact: a.ID, QuotaKey: a.QuotaKey, Labels: a.Labels,
		Version: r.Version, RunID: r.ID})
	if err != nil {
		var down *brokerDown
		if errors.As(err, &down) {
			return &outcome{retry: true, reason: err.Error()}
		}
		return failed("credentials", err)
	}
	sb, err := s.ensureRunner(ctx, a, j.Template)
	if err != nil {
		if o := waitable(err); o != nil {
			return o
		}
		return failed("runner sandbox", err)
	}
	// Wake it explicitly: a suspended runner is admitted against the quota
	// key's limits here, and a refusal waits instead of failing the run.
	if _, err := s.fired.Resume(ctx, sb); err != nil {
		if o := waitable(err); o != nil {
			return o
		}
		return failed("runner sandbox", err)
	}
	// Reset the work directory through the file API (not a shell command),
	// so paths are always interpreted as sandbox paths.
	w := s.cfg.Runner.Workdir
	if err := s.fired.DeleteFile(ctx, sb, w, true); err != nil && !client.IsNotFound(err) {
		return waitOr(failed("prepare", err), err)
	}
	if err := s.fired.Mkdir(ctx, sb, w); err != nil {
		return waitOr(failed("prepare", err), err)
	}
	if err := s.fired.UploadArchive(ctx, sb, w, s.treeArchive(si.tree)); err != nil {
		return waitOr(failed("upload files", err), err)
	}
	trig, _ := json.Marshal(map[string]any{"type": r.Trigger, "run_id": r.ID, "at": r.CreatedAt, "payload": json.RawMessage(orNull(r.Payload))})
	triggerFile := path.Join(triggerDir, "trigger.json")
	if _, err := s.fired.WriteFile(ctx, sb, triggerFile, strings.NewReader(string(trig)), 0o644); err != nil {
		return waitOr(failed("write trigger", err), err)
	}
	env := map[string]string{
		"ARTIFACT_ID": a.ID, "ARTIFACT_VERSION": fmt.Sprint(r.Version),
		"ARTIFACT_RUN_ID": r.ID, "TRIGGER": r.Trigger, "TRIGGER_FILE": triggerFile,
	}
	// The run's credentials are kept from its db writes (shown to viewers)
	// and its logs.
	secrets := lr.secrets
	for k, v := range creds.SecretEnv {
		secrets.Add(k, v)
	}
	for k, v := range creds.SecretFiles {
		secrets.Add(k, v)
	}
	secretEnv := creds.SecretEnv
	if s.cfg.Runner.DBURL != "" {
		env["FIRED_DB_URL"] = s.cfg.Runner.DBURL + "/v1/artifacts/" + url.PathEscape(a.ID) + "/db"
		secretEnv = make(map[string]string, len(creds.SecretEnv)+1)
		for k, v := range creds.SecretEnv {
			secretEnv[k] = v
		}
		secretEnv["FIRED_DB_TOKEN"] = s.runToken(r.ID)
		secrets.Add("FIRED_DB_TOKEN", secretEnv["FIRED_DB_TOKEN"])
	}
	started, err := s.fired.Exec(ctx, sb, api.ExecRequest{Cmd: j.Run, Cwd: w, Env: env, SecretEnv: secretEnv,
		SecretFiles: creds.SecretFiles, Background: true})
	if err != nil {
		return failed("start", err)
	}
	out := &outcome{}
	s.follow(ctx, sb, started.PID, timeout, out)
	// Whatever the job left running in its process group ends with the
	// run: the next run starts clean, and nothing outlives its db token.
	_, _ = s.fired.Kill(ctx, sb, started.PID, "KILL")
	// Give the memory back now: a running runner counts against the quota
	// key's limits in fired until it's suspended, holding up the key's other
	// runs. Resuming it next time is quick.
	_, _ = s.fired.Suspend(ctx, sb)
	out.logs.Stdout = string(secrets.Bytes([]byte(out.logs.Stdout)))
	out.logs.Stderr = string(secrets.Bytes([]byte(out.logs.Stderr)))
	return out
}

func orNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

// follow polls the process until it exits or the timeout passes, keeping
// the tail of its output.
func (s *Service) follow(ctx context.Context, sb string, pid int, timeout time.Duration, out *outcome) {
	deadline := time.Now().Add(timeout)
	var so, se int64
	keep := s.cfg.Runner.LogBytes
	appendTail := func(dst *string, add string) {
		*dst += add
		if len(*dst) > keep {
			*dst = (*dst)[len(*dst)-keep:]
			out.logs.Truncated = true
		}
	}
	poll := func(wait time.Duration) (*api.ProcessOutput, error) {
		po, err := s.fired.ProcessOutput(ctx, sb, pid, so, se, int(wait/time.Millisecond))
		if err != nil {
			return nil, err
		}
		appendTail(&out.logs.Stdout, po.Stdout)
		appendTail(&out.logs.Stderr, po.Stderr)
		so, se = po.StdoutOffset, po.StderrOffset
		if po.Dropped {
			out.logs.Truncated = true
		}
		return po, nil
	}
	for {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		wait := 10 * time.Second
		if left < wait {
			wait = left
		}
		po, err := poll(wait)
		if err != nil {
			// fired restarting or a network blip: the job goes on in its
			// sandbox (a restart snapshots it, processes and all), so keep
			// following the same process; only a gone sandbox or process
			// ends the run.
			if unavailable(err) && ctx.Err() == nil {
				if !out.reconnecting {
					s.log.Info("lost the run's process, reconnecting", "sandbox", sb, "pid", pid, "err", err)
				}
				out.reconnecting = true
				select {
				case <-ctx.Done():
				case <-time.After(min(5*time.Second, max(time.Until(deadline), 0))):
				}
				continue
			}
			out.status, out.reason = pa.RunFailed, "follow: "+err.Error()
			return
		}
		if out.reconnecting {
			s.log.Info("reconnected to the run's process", "sandbox", sb, "pid", pid)
			out.reconnecting = false
		}
		if !po.Running {
			out.exitCode = po.ExitCode
			if po.ExitCode != nil && *po.ExitCode == 0 {
				out.status = pa.RunSucceeded
			} else {
				out.status = pa.RunFailed
				if po.ExitCode != nil {
					out.reason = fmt.Sprintf("exit status %d", *po.ExitCode)
				}
			}
			return
		}
	}
	out.status, out.reason = pa.RunTimedOut, fmt.Sprintf("timed out after %s", timeout)
	_, _ = s.fired.Kill(ctx, sb, pid, "TERM")
	for i := 0; i < 5; i++ {
		po, err := poll(2 * time.Second)
		if err != nil || !po.Running {
			if po != nil {
				out.exitCode = po.ExitCode
			}
			return
		}
	}
	_, _ = s.fired.Kill(ctx, sb, pid, "KILL")
}

// treeArchive streams a version's files as tar.gz.
func (s *Service) treeArchive(tree map[string]pa.File) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		err := func() error {
			paths := make([]string, 0, len(tree))
			for p := range tree {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			now := time.Now()
			for _, p := range paths {
				f := tree[p]
				if err := tw.WriteHeader(&tar.Header{Name: p, Mode: int64(f.Mode), Size: f.Size, Typeflag: tar.TypeReg, ModTime: now}); err != nil {
					return err
				}
				rc, err := s.openContent(context.Background(), f.Hash)
				if err != nil {
					return err
				}
				_, err = io.CopyN(tw, rc, f.Size)
				rc.Close()
				if err != nil {
					return err
				}
			}
			if err := tw.Close(); err != nil {
				return err
			}
			return gz.Close()
		}()
		pw.CloseWithError(err)
	}()
	return pr
}

// finishRun records a run's outcome, its logs and events.
func (s *Service) finishRun(si *startInfo, lr *liveRun, out *outcome) error {
	ctx := context.Background()
	r, a := si.run, si.artifact
	if out.retry {
		s.mu.Lock()
		if out.overQuota {
			s.quotaWait[a.QuotaKey] = s.now().Add(time.Duration(s.cfg.Runner.QuotaBackoff))
		} else {
			s.downWait = s.now().Add(downBackoff)
		}
		s.mu.Unlock()
		s.log.Info("run waits", "run", r.ID, "quota_key", a.QuotaKey, "over_quota", out.overQuota, "reason", out.reason)
		r.Status, r.StartedAt, r.Version, r.Reason = pa.RunPending, nil, 0, "waiting: "+out.reason
		return putRun(ctx, s.st.db, r)
	}
	now := s.now()
	if b, err := json.Marshal(out.logs); err == nil {
		if h, err := s.blobs.PutBytes(b); err == nil {
			r.Logs = h
		}
	}
	s.mu.Lock()
	r.DBWrites = lr.writes
	s.mu.Unlock()
	var notify string
	err := s.st.tx(ctx, func(q querier) error {
		r.Status, r.Reason, r.ExitCode, r.FinishedAt = out.status, out.reason, out.exitCode, &now
		if r.StartedAt != nil {
			r.DurationMS = now.Sub(*r.StartedAt).Milliseconds()
		}
		r.Payload = nil
		if err := putRun(ctx, q, r); err != nil {
			return err
		}
		var err error
		if notify, err = takeNotify(ctx, q, a.ID, r.Version); err != nil {
			return err
		}
		return touchArtifact(ctx, q, a.ID, now)
	})
	if err != nil {
		return err
	}
	s.emit(pa.Event{Type: "run.finished", Artifact: a.ID, Version: r.Version, RunID: r.ID, Status: r.Status, Reason: r.Reason, Notify: notify})
	return nil
}

// ensureRunner returns the artifact's runner sandbox, creating it if it's
// missing, broken, on a down host, or made for a different template, quota
// key, pool or allowlist (runner.egress, runner.db_url), or from an older
// build of its template (rebuilt under the same name).
func (s *Service) ensureRunner(ctx context.Context, a pa.Artifact, template string) (string, error) {
	id, pool := a.ID, s.cfg.Runner.Pool
	tpl, err := s.fired.GetTemplate(ctx, template)
	if err != nil {
		return "", err
	}
	list, err := s.fired.ListSandboxes(ctx, map[string]string{labelArtifact: id}, "")
	if err != nil {
		return "", err
	}
	allow := s.runnerEgress()
	for _, sb := range list {
		if sb.State != api.StateError && sb.State != api.StateDeleting && !sb.HostDown &&
			sb.Labels[labelTemplate] == template && sb.TemplateID == tpl.ID && sb.QuotaKey == a.QuotaKey && sb.Pool == pool && slices.Equal(sb.Egress.Allow, allow) {
			return sb.ID, nil
		}
		_ = s.fired.DeleteSandbox(ctx, sb.ID)
	}
	req := api.CreateSandboxRequest{
		Template: template, VCPUs: s.cfg.Runner.VCPUs, MemoryMB: s.cfg.Runner.MemoryMB, DiskMB: s.cfg.Runner.DiskMB,
		Labels:   map[string]string{labelRunner: "runner", labelArtifact: id, labelTemplate: template},
		TTL:      s.cfg.Runner.TTL,
		QuotaKey: a.QuotaKey, Pool: pool,
	}
	if allow := s.runnerEgress(); len(allow) > 0 {
		req.Egress = &api.EgressPolicy{Allow: allow}
	}
	sb, err := s.fired.CreateSandbox(ctx, req)
	if err != nil {
		return "", err
	}
	return sb.ID, nil
}

// runnerEgress is the runner sandboxes' allowlist: the configured one and
// this service's db.
func (s *Service) runnerEgress() []string {
	allow := append([]string(nil), s.cfg.Runner.Egress...)
	if u, err := url.Parse(s.cfg.Runner.DBURL); err == nil && u.Host != "" {
		host := u.Host
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), map[string]string{"http": "80", "https": "443"}[u.Scheme])
		}
		allow = append(allow, host)
	}
	return allow
}

func (s *Service) removeRunner(ctx context.Context, id string) {
	list, err := s.fired.ListSandboxes(ctx, map[string]string{labelArtifact: id}, "")
	if err != nil {
		s.log.Warn("list runner sandboxes", "artifact", id, "err", err)
		return
	}
	for _, sb := range list {
		if err := s.fired.DeleteSandbox(ctx, sb.ID); err != nil && !client.IsNotFound(err) {
			s.log.Warn("delete runner sandbox", "sandbox", sb.ID, "err", err)
		}
	}
}
