package fsutil

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// The cold-tier archive is a zstd stream of entries. Unlike tar it records
// file holes explicitly, so a 20 GiB sparse disk image with 300 MiB of data
// reads and writes only 300 MiB.
//
//	"FIREDAR1"
//	repeated: uint32 BE header length, JSON header, then file data segments
//	final header {"t":"end"}

const archiveMagic = "FIREDAR1"

type arHeader struct {
	Path  string    `json:"p,omitempty"`
	Type  string    `json:"t"` // d, f, l, end
	Mode  uint32    `json:"m,omitempty"`
	Size  int64     `json:"s,omitempty"`
	Segs  []Segment `json:"g,omitempty"`
	Link  string    `json:"k,omitempty"`
	UID   int       `json:"u,omitempty"`
	GID   int       `json:"i,omitempty"`
	MTime int64     `json:"tm,omitempty"`
}

// Pack archives the directory tree at src into dst (written atomically).
// skip, if non-nil, is called with slash-separated relative paths.
func Pack(src, dst string, skip func(rel string, d fs.DirEntry) bool) error {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	bw := bufio.NewWriterSize(f, 1<<20)
	zw, err := zstd.NewWriter(bw, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(2))
	if err != nil {
		return err
	}
	if _, err := zw.Write([]byte(archiveMagic)); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip != nil && skip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		h := arHeader{Path: rel, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()}
		if st, ok := sysStat(info); ok {
			h.UID, h.GID = int(st.Uid), int(st.Gid)
			h.Mode = st.Mode & 0o7777
		}
		switch {
		case info.Mode().IsDir():
			h.Type = "d"
			return writeHeader(zw, &h)
		case info.Mode()&os.ModeSymlink != 0:
			h.Type = "l"
			h.Link, err = os.Readlink(p)
			if err != nil {
				return err
			}
			return writeHeader(zw, &h)
		case info.Mode().IsRegular():
			h.Type = "f"
			h.Size = info.Size()
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			h.Segs, err = DataSegments(in)
			if err != nil {
				return err
			}
			if err := writeHeader(zw, &h); err != nil {
				return err
			}
			for _, s := range h.Segs {
				if _, err := io.CopyBuffer(zw, io.NewSectionReader(in, s.Off, s.Len), buf); err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
			}
			return nil
		default:
			return nil // sockets, fifos, devices are not archived
		}
	})
	if err != nil {
		return err
	}
	if err := writeHeader(zw, &arHeader{Type: "end"}); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return os.Rename(tmp, dst)
}

func writeHeader(w io.Writer, h *arHeader) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Unpack extracts an archive produced by Pack into dst (created if needed).
func Unpack(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return err
	}
	defer zr.Close()
	magic := make([]byte, len(archiveMagic))
	if _, err := io.ReadFull(zr, magic); err != nil || string(magic) != archiveMagic {
		return fmt.Errorf("%s: not a fired archive", src)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	type dirTime struct {
		path string
		mt   time.Time
	}
	var dirs []dirTime
	buf := make([]byte, 1<<20)
	for {
		var l [4]byte
		if _, err := io.ReadFull(zr, l[:]); err != nil {
			return fmt.Errorf("truncated archive: %w", err)
		}
		n := binary.BigEndian.Uint32(l[:])
		if n > 1<<24 {
			return errors.New("corrupt archive header")
		}
		hb := make([]byte, n)
		if _, err := io.ReadFull(zr, hb); err != nil {
			return err
		}
		var h arHeader
		if err := json.Unmarshal(hb, &h); err != nil {
			return err
		}
		if h.Type == "end" {
			break
		}
		target, err := safeJoin(dst, h.Path)
		if err != nil {
			return err
		}
		mode := os.FileMode(h.Mode & 0o777)
		switch h.Type {
		case "d":
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			_ = os.Chmod(target, mode|modeExtra(h.Mode))
			_ = os.Lchown(target, h.UID, h.GID)
			dirs = append(dirs, dirTime{target, time.Unix(0, h.MTime)})
		case "l":
			_ = os.Remove(target)
			if err := os.Symlink(h.Link, target); err != nil {
				return err
			}
			_ = os.Lchown(target, h.UID, h.GID)
		case "f":
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			for _, s := range h.Segs {
				if _, err := out.Seek(s.Off, io.SeekStart); err != nil {
					out.Close()
					return err
				}
				if _, err := io.CopyBuffer(out, io.LimitReader(zr, s.Len), buf); err != nil {
					out.Close()
					return err
				}
			}
			if err := out.Truncate(h.Size); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			_ = os.Lchown(target, h.UID, h.GID)
			_ = os.Chmod(target, mode|modeExtra(h.Mode))
			mt := time.Unix(0, h.MTime)
			_ = os.Chtimes(target, mt, mt)
		default:
			return fmt.Errorf("unknown archive entry type %q", h.Type)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chtimes(dirs[i].path, dirs[i].mt, dirs[i].mt)
	}
	return nil
}

func modeExtra(m uint32) os.FileMode {
	var fm os.FileMode
	if m&0o4000 != 0 {
		fm |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		fm |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		fm |= os.ModeSticky
	}
	return fm
}

func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean("/" + rel)
	p := filepath.Join(root, clean)
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry escapes destination: %q", rel)
	}
	return p, nil
}
