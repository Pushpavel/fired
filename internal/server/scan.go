package server

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"

	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/pkg/api"
)

// handleScan serves GET /v1/sandboxes/{id}/files/scan?path=P[&exclude=…]:
// a manifest of the regular files under P whose hashes are computed over the
// same bytes that are checked for the sandbox's secret values. The secret
// values live only in the daemon, so the check happens here, on the guest's
// archive stream, and only secret names are returned.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	l, err := s.m.Acquire(r.Context(), id, true)
	if err != nil {
		writeError(w, err)
		return
	}
	defer l.Release()

	resp, err := l.Agent.Forward(r.Context(), "GET", "/files?stat=1&path="+url.QueryEscape(q.Get("path")), nil, http.Header{})
	if err != nil {
		writeError(w, sandbox.AgentErr(err))
		return
	}
	var fi api.FileInfo
	if resp.StatusCode != http.StatusOK {
		passThrough(w, resp)
		return
	}
	err = json.NewDecoder(resp.Body).Decode(&fi)
	resp.Body.Close()
	if err != nil {
		writeError(w, sandbox.AgentErr(err))
		return
	}
	base := fi.Path
	if !fi.IsDir {
		base = path.Dir(fi.Path)
	}

	aq := url.Values{"path": {fi.Path}, "exclude": q["exclude"]}
	resp, err = l.Agent.Forward(r.Context(), "GET", "/archive?"+aq.Encode(), nil, http.Header{})
	if err != nil {
		writeError(w, sandbox.AgentErr(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		passThrough(w, resp)
		return
	}
	set := s.m.RedactSet(id)
	out := api.FileScan{Path: fi.Path, Base: base, Files: []api.ManifestEntry{}, Found: []api.SecretMatch{},
		SecretsChecked: set.Names()}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		writeError(w, sandbox.AgentErr(fmt.Errorf("read archive: %w", err)))
		return
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, sandbox.AgentErr(fmt.Errorf("read archive: %w", err)))
			return
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		sum, secret, err := hashAndScan(tr, set)
		if err != nil {
			writeError(w, sandbox.AgentErr(fmt.Errorf("read archive: %w", err)))
			return
		}
		out.Files = append(out.Files, api.ManifestEntry{Path: hdr.Name, Size: hdr.Size,
			Mode: fmt.Sprintf("%04o", hdr.FileInfo().Mode().Perm()), SHA256: sum})
		if secret != "" {
			out.Found = append(out.Found, api.SecretMatch{Path: hdr.Name, Secret: secret})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// hashAndScan hashes r and looks for secret values in it, reading in chunks
// that overlap by the longest value so a match across chunks is found.
func hashAndScan(r io.Reader, set *redact.Set) (sum, secret string, err error) {
	h := sha256.New()
	overlap := set.MaxLen()
	buf := make([]byte, overlap+256<<10)
	keep := 0
	for {
		n, rerr := io.ReadFull(r, buf[keep:])
		h.Write(buf[keep : keep+n])
		if secret == "" && overlap > 0 {
			secret, _ = set.Find(buf[:keep+n])
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
			break
		}
		if rerr != nil {
			return "", "", rerr
		}
		total := keep + n
		keep = min(overlap, total)
		copy(buf, buf[total-keep:total])
	}
	return hex.EncodeToString(h.Sum(nil)), secret, nil
}

// passThrough copies an agent error response to the client.
func passThrough(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
