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
	if err := s.expireQueued(ctx); err != nil {
		s.log.Error("expire queued runs", "err", err)
	}
	if err := s.dispatch(ctx); err != nil {
		s.log.Error("dispatch", "err", err)
	}
}

// expireQueued drops queued runs older than their job's queue.max_age,
// so a run waiting behind a long one doesn't look queued after it expired.
func (s *Service) expireQueued(ctx context.Context) error {
	pending, err := queryRuns(ctx, s.st.db, `status='pending' ORDER BY created_at`)
	if err != nil {
		return err
	}
	now := s.now()
	specs := map[string]*pa.Job{}
	for _, p := range pending {
		j, ok := specs[p.Artifact]
		if !ok {
			if _, spec, err := s.currentSpec(ctx, s.st.db, p.Artifact); err == nil && spec.Job != nil {
				d := s.cfg.jobDefaults(*spec.Job)
				j = &d
			}
			specs[p.Artifact] = j
		}
		if j == nil || j.OnBusy != pa.OnBusyQueue || j.Queue.MaxAge <= 0 || now.Sub(p.CreatedAt) <= time.Duration(j.Queue.MaxAge) {
			continue
		}
		dropped := false
		err := s.st.tx(ctx, func(q querier) error {
			cur, err := getRun(ctx, q, p.ID)
			if err != nil || cur.Status != pa.RunPending {
				return err
			}
			dropped = true
			return s.finishPending(ctx, q, cur, pa.RunDropped, "expired", now)
		})
		if err != nil {
			return err
		}
		if dropped {
			s.emit(pa.Event{Type: "run.dropped", Artifact: p.Artifact, RunID: p.ID, Status: pa.RunDropped, Reason: "expired"})
		}
	}
	return nil
}

// dueJobs returns the artifacts whose job's schedule is due.
func dueJobs(ctx context.Context, q querier, now time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT artifact FROM jobs WHERE sched_key<>'' AND next_at>0 AND next_at<=?`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// minInterval is the shortest gap between firings of a key's schedules:
// limits.min_interval, or the key's own if longer.
func (s *Service) minInterval(ctx context.Context, q querier, key string) (time.Duration, error) {
	quota, err := s.effectiveQuota(ctx, q, key)
	if err != nil {
		return 0, err
	}
	interval := time.Duration(s.cfg.Limits.MinInterval)
	if m := time.Duration(quota.MinInterval); m > interval {
		interval = m
	}
	return interval, nil
}

// nextFire is the first slot of c a schedule that last fired at last may
// fire at, so next_run is always a time it will run: firings are at least
// interval apart, less slack for the scheduler getting to a slot a few
// seconds late (or an every-minute schedule would skip every other slot).
// A catch-up firing after downtime counts from when it happened, so the
// next slot can't follow it a second later.
func nextFire(c *Cron, last, now time.Time, interval, slack time.Duration) time.Time {
	if !last.IsZero() {
		if from := last.Add(interval - slack); from.After(now) {
			// the first slot at or after from
			return c.Next(from.Add(time.Minute - time.Nanosecond).Truncate(time.Minute).Add(-time.Second))
		}
	}
	return c.Next(now)
}

// fireSlack is how late the scheduler may get to a due slot.
func (s *Service) fireSlack() time.Duration {
	return time.Duration(s.cfg.SchedulerInterval) + time.Second
}

// fireSchedules turns due schedules into pending runs. A schedule missed
// while the service was down fires once, not once per missed slot.
func (s *Service) fireSchedules(ctx context.Context) error {
	now := s.now()
	due, err := dueJobs(ctx, s.st.db, now)
	if err != nil {
		return err
	}
	for _, id := range due {
		var events []pa.Event
		err := s.st.tx(ctx, func(q querier) error {
			r, err := getJob(ctx, q, id)
			if err != nil {
				return err
			}
			_, spec, err := s.currentSpec(ctx, q, id)
			if err != nil {
				return err
			}
			var raw pa.Job
			if spec.Job != nil {
				raw = *spec.Job
			}
			j := s.cfg.jobDefaults(raw)
			if spec.Job == nil || j.Schedule == "" {
				r.SchedKey, r.NextAt = "", time.Time{}
				return putJob(ctx, q, r)
			}
			c, err := ParseCron(j.Schedule, j.Timezone)
			if err != nil {
				r.SchedKey, r.NextAt = "", time.Time{}
				return putJob(ctx, q, r)
			}
			a, err := getArtifact(ctx, q, id)
			if err != nil {
				return err
			}
			interval, err := s.minInterval(ctx, q, a.QuotaKey)
			if err != nil {
				return err
			}
			r.NextAt = nextFire(c, r.LastFireAt, now, interval, s.fireSlack())
			if st := s.jobStatus(raw, r, 0, now); st.State != pa.JobScheduled {
				return putJob(ctx, q, r)
			}
			if !r.LastFireAt.IsZero() && now.Before(r.LastFireAt.Add(interval-s.fireSlack())) {
				s.log.Debug("schedule skipped: min interval", "artifact", id, "interval", interval)
				return putJob(ctx, q, r)
			}
			r.LastFireAt = now
			r.NextAt = nextFire(c, now, now, interval, s.fireSlack())
			if err := putJob(ctx, q, r); err != nil {
				return err
			}
			_, dropped, err := s.enqueue(ctx, q, id, raw, pa.TriggerSchedule, nil, now)
			for _, x := range dropped {
				events = append(events, pa.Event{Type: "run.dropped", Artifact: id, RunID: x.ID, Status: x.Status, Reason: x.Reason})
			}
			if err != nil && asError(err).Code == "queue_full" {
				x := &runRow{Run: pa.Run{ID: newID("run-"), Artifact: id, Status: pa.RunDropped, Trigger: pa.TriggerSchedule,
					Reason: "queue_full", CreatedAt: now, FinishedAt: &now}}
				events = append(events, pa.Event{Type: "run.dropped", Artifact: id, RunID: x.ID, Status: x.Status, Reason: x.Reason})
				return putRun(ctx, q, x)
			}
			return err
		})
		if err != nil {
			s.log.Error("fire schedule", "artifact", id, "err", err)
			continue
		}
		for _, e := range events {
			s.emit(e)
		}
	}
	return nil
}

// dispatch starts pending runs while limits allow: one run at a time per
// artifact and the service-wide cap. Per-user compute limits are fired's
// (runner sandboxes carry the quota key): a run fired refuses goes back to
// pending, and that key's runs wait (quotaBackoff) before trying again;
// when fired is unavailable, every run waits firedBackoff.
// Runs that can't start yet stay pending.
func (s *Service) dispatch(ctx context.Context) error {
	s.mu.Lock()
	wait := s.now().Before(s.firedWait)
	s.mu.Unlock()
	if wait {
		return nil
	}
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
				events = append(events, pa.Event{Type: "run.dropped", Artifact: p.Artifact, RunID: p.ID, Status: pa.RunDropped, Reason: reason})
				return s.finishPending(ctx, q, cur, pa.RunDropped, reason, now)
			}
			if spec.Job == nil {
				return drop("job_removed")
			}
			raw := *spec.Job
			j := s.cfg.jobDefaults(raw)
			r, err := getJob(ctx, q, p.Artifact)
			if err != nil {
				return err
			}
			st := s.jobStatus(raw, r, 0, now)
			if st.State == pa.JobBlocked {
				return drop("needs_approval")
			}
			if j.OnBusy == pa.OnBusyQueue && j.Queue.MaxAge > 0 && now.Sub(cur.CreatedAt) > time.Duration(j.Queue.MaxAge) {
				return drop("expired")
			}
			if s.quotaWaiting(a.QuotaKey, now) {
				return nil
			}
			cur.Status, cur.StartedAt, cur.Version = pa.RunRunning, &now, a.CurrentVersion
			if err := putRun(ctx, q, cur); err != nil {
				return err
			}
			tree, err := getTree(ctx, q, a.ID, a.CurrentVersion)
			if err != nil {
				return err
			}
			start = &startInfo{run: cur, artifact: *a, job: j, maxRun: time.Duration(st.MaxRun), tree: tree}
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
		s.emit(pa.Event{Type: "run.started", Artifact: p.Artifact, Version: start.run.Version, RunID: p.ID, Status: pa.RunRunning})
		go s.execute(start)
	}
	return nil
}
