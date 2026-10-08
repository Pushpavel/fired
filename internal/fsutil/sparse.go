// Package fsutil has filesystem helpers: sparse-aware copies and the
// compressed archive format used for the cold tier.
package fsutil

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Segment is a run of data in a sparse file.
type Segment struct {
	Off int64 `json:"o"`
	Len int64 `json:"l"`
}

// DataSegments returns the data (non-hole) ranges of f using SEEK_DATA /
// SEEK_HOLE. Filesystems without support report the whole file as data.
func DataSegments(f *os.File) ([]Segment, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}
	fd := int(f.Fd())
	var segs []Segment
	var off int64
	for off < size {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) { // no more data
				break
			}
			if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EOPNOTSUPP) {
				return []Segment{{0, size}}, nil
			}
			return nil, err
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE)
		if err != nil {
			return nil, err
		}
		if end > size {
			end = size
		}
		segs = append(segs, Segment{start, end - start})
		off = end
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return segs, nil
}

// SparseCopy copies src to dst preserving holes. dst is created/truncated.
func SparseCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	segs, err := DataSegments(in)
	if err != nil {
		out.Close()
		return err
	}
	buf := make([]byte, 1<<20)
	for _, s := range segs {
		if err := copyRange(out, in, s.Off, s.Len, buf); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Truncate(st.Size()); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyRange(dst, src *os.File, off, n int64, buf []byte) error {
	for n > 0 {
		chunk := int64(len(buf))
		if chunk > n {
			chunk = n
		}
		r, err := src.ReadAt(buf[:chunk], off)
		if r > 0 {
			if err := writeSparse(dst, buf[:r], off); err != nil {
				return err
			}
			off += int64(r)
			n -= int64(r)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

// writeSparse writes p at off, skipping all-zero 4 KiB blocks so they stay holes.
func writeSparse(f *os.File, p []byte, off int64) error {
	const blk = 4096
	for len(p) > 0 {
		n := blk
		if n > len(p) {
			n = len(p)
		}
		if !allZero(p[:n]) {
			if _, err := f.WriteAt(p[:n], off); err != nil {
				return err
			}
		}
		p = p[n:]
		off += int64(n)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// CreateSparse creates a sparse file of the given size.
func CreateSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// AllocatedBytes returns the disk space actually used under path.
func AllocatedBytes(path string) int64 {
	var total int64
	_ = walk(path, func(p string, st *unix.Stat_t) {
		total += st.Blocks * 512
	})
	return total
}

// FreeBytes returns free space on the filesystem holding path.
func FreeBytes(path string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func walk(path string, fn func(string, *unix.Stat_t)) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	fn(path, &st)
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range ents {
		_ = walk(path+"/"+e.Name(), fn)
	}
	return nil
}
