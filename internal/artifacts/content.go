package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// File contents are blobs, named by the SHA-256 of their bytes. A file of
// pa.ChunkThreshold or more is stored as content-defined chunks instead:
// each chunk is a blob, and the chunks table lists a file's chunks in
// order under the file's hash. Everything above this layer (versions,
// serving, runs, the API) still names files by that hash.

// hasContent reports whether file h is stored, as a blob or as chunks, and
// refreshes it for garbage collection: whoever asked is about to reference
// it, so it gets the grace period again.
func (s *Service) hasContent(ctx context.Context, h string) bool {
	now := s.now()
	if s.blobs.Touch(h, now) {
		return true
	}
	chunks, _, err := chunkList(ctx, s.st.db, h)
	if err != nil || chunks == nil {
		return false
	}
	for _, c := range chunks {
		if !s.blobs.Touch(c.Hash, now) {
			return false
		}
	}
	_, err = s.st.db.ExecContext(ctx, `UPDATE chunked SET touched_at=? WHERE file=?`, ms(now), h)
	return err == nil
}

// openContent returns the bytes of file h.
func (s *Service) openContent(ctx context.Context, h string) (io.ReadCloser, error) {
	rc, err := s.blobs.Open(h)
	if !errors.Is(err, os.ErrNotExist) {
		return rc, err
	}
	chunks, _, cerr := chunkList(ctx, s.st.db, h)
	if cerr != nil {
		return nil, cerr
	}
	if chunks == nil {
		return nil, err
	}
	return &chunkReader{blobs: s.blobs, chunks: chunks}, nil
}

// contentSize is the size of file h.
func (s *Service) contentSize(ctx context.Context, h string) (int64, error) {
	if _, size, err := chunkList(ctx, s.st.db, h); err != nil || size > 0 {
		return size, err
	}
	return s.blobSize(h)
}

// readContent returns file h, refusing files above max bytes.
func (s *Service) readContent(ctx context.Context, h string, max int64) ([]byte, error) {
	r, err := s.openContent(ctx, h)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

// putContent stores r (at most max bytes; 0 = no limit) and returns its
// hash and size: as one blob, or chunked when it's large. If expect is set
// and the content hashes differently, nothing is recorded under either
// hash and the real hash is returned (stored chunks are left to GC).
func (s *Service) putContent(ctx context.Context, r io.Reader, max int64, expect string) (string, int64, error) {
	head := make([]byte, pa.ChunkThreshold)
	n, err := io.ReadFull(r, head)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		if max > 0 && int64(n) > max {
			return "", 0, ErrTooLarge
		}
		return s.blobs.write(bytes.NewReader(head[:n]), max, expect)
	}
	if err != nil {
		return "", 0, err
	}
	src := io.MultiReader(bytes.NewReader(head), r)
	if max > 0 {
		src = io.LimitReader(src, max+1)
	}
	hs := sha256.New()
	var chunks []pa.ChunkRef
	var size int64
	err = pa.SplitChunks(io.TeeReader(src, hs), func(c []byte) error {
		size += int64(len(c))
		if max > 0 && size > max {
			return ErrTooLarge
		}
		h, err := s.blobs.PutBytes(c)
		chunks = append(chunks, pa.ChunkRef{Hash: h, Size: int64(len(c))})
		return err
	})
	if err != nil {
		return "", 0, err
	}
	got := hex.EncodeToString(hs.Sum(nil))
	if expect != "" && got != expect {
		return got, size, nil
	}
	return got, size, s.recordChunks(ctx, got, size, chunks)
}

// putChunkList records file h as chunks that are already stored, after
// checking they are and that they add up to h (PUT /v1/blobs/{h}/chunks).
func (s *Service) putChunkList(ctx context.Context, h string, chunks []pa.ChunkRef) (int64, error) {
	if !validHash(h) {
		return 0, badRequest("%v", errBadHash)
	}
	max := s.cfg.Limits.MaxVersionBytes
	if len(chunks) == 0 || int64(len(chunks)) > max/(64<<10)+1 {
		return 0, badRequest("chunks: give the file's chunks in order (1 to %d)", max/(64<<10)+1)
	}
	if s.hasContent(ctx, h) {
		var size int64
		for _, c := range chunks {
			size += c.Size
		}
		return size, nil
	}
	for i, c := range chunks {
		if !validHash(c.Hash) {
			return 0, badRequest("chunks[%d]: %v", i, errBadHash)
		}
		if !s.blobs.Touch(c.Hash, s.now()) {
			return 0, errf(http.StatusUnprocessableEntity, "missing_blob", "chunks[%d]: blob %s has not been uploaded", i, c.Hash)
		}
	}
	// Read the chunks back to check the whole: the file's hash is what
	// versions and viewers rely on.
	hs := sha256.New()
	real := make([]pa.ChunkRef, len(chunks))
	var size int64
	for i, c := range chunks {
		rc, err := s.blobs.Open(c.Hash)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(hs, rc)
		rc.Close()
		if err != nil {
			return 0, err
		}
		real[i] = pa.ChunkRef{Hash: c.Hash, Size: n}
		size += n
		if size > max {
			return 0, errf(http.StatusRequestEntityTooLarge, "too_large", "the chunks add up to more than %d bytes", max)
		}
	}
	if got := hex.EncodeToString(hs.Sum(nil)); got != h {
		return 0, errf(http.StatusUnprocessableEntity, "hash_mismatch", "the chunks, in this order, hash to %s, not %s", got, h)
	}
	return size, s.recordChunks(ctx, h, size, real)
}

func (s *Service) recordChunks(ctx context.Context, h string, size int64, chunks []pa.ChunkRef) error {
	return s.st.tx(ctx, func(q querier) error {
		if _, err := q.ExecContext(ctx, `INSERT INTO chunked(file, size, touched_at) VALUES(?,?,?)
ON CONFLICT(file) DO UPDATE SET touched_at=excluded.touched_at`, h, size, ms(s.now())); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM chunks WHERE file=?`, h); err != nil {
			return err
		}
		for i, c := range chunks {
			if _, err := q.ExecContext(ctx, `INSERT INTO chunks(file, idx, chunk, size) VALUES(?,?,?,?)`, h, i, c.Hash, c.Size); err != nil {
				return err
			}
		}
		return nil
	})
}

// chunkList returns file h's chunks and size, or nil if it isn't chunked.
func chunkList(ctx context.Context, q querier, h string) ([]pa.ChunkRef, int64, error) {
	var size int64
	if err := q.QueryRowContext(ctx, `SELECT size FROM chunked WHERE file=?`, h).Scan(&size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT chunk, size FROM chunks WHERE file=? ORDER BY idx`, h)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []pa.ChunkRef
	for rows.Next() {
		var c pa.ChunkRef
		if err := rows.Scan(&c.Hash, &c.Size); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, size, rows.Err()
}

// chunkReader reads a chunked file's chunks one after another.
type chunkReader struct {
	blobs  *Blobs
	chunks []pa.ChunkRef
	cur    io.ReadCloser
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if len(r.chunks) == 0 {
				return 0, io.EOF
			}
			rc, err := r.blobs.Open(r.chunks[0].Hash)
			if err != nil {
				return 0, err
			}
			r.cur, r.chunks = rc, r.chunks[1:]
		}
		n, err := r.cur.Read(p)
		if err == io.EOF {
			r.cur.Close()
			r.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (r *chunkReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}
