package artifacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Blobs is a content-addressed store: each blob is kept once, named by the
// SHA-256 of its uncompressed bytes and stored zstd-compressed. Versions,
// forks and run state all refer to blobs by hash, so identical files are
// never stored twice.
type Blobs struct {
	dir string
	tmp string
}

var errBadHash = errors.New("hash must be 64 lowercase hex characters (sha256)")

// ErrHashMismatch means uploaded bytes don't hash to the claimed name.
var ErrHashMismatch = errors.New("content does not match hash")

// ErrTooLarge means an upload exceeded its size limit.
var ErrTooLarge = errors.New("blob too large")

func OpenBlobs(dir string) (*Blobs, error) {
	b := &Blobs{dir: filepath.Join(dir, "blobs"), tmp: filepath.Join(dir, "tmp")}
	for _, d := range []string{b.dir, b.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (b *Blobs) path(h string) string { return filepath.Join(b.dir, h[:2], h+".zst") }

func (b *Blobs) Has(h string) bool {
	if !validHash(h) {
		return false
	}
	_, err := os.Stat(b.path(h))
	return err == nil
}

// Touch reports whether blob h is stored, and if so refreshes its mtime:
// whoever asked is about to reference it, so garbage collection gives it
// the grace period again instead of removing it before the publish lands.
func (b *Blobs) Touch(h string, now time.Time) bool {
	if !validHash(h) {
		return false
	}
	return os.Chtimes(b.path(h), now, now) == nil
}

// Put stores r under hash h, verifying the content. maxSize <= 0 means no
// limit. Storing an existing blob is a no-op (it refreshes its mtime so a
// concurrent garbage collection doesn't remove it before it's referenced).
func (b *Blobs) Put(h string, r io.Reader, maxSize int64) error {
	if !validHash(h) {
		return errBadHash
	}
	if p := b.path(h); fileExists(p) {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	got, _, err := b.write(r, maxSize, h)
	if err != nil {
		return err
	}
	if got != h {
		return ErrHashMismatch
	}
	return nil
}

// PutBytes stores data and returns its hash.
func (b *Blobs) PutBytes(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	return h, b.Put(h, bytes.NewReader(data), 0)
}

// Add stores r and returns its hash and size.
func (b *Blobs) Add(r io.Reader, maxSize int64) (string, int64, error) {
	return b.write(r, maxSize, "")
}

// write compresses r into a temp file and renames it into place under its
// real hash, unless expect is set and differs. It returns the real hash.
func (b *Blobs) write(r io.Reader, maxSize int64, expect string) (string, int64, error) {
	f, err := os.CreateTemp(b.tmp, "blob-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		f.Close()
		return "", 0, err
	}
	hs := sha256.New()
	src := r
	if maxSize > 0 {
		src = io.LimitReader(r, maxSize+1)
	}
	n, err := io.Copy(io.MultiWriter(enc, hs), src)
	if err == nil && maxSize > 0 && n > maxSize {
		err = ErrTooLarge
	}
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	got := hex.EncodeToString(hs.Sum(nil))
	if expect != "" && got != expect {
		return got, n, nil
	}
	dst := b.path(got)
	if fileExists(dst) {
		now := time.Now()
		_ = os.Chtimes(dst, now, now)
		return got, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", 0, err
	}
	return got, n, nil
}

// Open returns the uncompressed content of blob h.
func (b *Blobs) Open(h string) (io.ReadCloser, error) {
	if !validHash(h) {
		return nil, errBadHash
	}
	f, err := os.Open(b.path(h))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &blobReader{dec: dec, f: f}, nil
}

// ReadAll returns the content of blob h, refusing blobs above max bytes.
func (b *Blobs) ReadAll(h string, max int64) ([]byte, error) {
	r, err := b.Open(h)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("blob %s: %w", h, ErrTooLarge)
	}
	return data, nil
}

type blobReader struct {
	dec *zstd.Decoder
	f   *os.File
}

func (r *blobReader) Read(p []byte) (int, error) { return r.dec.Read(p) }
func (r *blobReader) Close() error {
	r.dec.Close()
	return r.f.Close()
}

// Remove deletes blob h.
func (b *Blobs) Remove(h string) error {
	if !validHash(h) {
		return errBadHash
	}
	err := os.Remove(b.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Walk calls fn for every stored blob with its modification time.
func (b *Blobs) Walk(fn func(h string, mod time.Time) error) error {
	return filepath.WalkDir(b.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if filepath.Ext(name) != ".zst" {
			return nil
		}
		h := name[:len(name)-4]
		if !validHash(h) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(h, info.ModTime())
	})
}

// CleanTmp removes temp files older than age (left behind by crashes).
func (b *Blobs) CleanTmp(age time.Duration) {
	ents, _ := os.ReadDir(b.tmp)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > age {
			_ = os.Remove(filepath.Join(b.tmp, e.Name()))
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
