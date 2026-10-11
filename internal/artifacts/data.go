package artifacts

import (
	"context"
	"math"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// An artifact's job owns the paths in its writes. A run that changes them
// records an output, not a version: versions are what the author made.
// What everyone sees (viewers, exports, forks, the job's next run) is the
// version's files with the job's current output in place of anything under
// its writes; before the first output, or after a reset, the version's own
// files there are shown (the seed).

// owned reports whether p is under one of the job's writes.
func owned(job *pa.Job, p string) bool {
	if job == nil {
		return false
	}
	for _, w := range job.Writes {
		if under(p, w) {
			return true
		}
	}
	return false
}

// liveTree is what artifact a shows at version n (whose spec is spec):
// the version's files, with the current output over the job's writes.
func (s *Service) liveTree(ctx context.Context, q querier, a *pa.Artifact, n int, spec pa.Spec) (map[string]pa.File, error) {
	tree, err := getTree(ctx, q, a.ID, n)
	if err != nil || a.Output == 0 || spec.Job == nil || len(spec.Job.Writes) == 0 {
		return tree, err
	}
	out, err := getOutputFiles(ctx, q, a.ID, a.Output)
	if err != nil {
		return nil, err
	}
	for p := range tree {
		if owned(spec.Job, p) {
			delete(tree, p)
		}
	}
	for p, f := range out {
		if owned(spec.Job, p) {
			tree[p] = f
		}
	}
	return tree, nil
}

// liveFile is one file of liveTree.
func (s *Service) liveFile(ctx context.Context, q querier, a *pa.Artifact, n int, spec pa.Spec, p string) (*pa.File, error) {
	if a.Output != 0 && owned(spec.Job, p) {
		out, err := getOutputFiles(ctx, q, a.ID, a.Output)
		if err != nil {
			return nil, err
		}
		if f, ok := out[p]; ok {
			return &f, nil
		}
		return nil, errNotFound
	}
	return getFile(ctx, q, a.ID, n, p)
}

// Data lists the job's outputs, newest first, with their files.
func (s *Service) Data(ctx context.Context, id string) (*pa.Data, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	outs, err := listOutputs(ctx, s.st.db, id)
	if err != nil {
		return nil, err
	}
	for i := range outs {
		files, err := getOutputFiles(ctx, s.st.db, id, outs[i].Output)
		if err != nil {
			return nil, err
		}
		outs[i].Files = sortedFiles(files)
	}
	return &pa.Data{Current: a.Output, Outputs: outs}, nil
}

// ResetData drops the job's outputs, so the version's files under its
// writes show again and the next run starts from them.
func (s *Service) ResetData(ctx context.Context, id string) error {
	err := s.st.tx(ctx, func(q querier) error {
		a, err := getArtifact(ctx, q, id)
		if err != nil {
			return s.notFound(err, id)
		}
		if err := deleteOutputs(ctx, q, id, math.MaxInt32); err != nil {
			return err
		}
		a.Output, a.UpdatedAt = 0, s.now()
		return putArtifact(ctx, q, a)
	})
	if err == nil {
		s.emit(pa.Event{Type: "artifact.data", Artifact: id})
	}
	return err
}

// commitOutput records what a run wrote under the job's writes as the next
// output, unless it's what everyone already sees there. It returns nil when
// nothing changed.
func (s *Service) commitOutput(ctx context.Context, q querier, r *runRow, j pa.Job, captured map[string]pa.File, now time.Time) (*pa.Output, error) {
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
	live, err := s.liveTree(ctx, q, a, a.CurrentVersion, cur.Spec)
	if err != nil {
		return nil, err
	}
	before := map[string]pa.File{}
	for p, f := range live {
		if owned(&j, p) {
			before[p] = f
		}
	}
	if treesEqual(before, captured) {
		return nil, nil
	}
	if err := checkNoFileDirClash(captured); err != nil {
		return nil, err
	}
	size, err := s.checkTree(ctx, q, a.QuotaKey, captured)
	if err != nil {
		return nil, err
	}
	n, err := maxOutput(ctx, q, a.ID)
	if err != nil {
		return nil, err
	}
	o := &pa.Output{Output: n + 1, RunID: r.ID, Version: r.Version, FileCount: len(captured), SizeBytes: size, CreatedAt: now}
	if err := insertOutput(ctx, q, a.ID, o, captured); err != nil {
		return nil, err
	}
	a.Output, a.UpdatedAt = o.Output, now
	if err := putArtifact(ctx, q, a); err != nil {
		return nil, err
	}
	keep := a.RetainOutputs
	if keep <= 0 {
		keep = s.cfg.Limits.RetainOutputs
	}
	return o, deleteOutputs(ctx, q, a.ID, o.Output-keep+1)
}
