package artifacts

import (
	"context"
	"time"
)

const (
	// blobGrace protects freshly uploaded blobs that no version references
	// yet (uploaded, publish not committed) from garbage collection.
	blobGrace = 2 * time.Hour
	// runHistory is how long finished run records (and their logs) are kept.
	runHistory = 30 * 24 * time.Hour
)

// GC deletes artifacts past their delete_after, old run records, and
// blobs nothing references any more.
func (s *Service) GC(ctx context.Context) {
	now := s.now()
	arts, err := listArtifacts(ctx, s.st.db, "")
	if err != nil {
		s.log.Error("gc: list artifacts", "err", err)
		return
	}
	for _, a := range arts {
		ttl := time.Duration(a.DeleteAfter)
		if ttl == 0 {
			ttl = time.Duration(s.cfg.Limits.DeleteAfter)
		}
		if ttl <= 0 || now.Sub(a.LastActiveAt) < ttl {
			continue
		}
		s.mu.Lock()
		busy := s.running[a.ID] != ""
		s.mu.Unlock()
		if busy {
			continue
		}
		s.log.Info("gc: deleting inactive artifact", "artifact", a.ID, "last_active", a.LastActiveAt)
		if err := s.Delete(ctx, a.ID); err != nil {
			s.log.Error("gc: delete artifact", "artifact", a.ID, "err", err)
		}
	}
	if _, err := s.st.db.ExecContext(ctx, `DELETE FROM runs WHERE finished_at>0 AND finished_at<? AND status NOT IN ('pending','running')`,
		ms(now.Add(-runHistory))); err != nil {
		s.log.Error("gc: prune runs", "err", err)
	}
	// Chunked files nothing references (past the grace period) go, and the
	// chunks of the rest count as referenced.
	if _, err := s.st.db.ExecContext(ctx, `DELETE FROM chunked WHERE touched_at<?
AND file NOT IN (SELECT hash FROM files UNION SELECT hash FROM output_files UNION SELECT logs FROM runs)`, ms(now.Add(-blobGrace))); err != nil {
		s.log.Error("gc: chunked files", "err", err)
		return
	}
	if _, err := s.st.db.ExecContext(ctx, `DELETE FROM chunks WHERE file NOT IN (SELECT file FROM chunked)`); err != nil {
		s.log.Error("gc: chunk lists", "err", err)
		return
	}
	refs, err := referencedBlobs(ctx, s.st.db)
	if err != nil {
		s.log.Error("gc: referenced blobs", "err", err)
		return
	}
	removed := 0
	_ = s.blobs.Walk(func(h string, mod time.Time) error {
		if refs[h] || now.Sub(mod) < blobGrace {
			return nil
		}
		if err := s.blobs.Remove(h); err == nil {
			removed++
		}
		return ctx.Err()
	})
	s.blobs.CleanTmp(blobGrace)
	if removed > 0 {
		s.log.Info("gc: removed unreferenced blobs", "count", removed)
	}
}
