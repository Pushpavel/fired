package artifacts

import (
	"context"
	"math"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// The job works in .output/. A run starts with the latest output there and
// whatever it leaves becomes the next output, not a version: versions are
// what the author published (the source), outputs are what the source
// produces. What everyone sees (viewers, exports, forks, the next run) is
// the version's files plus the current output under .output/.

func isOutput(p string) bool { return under(p, pa.OutputDir) }

// liveTree is what artifact a shows at version n: its files plus the
// current output.
func (s *Service) liveTree(ctx context.Context, q querier, a *pa.Artifact, n int) (map[string]pa.File, error) {
	tree, err := getTree(ctx, q, a.ID, n)
	if err != nil || a.Output == 0 {
		return tree, err
	}
	out, err := getOutputFiles(ctx, q, a.ID, a.Output)
	if err != nil {
		return nil, err
	}
	for p, f := range out {
		tree[p] = f
	}
	return tree, nil
}

// liveFile is one file of liveTree.
func (s *Service) liveFile(ctx context.Context, q querier, a *pa.Artifact, n int, p string) (*pa.File, error) {
	if !isOutput(p) {
		return getFile(ctx, q, a.ID, n, p)
	}
	if a.Output == 0 {
		return nil, errNotFound
	}
	out, err := getOutputFiles(ctx, q, a.ID, a.Output)
	if err != nil {
		return nil, err
	}
	if f, ok := out[p]; ok {
		return &f, nil
	}
	return nil, errNotFound
}

// currentOutput is the current output's files ({} for none).
func currentOutput(ctx context.Context, q querier, a *pa.Artifact) (map[string]pa.File, error) {
	if a.Output == 0 {
		return map[string]pa.File{}, nil
	}
	return getOutputFiles(ctx, q, a.ID, a.Output)
}

// Outputs lists the outputs kept, newest first, with their files.
func (s *Service) Outputs(ctx context.Context, id string) (*pa.OutputList, error) {
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
	return &pa.OutputList{Current: a.Output, Outputs: outs}, nil
}

// ClearOutputs drops the outputs: .output/ is empty again, and the next
// run starts from nothing.
func (s *Service) ClearOutputs(ctx context.Context, id string) error {
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
		s.emit(pa.Event{Type: "artifact.output", Artifact: id})
	}
	return err
}

// commitOutput records what a run left in .output/ as the next output,
// unless it's the current one. It returns nil when nothing changed.
func (s *Service) commitOutput(ctx context.Context, q querier, r *runRow, captured map[string]pa.File, now time.Time) (*pa.Output, error) {
	a, err := getArtifact(ctx, q, r.Artifact)
	if err != nil {
		return nil, err
	}
	before, err := currentOutput(ctx, q, a)
	if err != nil {
		return nil, err
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

// skipOutput drops publish ops under .output/ (the job's folder is never
// part of a version) and returns their paths.
func skipOutput(ops []resolvedOp) ([]resolvedOp, []string) {
	var skipped []string
	out := ops[:0]
	for _, op := range ops {
		if op.path != "" && isOutput(op.path) {
			if !op.delete && !op.clear {
				skipped = append(skipped, op.path)
			}
			continue
		}
		out = append(out, op)
	}
	return out, skipped
}
