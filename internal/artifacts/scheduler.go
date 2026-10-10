package artifacts

import (
	"context"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// Start reconciles runs interrupted by a restart and starts the scheduler
// and garbage collector. Close stops them.
func (s *Service) Start(ctx context.Context) error {
	interrupted, err := queryRuns(ctx, s.st.db, `status='running'`)
	if err != nil {
		return err
	}
	now := s.now()
	for _, r := range interrupted {
		r.Status, r.Reason, r.FinishedAt = pa.RunFailed, "interrupted: the service restarted during the run", &now
		if err := putRun(ctx, s.st.db, r); err != nil {
			return err
		}
	}
	s.bgWG.Add(2)
	go s.loop(time.Duration(s.cfg.SchedulerInterval), true, s.tick)
	go s.loop(time.Duration(s.cfg.GCInterval), false, s.GC)
	return nil
}

func (s *Service) loop(every time.Duration, wakeable bool, fn func(context.Context)) {
	defer s.bgWG.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	var wake chan struct{}
	if wakeable {
		wake = s.wake
	}
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		case <-wake:
		}
		ctx, cancel := context.WithTimeout(context.Background(), every+time.Minute)
		fn(ctx)
		cancel()
	}
}

// kick runs the scheduler soon instead of at the next tick.
func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) tick(ctx context.Context) {
	if err := s.fireSchedules(ctx); err != nil {
		s.log.Error("schedules", "err", err)
	}
	if err := s.dispatch(ctx); err != nil {
		s.log.Error("dispatch", "err", err)
	}
}

func dueJobs(ctx context.Context, q querier, now time.Time) ([][2]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT artifact, job FROM jobs WHERE sched_key<>'' AND next_at>0 AND next_at<=?`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, j string
		if err := rows.Scan(&a, &j); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, j})
	}
	return out, rows.Err()
}

// fireSchedules turns due schedules into pending runs. A schedule missed
// while the service was down fires once, not once per missed slot.
func (s *Service) fireSchedules(ctx context.Context) error {
	now := s.now()
	due, err := dueJobs(ctx, s.st.db, now)
	if err != nil {
		return err
	}
	for _, d := range due {
		id, job := d[0], d[1]
		var events []pa.Event
		err := s.st.tx(ctx, func(q querier) error {
			r, err := getJob(ctx, q, id, job)
			if err != nil {
				return err
			}
			_, spec, err := s.currentSpec(ctx, q, id)
			if err != nil {
				return err
			}
			raw, ok := spec.Jobs[job]
			j := s.cfg.jobDefaults(raw)
			if !ok || j.Schedule == "" {
				r.SchedKey, r.NextAt = "", time.Time{}
				return putJob(ctx, q, r)
			}
			c, err := ParseCron(j.Schedule, j.Timezone)
			if err != nil {
				r.SchedKey, r.NextAt = "", time.Time{}
				return putJob(ctx, q, r)
			}
			r.NextAt = c.Next(now)
			if st := s.jobStatus(job, raw, r, 0, now); st.State != pa.JobScheduled {
				return putJob(ctx, q, r)
			}
			a, err := getArtifact(ctx, q, id)
			if err != nil {
				return err
			}
			quota, err := s.effectiveQuota(ctx, q, a.QuotaKey)
			if err != nil {
				return err
			}
			interval := time.Duration(s.cfg.Limits.MinInterval)
			if m := time.Duration(quota.MinInterval); m > interval {
				interval = m
			}
			if !r.LastFireAt.IsZero() && now.Sub(r.LastFireAt) < interval-time.Second {
				s.log.Debug("schedule skipped: min interval", "artifact", id, "job", job, "interval", interval)
				return putJob(ctx, q, r)
			}
			r.LastFireAt = now
			if err := putJob(ctx, q, r); err != nil {
				return err
			}
			run, dropped, err := s.enqueue(ctx, q, id, job, raw, pa.TriggerSchedule, nil, now)
			for _, x := range dropped {
				events = append(events, pa.Event{Type: "run.dropped", Artifact: id, Job: job, RunID: x.ID, Status: x.Status, Reason: x.Reason})
			}
			if err == errQueueFull {
				x := &runRow{Run: pa.Run{ID: newID("run-"), Artifact: id, Job: job, Status: pa.RunDropped, Trigger: pa.TriggerSchedule,
					Reason: "queue_full", CreatedAt: now, FinishedAt: &now}}
				events = append(events, pa.Event{Type: "run.dropped", Artifact: id, Job: job, RunID: x.ID, Status: x.Status, Reason: x.Reason})
				return putRun(ctx, q, x)
			}
			_ = run
			return err
		})
		if err != nil {
			s.log.Error("fire schedule", "artifact", id, "job", job, "err", err)
			continue
		}
		for _, e := range events {
			s.emit(e)
		}
	}
	return nil
}

// dispatch starts pending runs while limits allow: one run at a time per
// artifact, the quota key's concurrent runs and daily run minutes, and the
// service-wide cap. Runs that can't start yet stay pending.
func (s *Service) dispatch(ctx context.Context) error {
	pending, err := queryRuns(ctx, s.st.db, `status='pending' ORDER BY created_at`)
	if err != nil {
		return err
	}
	for _, p := range pending {
		s.mu.Lock()
		busy := s.running[p.Artifact] != ""
		full := len(s.running) >= s.cfg.Limits.MaxConcurrentRuns
		s.mu.Unlock()
		if full {
			return nil
		}
		if busy {
			continue
		}
		var start *startInfo
		var events []pa.Event
		err := s.st.tx(ctx, func(q querier) error {
			cur, err := getRun(ctx, q, p.ID)
			if err != nil || cur.Status != pa.RunPending {
				return nil
			}
			a, spec, err := s.currentSpec(ctx, q, p.Artifact)
			if err != nil {
				return err
			}
			now := s.now()
			drop := func(reason string) error {
				events = append(events, pa.Event{Type: "run.dropped", Artifact: p.Artifact, Job: p.Job, RunID: p.ID, Status: pa.RunDropped, Reason: reason})
				return s.finishPending(ctx, q, cur, pa.RunDropped, reason, now)
			}
			raw, ok := spec.Jobs[p.Job]
			if !ok {
				return drop("job_removed")
			}
			j := s.cfg.jobDefaults(raw)
			r, err := getJob(ctx, q, p.Artifact, p.Job)
			if err != nil {
				return err
			}
			st := s.jobStatus(p.Job, raw, r, 0, now)
			if st.State == pa.JobBlocked {
				return drop("needs_approval")
			}
			if j.OnBusy == pa.OnBusyQueue && j.Queue.MaxAge > 0 && now.Sub(cur.CreatedAt) > time.Duration(j.Queue.MaxAge) {
				return drop("expired")
			}
			quota, err := s.effectiveQuota(ctx, q, a.QuotaKey)
			if err != nil {
				return err
			}
			if quota.MaxConcurrentRuns > 0 {
				n, err := countRunning(ctx, q, a.QuotaKey)
				if err != nil {
					return err
				}
				if n >= quota.MaxConcurrentRuns {
					return nil
				}
			}
			if quota.RunMinutesPerDay > 0 {
				used, err := runMillisSince(ctx, q, a.QuotaKey, now.Add(-24*time.Hour), now)
				if err != nil {
					return err
				}
				if used >= int64(quota.RunMinutesPerDay)*60000 {
					return nil
				}
			}
			cur.Status, cur.StartedAt, cur.Version = pa.RunRunning, &now, a.CurrentVersion
			if err := putRun(ctx, q, cur); err != nil {
				return err
			}
			tree, err := getTree(ctx, q, a.ID, a.CurrentVersion)
			if err != nil {
				return err
			}
			stateHash, err := getRunState(ctx, q, a.ID, p.Job)
			if err != nil {
				return err
			}
			start = &startInfo{run: cur, artifact: *a, job: j, maxRun: time.Duration(st.MaxRun), tree: tree, stateHash: stateHash}
			return nil
		})
		for _, e := range events {
			s.emit(e)
		}
		if err != nil {
			s.log.Error("dispatch run", "run", p.ID, "err", err)
			continue
		}
		if start == nil {
			continue
		}
		s.mu.Lock()
		s.running[p.Artifact] = p.ID
		s.mu.Unlock()
		s.runsWG.Add(1)
		s.emit(pa.Event{Type: "run.started", Artifact: p.Artifact, Version: start.run.Version, Job: p.Job, RunID: p.ID, Status: pa.RunRunning})
		go s.execute(start)
	}
	return nil
}
