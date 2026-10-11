package artifacts

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// Handler returns the HTTP API. Every route except /healthz (and the
// console's login page) needs the token.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	h := mux.HandleFunc
	h("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	h("GET /v1/events", s.handleEvents)

	h("POST /v1/blobs/missing", s.handleMissingBlobs)
	h("PUT /v1/blobs/{hash}", s.handlePutBlob)

	h("POST /v1/artifacts", s.handleCreate)
	h("GET /v1/artifacts", s.handleList)
	h("GET /v1/artifacts/{id}", s.handleGet)
	h("PATCH /v1/artifacts/{id}", s.handleUpdate)
	h("DELETE /v1/artifacts/{id}", s.handleDelete)
	h("POST /v1/artifacts/{id}/versions", s.handlePublish)
	h("GET /v1/artifacts/{id}/versions", s.handleVersions)
	h("GET /v1/artifacts/{id}/versions/{v}", s.handleVersion)
	h("GET /v1/artifacts/{id}/versions/{v}/export", s.handleExport)
	h("POST /v1/artifacts/{id}/restore", s.handleRestore)
	h("POST /v1/artifacts/{id}/fork", s.handleFork)
	h("GET /v1/artifacts/{id}/files/{path...}", s.handleFile)

	h("GET /v1/artifacts/{id}/jobs", s.handleJobs)
	h("PUT /v1/artifacts/{id}/jobs/{job}/grant", s.handleGrant)
	h("POST /v1/artifacts/{id}/jobs/{job}/runs", s.handleTrigger)
	h("GET /v1/artifacts/{id}/runs", s.handleRuns)
	h("GET /v1/artifacts/{id}/runs/{run}", s.handleRun)
	h("GET /v1/artifacts/{id}/runs/{run}/logs", s.handleRunLogs)

	h("GET /v1/tiers", s.handleTiers)
	h("GET /v1/tiers/{name}", s.handleGetTier)
	h("PUT /v1/tiers/{name}", s.handlePutTier)
	h("DELETE /v1/tiers/{name}", s.handleDeleteTier)
	h("GET /v1/quotas/{key}", s.handleGetQuota)
	h("PUT /v1/quotas/{key}", s.handlePutQuota)
	if s.cfg.UI {
		s.uiRoutes(mux)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if id, n, ok := s.previewTarget(r.Host); ok {
			s.serveView(w, r, id, n)
			return
		}
		// Header-routed requests always need the token: "/" is otherwise
		// public (the console's redirect).
		routed := r.Header.Get("X-Artifact") != ""
		if (routed || !s.public(r.URL.Path)) && !s.authorized(r) {
			writeErr(w, errf(http.StatusUnauthorized, "unauthorized", "missing or invalid API token"))
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: 200}
		if routed {
			s.handleRouted(sw, r, r.Header.Get("X-Artifact"))
		} else {
			mux.ServeHTTP(sw, r)
		}
		lvl := slog.LevelInfo
		if r.URL.Path == "/healthz" || strings.Contains(r.URL.Path, "/files/") {
			lvl = slog.LevelDebug
		}
		s.log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Service) authorized(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	got := r.Header.Get("X-Fired-Token")
	if got == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			got = strings.TrimPrefix(a, "Bearer ")
		}
	}
	if got != "" {
		return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
	}
	return s.cfg.UI && s.uiSession(r)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	e := asError(err)
	writeJSON(w, e.Status, api.Error{Err: api.ErrorBody{Code: e.Code, Message: e.Msg}})
}

func decode(r *http.Request, v any, max int64) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return badRequest("request body is required")
		}
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// bodyLimit bounds JSON request bodies (inline content included).
func (s *Service) bodyLimit() int64 { return s.cfg.Limits.MaxInlineBytes*2 + 1<<20 }

// ---- blobs

func (s *Service) handleMissingBlobs(w http.ResponseWriter, r *http.Request) {
	var req pa.MissingBlobsRequest
	if err := decode(r, &req, 16<<20); err != nil {
		writeErr(w, err)
		return
	}
	m, err := s.MissingBlobs(req.Hashes)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, pa.MissingBlobsResult{Missing: m})
}

func (s *Service) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	h := r.PathValue("hash")
	err := s.blobs.Put(h, r.Body, s.cfg.Limits.MaxVersionBytes)
	switch err {
	case nil:
		writeJSON(w, 200, map[string]string{"hash": h})
	case errBadHash:
		writeErr(w, badRequest("%v", err))
	case ErrHashMismatch:
		writeErr(w, errf(http.StatusUnprocessableEntity, "hash_mismatch", "content does not hash to %s", h))
	case ErrTooLarge:
		writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", "blob exceeds %d bytes", s.cfg.Limits.MaxVersionBytes))
	default:
		writeErr(w, err)
	}
}

// ---- artifacts

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req pa.CreateArtifactRequest
	if err := decode(r, &req, s.bodyLimit()); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Create(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, _ := strings.Cut(l, "=")
		labels[k] = v
	}
	list, err := s.List(r.Context(), r.URL.Query().Get("quota_key"), labels)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, pa.ArtifactList{Artifacts: list})
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	a, err := s.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req pa.UpdateArtifactRequest
	if err := decode(r, &req, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	a, err := s.Update(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	var req pa.PublishRequest
	if err := decode(r, &req, s.bodyLimit()); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Publish(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, publishStatus(res), res)
}

func (s *Service) handleVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Versions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, pa.VersionList{Versions: vs})
}

// versionParam parses a version number; "current" or "" mean 0 (current).
func versionParam(v string) (int, error) {
	if v == "" || v == "current" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest("version must be a positive number or \"current\"")
	}
	return n, nil
}

func (s *Service) handleVersion(w http.ResponseWriter, r *http.Request) {
	n, err := versionParam(r.PathValue("v"))
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.Version(r.Context(), r.PathValue("id"), n, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Service) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req pa.RestoreRequest
	if err := decode(r, &req, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.Restore(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, publishStatus(res), res)
}

// publishStatus is 201 for a new version, 200 when nothing changed.
func publishStatus(res *pa.PublishResult) int {
	if res.Unchanged {
		return http.StatusOK
	}
	return http.StatusCreated
}

func (s *Service) handleFork(w http.ResponseWriter, r *http.Request) {
	var req pa.ForkRequest
	if err := decode(r, &req, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	a, err := s.Fork(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// ---- serving

// bucket is a token bucket for per-artifact read rate limiting.
type bucket struct {
	tokens float64
	last   time.Time
}

func (s *Service) allowRead(id string) bool {
	rate, burst := s.cfg.Limits.ServeRPS, float64(s.cfg.Limits.ServeBurst)
	if rate <= 0 {
		return true
	}
	if burst < 1 {
		burst = 1
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[id]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		s.buckets[id] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// noteRead records activity for delete_after, at most once a minute.
func (s *Service) noteRead(id string) {
	now := s.now()
	s.mu.Lock()
	last := s.touched[id]
	if now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.touched[id] = now
	s.mu.Unlock()
	_ = touchArtifact(context.Background(), s.st.db, id, now)
}

const memServeLimit = 8 << 20

// handleFile serves one file of a version (?version=N, default current),
// any file: this is the owner's raw access, not what viewers get.
func (s *Service) handleFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.allowRead(id) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errf(http.StatusTooManyRequests, "rate_limited", "too many requests for this artifact"))
		return
	}
	n, err := versionParam(r.URL.Query().Get("version"))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.serveFile(w, r, id, n, r.PathValue("path"), n != 0)
}

// serveFile serves path p of version n (0 = current); pinned responses are
// cacheable forever.
func (s *Service) serveFile(w http.ResponseWriter, r *http.Request, id string, n int, p string, pinned bool) {
	p, err := cleanPath(p)
	if err != nil {
		writeErr(w, badRequest("%v", err))
		return
	}
	a, err := s.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if n == 0 {
		n = a.CurrentVersion
	}
	f, err := getFile(r.Context(), s.st.db, id, n, p)
	if err != nil {
		if err == errNotFound {
			writeErr(w, errf(http.StatusNotFound, "not_found", "%s has no file %q in version %d", id, p, n))
			return
		}
		writeErr(w, err)
		return
	}
	go s.noteRead(id)
	rc, err := s.blobs.Open(f.Hash)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rc.Close()
	ct := contentType(p)
	w.Header().Set("ETag", `"`+f.Hash+`"`)
	w.Header().Set("X-Artifact-Version", strconv.Itoa(n))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if pinned {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	if f.Size <= memServeLimit {
		data, err := io.ReadAll(io.LimitReader(rc, f.Size+1))
		if err != nil {
			writeErr(w, err)
			return
		}
		if ct == "" {
			ct = http.DetectContentType(data)
		}
		w.Header().Set("Content-Type", ct)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		return
	}
	if match := r.Header.Get("If-None-Match"); match == `"`+f.Hash+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, rc)
}

// handleRouted serves a request routed by headers, for a front proxy that
// owns the domain artifacts are viewed on (and decides who may see them):
// it resolves the hostname to an artifact and forwards the request unchanged
// (same path and query) with X-Artifact, optionally X-Artifact-Version
// (a number, or "current" / absent to follow the current version), and the
// API token. It gets the artifact's view: "/" is the page, other paths are
// files of the page's folder.
func (s *Service) handleRouted(w http.ResponseWriter, r *http.Request, id string) {
	n, err := versionParam(r.Header.Get("X-Artifact-Version"))
	if err != nil {
		writeErr(w, badRequest("X-Artifact-Version must be a version number or \"current\""))
		return
	}
	s.serveView(w, r, id, n)
}

// handleExport streams a version as a zip of its files, artifact.json
// included.
func (s *Service) handleExport(w http.ResponseWriter, r *http.Request) {
	n, err := versionParam(r.PathValue("v"))
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.Version(r.Context(), r.PathValue("id"), n, true)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-v%d.zip"`, v.Artifact, v.Version))
	zw := zip.NewWriter(w)
	for _, f := range v.Files {
		hdr := &zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: v.CreatedAt}
		hdr.SetMode(0o644)
		if f.Mode == 0o755 {
			hdr.SetMode(0o755)
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return
		}
		rc, err := s.blobs.Open(f.Hash)
		if err != nil {
			s.log.Error("export: open blob", "hash", f.Hash, "err", err)
			return
		}
		_, err = io.Copy(fw, rc)
		rc.Close()
		if err != nil {
			return
		}
	}
	_ = zw.Close()
}

// ---- jobs and runs

func (s *Service) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Jobs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, pa.JobList{Jobs: jobs})
}

func (s *Service) handleGrant(w http.ResponseWriter, r *http.Request) {
	var g pa.Grant
	if err := decode(r, &g, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	st, err := s.SetGrant(r.Context(), r.PathValue("id"), r.PathValue("job"), g)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Service) handleTrigger(w http.ResponseWriter, r *http.Request) {
	var req pa.TriggerRequest
	if r.ContentLength != 0 {
		if err := decode(r, &req, int64(s.cfg.Limits.MaxPayloadBytes)+4096); err != nil {
			writeErr(w, err)
			return
		}
	}
	run, err := s.Trigger(r.Context(), r.PathValue("id"), r.PathValue("job"), req, pa.TriggerManual)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Service) handleRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	runs, err := s.Runs(r.Context(), r.PathValue("id"), q.Get("job"), q.Get("status"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, pa.RunList{Runs: runs})
}

func (s *Service) handleRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Run(r.Context(), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, run)
}

func (s *Service) handleRunLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.RunLogs(r.Context(), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, logs)
}

// ---- quotas

func (s *Service) handleGetQuota(w http.ResponseWriter, r *http.Request) {
	st, err := s.QuotaStatus(r.Context(), r.PathValue("key"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Service) handlePutQuota(w http.ResponseWriter, r *http.Request) {
	var q pa.Quota
	if err := decode(r, &q, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.SetQuota(r.Context(), r.PathValue("key"), q); err != nil {
		writeErr(w, err)
		return
	}
	st, err := s.QuotaStatus(r.Context(), r.PathValue("key"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, st)
}

// ---- tiers

func (s *Service) handleTiers(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Tiers(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, pa.TierList{Tiers: ts})
}

func (s *Service) handleGetTier(w http.ResponseWriter, r *http.Request) {
	t, err := s.Tier(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Service) handlePutTier(w http.ResponseWriter, r *http.Request) {
	var t pa.Tier
	if err := decode(r, &t, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	name := r.PathValue("name")
	if t.Name != "" && t.Name != name {
		writeErr(w, badRequest("the body's name %q differs from the path's %q", t.Name, name))
		return
	}
	t.Name = name
	if err := s.PutTier(r.Context(), t); err != nil {
		writeErr(w, err)
		return
	}
	s.handleGetTier(w, r)
}

func (s *Service) handleDeleteTier(w http.ResponseWriter, r *http.Request) {
	if err := s.DeleteTier(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- events

// handleEvents streams events as SSE. ?artifact=ID filters to one artifact.
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_ = rc.Flush()
	filter := r.URL.Query().Get("artifact")
	ch, cancel := s.bus.subscribe()
	defer cancel()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if filter != "" && ev.Artifact != filter {
				continue
			}
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}
