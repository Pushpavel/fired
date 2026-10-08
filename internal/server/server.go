// Package server is the daemon's HTTP API.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/pkg/api"
)

type Server struct {
	m     *sandbox.Manager
	token string
	log   *slog.Logger
	mux   *http.ServeMux
}

func New(m *sandbox.Manager, token string, log *slog.Logger) *Server {
	s := &Server{m: m, token: token, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	h := s.mux.HandleFunc
	h("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	h("GET /metrics", s.handleMetrics)
	h("GET /v1/host", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.m.Host()) })
	h("GET /v1/events", s.handleEvents)

	h("GET /v1/templates", s.handleListTemplates)
	h("POST /v1/templates", s.handleCreateTemplate)
	h("GET /v1/templates/{ref}", s.handleGetTemplate)
	h("GET /v1/templates/{ref}/logs", s.handleTemplateLogs)
	h("DELETE /v1/templates/{ref}", s.handleDeleteTemplate)

	h("POST /v1/sandboxes", s.handleCreate)
	h("GET /v1/sandboxes", s.handleList)
	h("GET /v1/sandboxes/{id}", s.handleGet)
	h("PATCH /v1/sandboxes/{id}", s.handleUpdate)
	h("DELETE /v1/sandboxes/{id}", s.handleDelete)
	h("POST /v1/sandboxes/{id}/resume", s.lifecycle(s.m.Resume))
	h("POST /v1/sandboxes/{id}/suspend", s.lifecycle(s.m.Suspend))
	h("POST /v1/sandboxes/{id}/archive", s.lifecycle(s.m.Archive))
	h("GET /v1/sandboxes/{id}/logs", s.handleVMLogs)

	h("POST /v1/sandboxes/{id}/exec", s.handleExec)
	h("GET /v1/sandboxes/{id}/exec/stream", s.handleExecStream)
	h("GET /v1/sandboxes/{id}/processes", s.handleProcesses)
	h("GET /v1/sandboxes/{id}/processes/{pid}", s.handleProcess)
	h("GET /v1/sandboxes/{id}/processes/{pid}/output", s.handleProcessOutput)
	h("DELETE /v1/sandboxes/{id}/processes/{pid}", s.handleKill)

	h("GET /v1/sandboxes/{id}/files", s.forwardFiles("GET", "/files"))
	h("PUT /v1/sandboxes/{id}/files", s.forwardFiles("PUT", "/files"))
	h("DELETE /v1/sandboxes/{id}/files", s.forwardFiles("DELETE", "/files"))
	h("POST /v1/sandboxes/{id}/files/mkdir", s.forwardFiles("POST", "/files/mkdir"))
	h("POST /v1/sandboxes/{id}/files/move", s.forwardFiles("POST", "/files/move"))
	h("GET /v1/sandboxes/{id}/files/archive", s.forwardFiles("GET", "/archive"))
	h("PUT /v1/sandboxes/{id}/files/archive", s.forwardFiles("PUT", "/archive"))

	h("GET /v1/sandboxes/{id}/ports", s.handleListPorts)
	h("POST /v1/sandboxes/{id}/ports", s.handleExpose)
	h("DELETE /v1/sandboxes/{id}/ports/{port}", s.handleUnexpose)
	h("/v1/sandboxes/{id}/ports/{port}/proxy", s.handlePortProxy)
	h("/v1/sandboxes/{id}/ports/{port}/proxy/{path...}", s.handlePortProxy)
}

// Handler returns the root handler with auth and logging.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if r.URL.Path != "/healthz" && !s.authorized(r) {
			writeError(w, &sandbox.Error{Status: 401, Code: "unauthorized", Msg: "missing or invalid API token"})
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: 200}
		s.mux.ServeHTTP(sw, r)
		lvl := slog.LevelInfo
		if strings.Contains(r.URL.Path, "/proxy") || r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			lvl = slog.LevelDebug
		}
		s.log.Log(r.Context(), lvl, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	got := r.Header.Get("X-Fired-Token")
	if got == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			got = strings.TrimPrefix(a, "Bearer ")
		}
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
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

// Unwrap lets http.ResponseController reach Flush/Hijack.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	e := sandbox.AsError(err)
	writeJSON(w, e.Status, api.Error{Err: api.ErrorBody{Code: e.Code, Message: e.Msg}})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return &sandbox.Error{Status: 400, Code: "invalid_request", Msg: "invalid JSON: " + err.Error()}
	}
	return nil
}

// ---- templates

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	ts, err := s.m.ListTemplates(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.TemplateList{Templates: ts})
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTemplateRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := s.m.CreateTemplate(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.GetTemplate(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) handleTemplateLogs(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.GetTemplate(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeError(w, err)
		return
	}
	serveTail(w, r, s.m.BuildLogPath(t.ID))
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	ids, err := s.m.DeleteTemplate(r.Context(), r.PathValue("ref"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": ids})
}

// ---- sandboxes

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSandboxRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	sb, err := s.m.Create(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// GET /v1/sandboxes?label=key=value&label=k2=v2&state=running
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	labels := map[string]string{}
	for _, l := range r.URL.Query()["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			k, v, _ = strings.Cut(l, ":")
		}
		labels[k] = v
	}
	writeJSON(w, 200, api.SandboxList{Sandboxes: s.m.List(labels, r.URL.Query().Get("state"))})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sb, err := s.m.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, sb)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req api.UpdateSandboxRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	sb, err := s.m.Update(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, sb)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lifecycle(fn func(ctx context.Context, id string) (*api.Sandbox, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sb, err := fn(r.Context(), r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, sb)
	}
}

func (s *Server) handleVMLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.m.Get(id); err != nil {
		writeError(w, err)
		return
	}
	serveTail(w, r, s.m.LogPath(id))
}

// serveTail returns the last ?bytes= (default 64 KiB) of a file.
func serveTail(w http.ResponseWriter, r *http.Request, path string) {
	n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
	if n <= 0 {
		n = 64 << 10
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, &sandbox.Error{Status: 404, Code: "not_found", Msg: "no log yet"})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if st != nil && st.Size() > n {
		_, _ = f.Seek(st.Size()-n, io.SeekStart)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, f)
}

// ---- exec & processes

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req api.ExecRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.m.Exec(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func pidOf(r *http.Request) (int, error) {
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		return 0, &sandbox.Error{Status: 400, Code: "invalid_request", Msg: "bad pid"}
	}
	return pid, nil
}

func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	res, err := s.m.Processes(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleProcess(w http.ResponseWriter, r *http.Request) {
	pid, err := pidOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := s.m.Process(r.Context(), r.PathValue("id"), pid)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleProcessOutput(w http.ResponseWriter, r *http.Request) {
	pid, err := pidOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := s.m.ProcessOutput(r.Context(), r.PathValue("id"), pid, r.URL.Query())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	pid, err := pidOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := s.m.KillProcess(r.Context(), r.PathValue("id"), pid, r.URL.Query().Get("signal"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// forwardFiles streams a files request to the guest agent unchanged.
func (s *Server) forwardFiles(method, agentPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l, err := s.m.Acquire(r.Context(), r.PathValue("id"), true)
		if err != nil {
			writeError(w, err)
			return
		}
		defer l.Release()
		hdr := http.Header{}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			hdr.Set("Content-Type", ct)
		}
		var body io.Reader
		if method == "PUT" || method == "POST" {
			body = r.Body
		}
		pq := agentPath
		if r.URL.RawQuery != "" {
			pq += "?" + r.URL.RawQuery
		}
		resp, err := l.Agent.Forward(r.Context(), method, pq, body, hdr)
		if err != nil {
			writeError(w, sandbox.AgentErr(err))
			return
		}
		defer resp.Body.Close()
		for _, k := range []string{"Content-Type", "Content-Length", "X-Fired-Type", "X-Fired-Mode"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}
}

// ---- ports

func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	sb, err := s.m.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.PortList{Ports: sb.Ports})
}

func (s *Server) handleExpose(w http.ResponseWriter, r *http.Request) {
	var req api.ExposePortRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	sb, err := s.m.ExposePort(r.Context(), r.PathValue("id"), req.Port)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.PortList{Ports: sb.Ports})
}

func (s *Server) handleUnexpose(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeError(w, &sandbox.Error{Status: 400, Code: "invalid_request", Msg: "bad port"})
		return
	}
	sb, err := s.m.UnexposePort(r.Context(), r.PathValue("id"), port)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.PortList{Ports: sb.Ports})
}

// ---- metrics

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	h := s.m.Host()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE fired_memory_budget_mb gauge\nfired_memory_budget_mb %d\n", h.MemoryBudgetMB)
	fmt.Fprintf(w, "# TYPE fired_memory_running_mb gauge\nfired_memory_running_mb %d\n", h.MemoryRunningMB)
	fmt.Fprintf(w, "# TYPE fired_sandboxes gauge\n")
	for _, st := range []api.SandboxState{api.StateCreating, api.StateRunning, api.StateSuspending, api.StateSuspended,
		api.StateArchiving, api.StateArchived, api.StateResuming, api.StateStopped, api.StateDeleting, api.StateError} {
		fmt.Fprintf(w, "fired_sandboxes{state=%q} %d\n", st, h.Counts[st])
	}
	fmt.Fprintf(w, "# TYPE fired_hot_dir_used_bytes gauge\nfired_hot_dir_used_bytes %d\n", h.HotDirUsedBytes)
	fmt.Fprintf(w, "# TYPE fired_cold_dir_used_bytes gauge\nfired_cold_dir_used_bytes %d\n", h.ColdDirUsedBytes)
}

// ---- events (SSE)

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	_ = rc.Flush()
	filter := r.URL.Query().Get("sandbox")
	ch, cancel := s.m.Bus().Subscribe()
	defer cancel()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
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
			if filter != "" && ev.SandboxID != filter {
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
