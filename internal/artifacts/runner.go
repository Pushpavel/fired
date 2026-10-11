package artifacts

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
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
	run       *runRow
	artifact  pa.Artifact
	job       pa.Job
	maxRun    time.Duration
	tree      map[string]pa.File
	stateHash string
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
	captured  map[string]pa.File
	state     string // new state blob ("" clears it)
}

// execute runs one job in the artifact's runner sandbox and records the
// result. The runner sandbox is only a cache: each run starts by replacing
// the work directory with the version's files and the job's saved state.
func (s *Service) execute(si *startInfo) {
	defer s.runsWG.Done()
	defer func() {
		s.mu.Lock()
		delete(s.running, si.artifact.ID)
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
	out := s.runJob(ctx, si, timeout)
	if err := s.finishRun(si, out); err != nil {
		s.log.Error("finish run", "run", si.run.ID, "err", err)
	}
}

func unavailable(err error) bool {
	var he *client.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusServiceUnavailable
}

// overQuota reports fired refusing a start for the quota key's limits.
func overQuota(err error) bool {
	var he *client.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusTooManyRequests && he.Code() == "quota_exceeded"
}

// waitable turns fired refusing to start the runner into a retry.
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

func failed(reason string, err error) *outcome {
	return &outcome{status: pa.RunFailed, reason: fmt.Sprintf("%s: %v", reason, err)}
}

func (s *Service) runJob(ctx context.Context, si *startInfo, timeout time.Duration) *outcome {
	r, j, a := si.run, si.job, si.artifact
	creds, err := s.broker.credentials(ctx, pa.CredentialsRequest{Artifact: a.ID, QuotaKey: a.QuotaKey, Labels: a.Labels,
		Version: r.Version, Job: r.Job, RunID: r.ID})
	if err != nil {
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
		if unavailable(err) {
			return &outcome{retry: true, reason: err.Error()}
		}
		return failed("prepare", err)
	}
	if err := s.fired.Mkdir(ctx, sb, w); err != nil {
		return failed("prepare", err)
	}
	if err := s.fired.UploadArchive(ctx, sb, w, s.treeArchive(si.tree)); err != nil {
		return failed("upload files", err)
	}
	if si.stateHash != "" && j.State != "" {
		rc, err := s.openContent(ctx, si.stateHash)
		if err != nil {
			return failed("open state", err)
		}
		err = s.fired.UploadArchive(ctx, sb, path.Join(w, j.State), rc)
		rc.Close()
		if err != nil {
			return failed("upload state", err)
		}
	}
	trig, _ := json.Marshal(map[string]any{"type": r.Trigger, "run_id": r.ID, "job": r.Job, "at": r.CreatedAt, "payload": json.RawMessage(orNull(r.Payload))})
	triggerFile := path.Join(triggerDir, "trigger.json")
	if _, err := s.fired.WriteFile(ctx, sb, triggerFile, strings.NewReader(string(trig)), 0o644); err != nil {
		return failed("write trigger", err)
	}
	env := map[string]string{
		"ARTIFACT_ID": a.ID, "ARTIFACT_VERSION": fmt.Sprint(r.Version), "ARTIFACT_JOB": r.Job,
		"ARTIFACT_RUN_ID": r.ID, "TRIGGER": r.Trigger, "TRIGGER_FILE": triggerFile,
	}
	started, err := s.fired.Exec(ctx, sb, api.ExecRequest{Cmd: j.Run, Cwd: w, Env: env, SecretEnv: creds.SecretEnv,
		SecretFiles: creds.SecretFiles, Background: true})
	if err != nil {
		return failed("start", err)
	}
	secrets := redact.NewSet()
	for k, v := range creds.SecretEnv {
		secrets.Add(k, v)
	}
	for k, v := range creds.SecretFiles {
		secrets.Add(k, v)
	}
	out := &outcome{}
	s.follow(ctx, sb, started.PID, timeout, out)
	out.logs.Stdout = string(secrets.Bytes([]byte(out.logs.Stdout)))
	out.logs.Stderr = string(secrets.Bytes([]byte(out.logs.Stderr)))
	if out.status != pa.RunSucceeded {
		return out
	}
	captured, err := s.capture(ctx, sb, w, j.Writes)
	if err != nil {
		out.status, out.reason = pa.RunFailed, "capture writes: "+err.Error()
		return out
	}
	// Artifacts get shared; a run must not publish the credentials it ran
	// with. Refuse the result if any written file contains one.
	if p, name, err := s.findSecret(captured, secrets); err != nil {
		out.status, out.reason = pa.RunFailed, "scan writes: "+err.Error()
		return out
	} else if p != "" {
		out.status, out.reason = pa.RunFailed, fmt.Sprintf("result rejected: %s contains the value of credential %s", p, name)
		return out
	}
	out.captured = captured
	if j.State != "" {
		h, err := s.captureState(ctx, sb, path.Join(w, j.State))
		if err != nil {
			out.status, out.reason = pa.RunFailed, "capture state: "+err.Error()
			return out
		}
		out.state = h
	}
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
			out.status, out.reason = pa.RunFailed, "follow: "+err.Error()
			return
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

// capture reads the paths a job may write back from the sandbox.
func (s *Service) capture(ctx context.Context, sb, w string, writes []string) (map[string]pa.File, error) {
	out := map[string]pa.File{}
	var total int64
	for _, wp := range writes {
		full := path.Join(w, wp)
		fi, err := s.fired.Stat(ctx, sb, full)
		if client.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rc, err := s.fired.DownloadArchive(ctx, sb, full)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer rc.Close()
			gz, err := gzip.NewReader(rc)
			if err != nil {
				return err
			}
			tr := tar.NewReader(gz)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if hdr.Typeflag != tar.TypeReg {
					continue
				}
				name := strings.TrimPrefix(hdr.Name, "./")
				p := wp
				if fi.IsDir {
					p = wp + "/" + name
				}
				p, err = cleanPath(p)
				if err != nil {
					return err
				}
				h, n, err := s.putContent(context.Background(), tr, s.cfg.Limits.MaxVersionBytes, "")
				if err != nil {
					return err
				}
				total += n
				if total > s.cfg.Limits.MaxVersionBytes {
					return fmt.Errorf("written files exceed %d bytes", s.cfg.Limits.MaxVersionBytes)
				}
				mode := uint32(0o644)
				if hdr.Mode&0o111 != 0 {
					mode = 0o755
				}
				out[p] = pa.File{Path: p, Hash: h, Size: n, Mode: mode}
			}
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// findSecret scans files for credential values, reading each in chunks
// that overlap by the longest value so matches across chunks are found.
func (s *Service) findSecret(files map[string]pa.File, secrets *redact.Set) (string, string, error) {
	if secrets.Empty() {
		return "", "", nil
	}
	overlap := secrets.MaxLen()
	buf := make([]byte, overlap+1<<20)
	for p, f := range files {
		rc, err := s.openContent(context.Background(), f.Hash)
		if err != nil {
			return "", "", err
		}
		keep := 0
		for {
			n, err := io.ReadFull(rc, buf[keep:])
			if name, ok := secrets.Find(buf[:keep+n]); ok {
				rc.Close()
				return p, name, nil
			}
			if err != nil {
				break
			}
			total := keep + n
			keep = overlap
			if keep > total {
				keep = total
			}
			copy(buf, buf[total-keep:total])
		}
		rc.Close()
	}
	return "", "", nil
}

// captureState stores the job's state directory as one tar.gz blob.
func (s *Service) captureState(ctx context.Context, sb, dir string) (string, error) {
	if _, err := s.fired.Stat(ctx, sb, dir); client.IsNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	rc, err := s.fired.DownloadArchive(ctx, sb, dir)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h, _, err := s.putContent(ctx, rc, s.cfg.Limits.MaxVersionBytes, "")
	return h, err
}

// finishRun records a run's outcome: a new version if its writes changed
// anything, its state, logs and events.
func (s *Service) finishRun(si *startInfo, out *outcome) error {
	ctx := context.Background()
	r, a, j := si.run, si.artifact, si.job
	if out.retry {
		if out.overQuota {
			s.mu.Lock()
			s.quotaWait[a.QuotaKey] = s.now().Add(time.Duration(s.cfg.Runner.QuotaBackoff))
			s.mu.Unlock()
		}
		s.log.Info("run waits for capacity", "run", r.ID, "quota_key", a.QuotaKey, "over_quota", out.overQuota, "reason", out.reason)
		r.Status, r.StartedAt, r.Version = pa.RunPending, nil, 0
		return putRun(ctx, s.st.db, r)
	}
	now := s.now()
	if b, err := json.Marshal(out.logs); err == nil {
		if h, err := s.blobs.PutBytes(b); err == nil {
			r.Logs = h
		}
	}
	var newVersion *pa.Version
	var notify string
	err := s.st.tx(ctx, func(q querier) error {
		if out.status == pa.RunSucceeded {
			v, err := s.commitRun(ctx, q, r, j, out.captured, now)
			if err != nil {
				out.status, out.reason = pa.RunFailed, "result rejected: "+err.Error()
			} else {
				newVersion = v
				if j.State != "" {
					if err := putRunState(ctx, q, a.ID, r.Job, out.state, now); err != nil {
						return err
					}
				}
			}
		}
		if newVersion != nil {
			r.ResultVersion = newVersion.Version
		}
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
	if newVersion != nil {
		s.emit(pa.Event{Type: "artifact.version", Artifact: a.ID, Version: newVersion.Version, Origin: pa.OriginRun, Job: r.Job, RunID: r.ID})
	}
	s.emit(pa.Event{Type: "run.finished", Artifact: a.ID, Version: r.Version, Job: r.Job, RunID: r.ID, Status: r.Status, Reason: r.Reason, Notify: notify})
	return nil
}

// commitRun applies a run's writes on top of the current version (not the
// version the run started from, so edits published meanwhile are kept) and
// prunes old run versions. It returns nil when nothing changed.
func (s *Service) commitRun(ctx context.Context, q querier, r *runRow, j pa.Job, captured map[string]pa.File, now time.Time) (*pa.Version, error) {
	if len(j.Writes) == 0 {
		return nil, nil
	}
	a, err := getArtifact(ctx, q, r.Artifact)
	if err != nil {
		return nil, err
	}
	cur, err := getVersion(ctx, q, a.ID, a.CurrentVersion)
	if err != nil {
		return nil, err
	}
	tree, err := getTree(ctx, q, a.ID, a.CurrentVersion)
	if err != nil {
		return nil, err
	}
	next := make(map[string]pa.File, len(tree))
	for p, f := range tree {
		keep := true
		for _, w := range j.Writes {
			if under(p, w) {
				keep = false
				break
			}
		}
		if keep {
			next[p] = f
		}
	}
	for p, f := range captured {
		next[p] = f
	}
	if treesEqual(tree, next) {
		return nil, nil
	}
	if err := checkNoFileDirClash(next); err != nil {
		return nil, err
	}
	spec := cur.Spec
	if err := ValidateSpec(&spec, next); err != nil {
		return nil, err
	}
	size, err := s.checkTree(ctx, q, a.QuotaKey, next)
	if err != nil {
		return nil, err
	}
	n, err := maxVersion(ctx, q, a.ID)
	if err != nil {
		return nil, err
	}
	v := &pa.Version{Artifact: a.ID, Version: n + 1, Parent: a.CurrentVersion, Origin: pa.OriginRun, Label: "run " + r.Job,
		RunID: r.ID, Job: r.Job, FileCount: len(next), SizeBytes: size, Spec: spec, CreatedAt: now}
	if err := insertVersion(ctx, q, v, next); err != nil {
		return nil, err
	}
	setCurrent(a, v)
	a.UpdatedAt = now
	if err := putArtifact(ctx, q, a); err != nil {
		return nil, err
	}
	return v, s.pruneRunVersions(ctx, q, a)
}

// pruneRunVersions keeps the newest RetainRuns run-produced versions besides
// the current one. Edit, restore and fork versions are always kept.
func (s *Service) pruneRunVersions(ctx context.Context, q querier, a *pa.Artifact) error {
	keep := a.RetainRuns
	if keep <= 0 {
		keep = s.cfg.Limits.RetainRuns
	}
	rows, err := q.QueryContext(ctx, `SELECT version FROM versions WHERE artifact=? AND origin=? AND version<>? ORDER BY version DESC`,
		a.ID, pa.OriginRun, a.CurrentVersion)
	if err != nil {
		return err
	}
	var old []int
	i := 0
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		if i++; i > keep {
			old = append(old, n)
		}
	}
	rows.Close()
	for _, n := range old {
		if err := deleteVersion(ctx, q, a.ID, n); err != nil {
			return err
		}
	}
	return nil
}

// ensureRunner returns the artifact's runner sandbox, creating it if it's
// missing, broken, on a down host, or made for a different template, quota
// key or pool.
func (s *Service) ensureRunner(ctx context.Context, a pa.Artifact, template string) (string, error) {
	id, pool := a.ID, s.cfg.Runner.Pool
	list, err := s.fired.ListSandboxes(ctx, map[string]string{labelArtifact: id}, "")
	if err != nil {
		return "", err
	}
	for _, sb := range list {
		if sb.State != api.StateError && sb.State != api.StateDeleting && !sb.HostDown &&
			sb.Labels[labelTemplate] == template && sb.QuotaKey == a.QuotaKey && sb.Pool == pool {
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
	if len(s.cfg.Runner.Egress) > 0 {
		req.Egress = &api.EgressPolicy{Allow: s.cfg.Runner.Egress}
	}
	sb, err := s.fired.CreateSandbox(ctx, req)
	if err != nil {
		return "", err
	}
	return sb.ID, nil
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
