package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// syncJobs updates the per-job rows after a new version's spec is in
// place. arm re-arms each schedule's default window (publishing is
// activity, like using a sandbox); a fork doesn't arm, so a copied
// schedule stays off until its new owner publishes or grants it.
func (s *Service) syncJobs(ctx context.Context, q querier, id string, spec pa.Spec, now time.Time, arm bool) error {
	for name, raw := range spec.Jobs {
		j := s.cfg.jobDefaults(raw)
		r, err := getJob(ctx, q, id, name)
		if err != nil {
			return err
		}
		if j.Schedule == "" {
			r.SchedKey, r.NextAt = "", time.Time{}
		} else {
			if arm {
				window := time.Duration(j.ActiveFor)
				if d := time.Duration(s.cfg.Limits.DefaultActiveFor); window > d {
					window = d
				}
				if until := now.Add(window); until.After(r.ArmedUntil) {
					r.ArmedUntil = until
				}
			}
			key := j.Schedule + "|" + j.Timezone
			if key != r.SchedKey || r.NextAt.IsZero() {
				c, err := ParseCron(j.Schedule, j.Timezone)
				if err != nil {
					return badRequest("jobs.%s: %v", name, err)
				}
				r.SchedKey, r.NextAt = key, c.Next(now)
			}
		}
		if err := putJob(ctx, q, r); err != nil {
			return err
		}
	}
	return nil
}

// jobStatus works out whether a job can run and why not.
func (s *Service) jobStatus(name string, raw pa.Job, r jobRow, pending int, now time.Time) pa.JobStatus {
	j := s.cfg.jobDefaults(raw)
	maxRun := time.Duration(s.cfg.Limits.DefaultMaxRun)
	if r.GrantMaxRun > maxRun {
		maxRun = r.GrantMaxRun
	}
	st := pa.JobStatus{Name: name, Schedule: j.Schedule, MaxRun: pa.Duration(maxRun), Pending: pending}
	if time.Duration(j.Timeout) > maxRun {
		st.NeedsApproval = append(st.NeedsApproval, fmt.Sprintf("timeout %s exceeds the allowed max run %s", durStr(j.Timeout), maxRun))
	}
	if j.Schedule != "" && j.ActiveFor > s.cfg.Limits.DefaultActiveFor && !r.GrantUntil.After(now) {
		st.NeedsApproval = append(st.NeedsApproval, fmt.Sprintf("active_for %s exceeds the default %s; the schedule runs for the default window until approved",
			durStr(j.ActiveFor), durStr(s.cfg.Limits.DefaultActiveFor)))
	}
	switch {
	case time.Duration(j.Timeout) > maxRun:
		st.State = pa.JobBlocked
	case j.Schedule == "":
		st.State = pa.JobManual
	case now.Before(r.activeUntil()):
		st.State = pa.JobScheduled
	default:
		st.State = pa.JobExpired
	}
	if j.Schedule != "" {
		if u := r.activeUntil(); !u.IsZero() {
			st.ActiveUntil = &u
		}
		if st.State == pa.JobScheduled && !r.NextAt.IsZero() {
			n := r.NextAt
			st.NextRun = &n
		}
	}
	return st
}

// currentSpec returns the artifact and the spec of its current version.
func (s *Service) currentSpec(ctx context.Context, q querier, id string) (*pa.Artifact, pa.Spec, error) {
	a, err := getArtifact(ctx, q, id)
	if err != nil {
		return nil, pa.Spec{}, s.notFound(err, id)
	}
	if a.CurrentVersion == 0 {
		return a, pa.Spec{}, nil
	}
	v, err := getVersion(ctx, q, id, a.CurrentVersion)
	if err != nil {
		return nil, pa.Spec{}, err
	}
	return a, v.Spec, nil
}

// Jobs lists the jobs of the current version with their status.
func (s *Service) Jobs(ctx context.Context, id string) ([]pa.JobStatus, error) {
	_, spec, err := s.currentSpec(ctx, s.st.db, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := []pa.JobStatus{}
	names := make([]string, 0, len(spec.Jobs))
	for n := range spec.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r, err := getJob(ctx, s.st.db, id, n)
		if err != nil {
			return nil, err
		}
		pending, err := queryRuns(ctx, s.st.db, `artifact=? AND job=? AND status='pending'`, id, n)
		if err != nil {
			return nil, err
		}
		out = append(out, s.jobStatus(n, spec.Jobs[n], r, len(pending), now))
	}
	return out, nil
}

// SetGrant records what the owner approved for a job beyond the defaults.
func (s *Service) SetGrant(ctx context.Context, id, job string, g pa.Grant) (*pa.JobStatus, error) {
	if g.MaxRun < 0 {
		return nil, badRequest("max_run must be positive")
	}
	var st pa.JobStatus
	err := s.st.tx(ctx, func(q querier) error {
		_, spec, err := s.currentSpec(ctx, q, id)
		if err != nil {
			return err
		}
		raw, ok := spec.Jobs[job]
		if !ok {
			return errf(http.StatusNotFound, "not_found", "the current version of %s has no job %q", id, job)
		}
		r, err := getJob(ctx, q, id, job)
		if err != nil {
			return err
		}
		r.GrantUntil, r.GrantMaxRun = g.ActiveUntil.UTC(), time.Duration(g.MaxRun)
		j := s.cfg.jobDefaults(raw)
		now := s.now()
		if j.Schedule != "" && r.NextAt.IsZero() {
			c, err := ParseCron(j.Schedule, j.Timezone)
			if err != nil {
				return err
			}
			r.SchedKey, r.NextAt = j.Schedule+"|"+j.Timezone, c.Next(now)
		}
		if err := putJob(ctx, q, r); err != nil {
			return err
		}
		st = s.jobStatus(job, raw, r, 0, now)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kick()
	return &st, nil
}

// Trigger asks for a run of job. It follows the job's on_busy policy: with
// "latest" a pending run is replaced; with "queue" it's appended, or the
// trigger is refused with 429 queue_full.
func (s *Service) Trigger(ctx context.Context, id, job string, req pa.TriggerRequest, kind string) (*pa.Run, error) {
	if len(req.Payload) > s.cfg.Limits.MaxPayloadBytes {
		return nil, errf(http.StatusRequestEntityTooLarge, "too_large", "payload exceeds %d bytes", s.cfg.Limits.MaxPayloadBytes)
	}
	if len(req.Payload) > 0 && !json.Valid(req.Payload) {
		return nil, badRequest("payload must be valid JSON")
	}
	var run *runRow
	var dropped []*runRow
	err := s.st.tx(ctx, func(q querier) error {
		_, spec, err := s.currentSpec(ctx, q, id)
		if err != nil {
			return err
		}
		raw, ok := spec.Jobs[job]
		if !ok {
			return errf(http.StatusNotFound, "not_found", "the current version of %s has no job %q", id, job)
		}
		r, err := getJob(ctx, q, id, job)
		if err != nil {
			return err
		}
		now := s.now()
		if st := s.jobStatus(job, raw, r, 0, now); st.State == pa.JobBlocked {
			return errf(http.StatusConflict, "needs_approval", "job %q can't run until approved: %v", job, st.NeedsApproval)
		}
		run, dropped, err = s.enqueue(ctx, q, id, job, raw, kind, req.Payload, now)
		return err
	})
	for _, d := range dropped {
		s.emit(pa.Event{Type: "run.dropped", Artifact: id, Job: job, RunID: d.ID, Status: d.Status, Reason: d.Reason})
	}
	if err != nil {
		return nil, err
	}
	s.kick()
	return &run.Run, nil
}

var errQueueFull = errf(http.StatusTooManyRequests, "queue_full", "the job's run queue is full")

// enqueue adds a pending run, applying the on_busy policy. It returns the
// new run and any runs dropped on the way (expired from the queue).
func (s *Service) enqueue(ctx context.Context, q querier, id, job string, raw pa.Job, kind string, payload []byte, now time.Time) (*runRow, []*runRow, error) {
	j := s.cfg.jobDefaults(raw)
	pending, err := queryRuns(ctx, q, `artifact=? AND job=? AND status='pending' ORDER BY created_at`, id, job)
	if err != nil {
		return nil, nil, err
	}
	var dropped []*runRow
	switch j.OnBusy {
	case pa.OnBusyQueue:
		live := 0
		for _, p := range pending {
			if j.Queue.MaxAge > 0 && now.Sub(p.CreatedAt) > time.Duration(j.Queue.MaxAge) {
				if err := s.finishPending(ctx, q, p, pa.RunDropped, "expired", now); err != nil {
					return nil, nil, err
				}
				dropped = append(dropped, p)
				continue
			}
			live++
		}
		if j.Queue.Max > 0 && live >= j.Queue.Max {
			return nil, dropped, errQueueFull
		}
	default: // latest
		for _, p := range pending {
			if err := s.finishPending(ctx, q, p, pa.RunSuperseded, "replaced by a newer trigger", now); err != nil {
				return nil, nil, err
			}
		}
	}
	r := &runRow{Run: pa.Run{ID: newID("run-"), Artifact: id, Job: job, Status: pa.RunPending, Trigger: kind, CreatedAt: now}, Payload: payload}
	if err := putRun(ctx, q, r); err != nil {
		return nil, nil, err
	}
	return r, dropped, nil
}

func (s *Service) finishPending(ctx context.Context, q querier, r *runRow, status, reason string, now time.Time) error {
	r.Status, r.Reason = status, reason
	r.FinishedAt = &now
	r.Payload = nil
	return putRun(ctx, q, r)
}

// ---- runs

func (s *Service) Runs(ctx context.Context, id, job, status string, limit int) ([]pa.Run, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	where := `artifact=?`
	args := []any{id}
	if job != "" {
		where += ` AND job=?`
		args = append(args, job)
	}
	if status != "" {
		where += ` AND status=?`
		args = append(args, status)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := queryRuns(ctx, s.st.db, where+fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit), args...)
	if err != nil {
		return nil, err
	}
	out := make([]pa.Run, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Run)
	}
	return out, nil
}

func (s *Service) Run(ctx context.Context, id, runID string) (*pa.Run, error) {
	r, err := getRun(ctx, s.st.db, runID)
	if err != nil || r.Artifact != id {
		return nil, errf(http.StatusNotFound, "not_found", "run %s not found", runID)
	}
	return &r.Run, nil
}

func (s *Service) RunLogs(ctx context.Context, id, runID string) (*pa.RunLogs, error) {
	r, err := getRun(ctx, s.st.db, runID)
	if err != nil || r.Artifact != id {
		return nil, errf(http.StatusNotFound, "not_found", "run %s not found", runID)
	}
	out := &pa.RunLogs{}
	if r.Logs == "" {
		return out, nil
	}
	data, err := s.blobs.ReadAll(r.Logs, 64<<20)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(data, out)
	return out, nil
}
