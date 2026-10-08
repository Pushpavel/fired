package agent

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Pushpavel/fired/pkg/api"
)

func (a *Agent) fileInfo(guestPath string, fi os.FileInfo) api.FileInfo {
	return api.FileInfo{
		Name: fi.Name(), Path: guestPath, Size: fi.Size(), Mode: fmt.Sprintf("%04o", fi.Mode().Perm()),
		IsDir: fi.IsDir(), IsLink: fi.Mode()&os.ModeSymlink != 0, ModTime: fi.ModTime(),
	}
}

func boolParam(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "1" || v == "true" || v == "yes"
}

// GET /files?path=P[&stat=1]
// A file is returned as raw bytes; a directory as a JSON DirListing.
func (a *Agent) handleReadFile(w http.ResponseWriter, r *http.Request) {
	g, h, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	fi, err := os.Stat(h)
	if err != nil {
		fsErr(w, err)
		return
	}
	if boolParam(r, "stat") {
		writeJSON(w, http.StatusOK, a.fileInfo(g, fi))
		return
	}
	if fi.IsDir() {
		ents, err := os.ReadDir(h)
		if err != nil {
			fsErr(w, err)
			return
		}
		out := api.DirListing{Path: g, Entries: make([]api.FileInfo, 0, len(ents))}
		for _, e := range ents {
			info, err := e.Info()
			if err != nil {
				continue
			}
			out.Entries = append(out.Entries, a.fileInfo(path.Join(g, e.Name()), info))
		}
		w.Header().Set("X-Fired-Type", "dir")
		writeJSON(w, http.StatusOK, out)
		return
	}
	f, err := os.Open(h)
	if err != nil {
		fsErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Fired-Type", "file")
	w.Header().Set("X-Fired-Mode", fmt.Sprintf("%04o", fi.Mode().Perm()))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	_, _ = io.Copy(w, f)
}

// PUT /files?path=P[&mode=0644][&parents=1 (default)]
func (a *Agent) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	g, h, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	mode := os.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", fmt.Errorf("bad mode %q", m))
			return
		}
		mode = os.FileMode(v)
	}
	if r.URL.Query().Get("parents") != "0" {
		if err := os.MkdirAll(filepath.Dir(h), 0o755); err != nil {
			fsErr(w, err)
			return
		}
	}
	// Write to a temp file and rename so readers never see partial content.
	tmp, err := os.CreateTemp(filepath.Dir(h), ".fired-upload-*")
	if err != nil {
		fsErr(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		writeErr(w, http.StatusBadRequest, "upload_failed", err)
		return
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		fsErr(w, err)
		return
	}
	// Keep the owner of an existing file.
	if old, err := os.Stat(h); err == nil {
		if st, ok := statT(old); ok {
			_ = tmp.Chown(int(st.Uid), int(st.Gid))
		}
	}
	if err := tmp.Close(); err != nil {
		fsErr(w, err)
		return
	}
	if err := os.Rename(tmp.Name(), h); err != nil {
		fsErr(w, err)
		return
	}
	fi, err := os.Stat(h)
	if err != nil {
		fsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.fileInfo(g, fi))
}

// DELETE /files?path=P[&recursive=1]
func (a *Agent) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	g, h, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	if g == "/" {
		writeErr(w, http.StatusBadRequest, "invalid_request", errors.New("refusing to delete /"))
		return
	}
	if _, err := os.Lstat(h); err != nil {
		fsErr(w, err)
		return
	}
	if boolParam(r, "recursive") {
		err = os.RemoveAll(h)
	} else {
		err = os.Remove(h)
	}
	if err != nil {
		fsErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /files/mkdir?path=P
func (a *Agent) handleMkdir(w http.ResponseWriter, r *http.Request) {
	g, h, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		fsErr(w, err)
		return
	}
	fi, err := os.Stat(h)
	if err != nil {
		fsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.fileInfo(g, fi))
}

// POST /files/move {"from":..,"to":..}
func (a *Agent) handleMove(w http.ResponseWriter, r *http.Request) {
	var req api.MoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	_, from, err := a.resolve(req.From)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	g, to, err := a.resolve(req.To)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		fsErr(w, err)
		return
	}
	if err := os.Rename(from, to); err != nil {
		fsErr(w, err)
		return
	}
	fi, err := os.Lstat(to)
	if err != nil {
		fsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.fileInfo(g, fi))
}

// GET /archive?path=P[&exclude=node_modules&exclude=.git]
// Streams a tar.gz of a directory (or single file). Entries are relative to P.
func (a *Agent) handleGetArchive(w http.ResponseWriter, r *http.Request) {
	_, h, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	root, err := os.Stat(h)
	if err != nil {
		fsErr(w, err)
		return
	}
	excludes := r.URL.Query()["exclude"]
	excluded := func(rel string) bool {
		for _, ex := range excludes {
			if ok, _ := path.Match(ex, path.Base(rel)); ok {
				return true
			}
			if ok, _ := path.Match(ex, rel); ok {
				return true
			}
		}
		return false
	}
	w.Header().Set("Content-Type", "application/gzip")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	base := h
	if !root.IsDir() {
		base = filepath.Dir(h)
	}
	walkErr := filepath.WalkDir(h, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if excluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		if !info.Mode().IsRegular() && !info.IsDir() && link == "" {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			f.Close()
			return err
		}
		return nil
	})
	if walkErr != nil {
		a.log.Warn("archive failed", "err", walkErr)
	}
	_ = tw.Close()
	_ = gz.Close()
}

// PUT /archive?path=P  body: tar.gz, extracted into directory P.
func (a *Agent) handlePutArchive(w http.ResponseWriter, r *http.Request) {
	g, h, err := a.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err)
		return
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		fsErr(w, err)
		return
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_archive", err)
		return
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_archive", err)
			return
		}
		target := filepath.Join(h, filepath.Clean("/"+hdr.Name))
		if target != h && !strings.HasPrefix(target, h+string(os.PathSeparator)) {
			writeErr(w, http.StatusBadRequest, "invalid_archive", fmt.Errorf("entry %q escapes target", hdr.Name))
			return
		}
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				fsErr(w, err)
				return
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				fsErr(w, err)
				return
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				fsErr(w, err)
				return
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_archive", err)
				return
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				fsErr(w, err)
				return
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				fsErr(w, err)
				return
			}
		default:
			continue
		}
		n++
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": g, "entries": n})
}
