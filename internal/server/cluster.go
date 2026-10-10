package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/pkg/api"
)

// Any host answers for the whole cluster. Requests about one sandbox (or one
// template build) are forwarded to the host it lives on, unchanged (streams,
// WebSockets and previews included), marked with forwardedHeader so they
// are never forwarded twice, and authenticated with the shared API token.

const forwardedHeader = "X-Fired-Forwarded-By"

// forwarded reports whether a peer already routed this request here.
func forwarded(r *http.Request) bool { return r.Header.Get(forwardedHeader) != "" }

// sandboxPathID returns the sandbox id of /v1/sandboxes/{id}[/...].
func sandboxPathID(p string) string {
	rest, ok := strings.CutPrefix(p, "/v1/sandboxes/")
	if !ok || rest == "" {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// remote forwards the request to the host that owns sandbox id and reports
// whether it handled the request. Requests about this host's sandboxes (or
// unknown ones, which the local handler answers with 404) are left alone.
func (s *Server) remote(w http.ResponseWriter, r *http.Request, id string) bool {
	if id == "" || forwarded(r) || s.m.Has(id) {
		return false
	}
	sb, h, err := s.m.Owner(r.Context(), id)
	if errors.Is(err, sandbox.ErrNotFound) {
		return false
	}
	if err != nil {
		writeError(w, err)
		return true
	}
	if h.ID == s.m.HostID() {
		return false
	}
	if sb.HostDown {
		// Reads of the record still work; everything else needs the host.
		if r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+id {
			writeJSON(w, 200, sb)
			return true
		}
		writeError(w, &sandbox.Error{Status: 503, Code: "host_unavailable",
			Msg: fmt.Sprintf("sandbox %s is on host %s, which stopped responding at %s", id, h.ID, h.LastSeen.Format(time.RFC3339))})
		return true
	}
	s.forward(w, r, h, nil)
	return true
}

// forward proxies r to host h. body replaces the request body when set.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, h *api.HostRecord, body []byte) {
	target, err := url.Parse(h.URL)
	if err != nil {
		writeError(w, &sandbox.Error{Status: 502, Code: "host_unreachable", Msg: fmt.Sprintf("host %s has a bad URL %q", h.ID, h.URL)})
		return
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Keep the original Host: preview origins are routed by it.
			pr.Out.Host = pr.In.Host
			for _, k := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Get(k); v != "" {
					pr.Out.Header.Set(k, v)
				}
			}
			pr.Out.Header.Set(forwardedHeader, s.m.HostID())
			if s.token != "" {
				pr.Out.Header.Set("X-Fired-Token", s.token)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("forward to host failed", "host", h.ID, "path", r.URL.Path, "err", err)
			writeError(w, &sandbox.Error{Status: 502, Code: "host_unreachable", Msg: fmt.Sprintf("host %s: %v", h.ID, err)})
		},
	}
	rp.ServeHTTP(w, r)
}

// ---- create: placement

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSandboxRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !forwarded(r) {
		h, err := s.m.Place(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		if h.ID != s.m.HostID() {
			// Choose the id here, so a retried forward can't create two.
			if req.ID == "" {
				req.ID = sandbox.NewSandboxID()
			}
			b, _ := json.Marshal(req)
			s.forward(w, r, h, b)
			return
		}
	}
	sb, err := s.m.Create(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

// ---- hosts and quotas

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hs, err := s.m.Hosts(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.HostList{Hosts: hs})
}

func (s *Server) handleQuotas(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	pools, err := s.m.Store().QuotaPools(r.Context(), key)
	if err != nil {
		writeError(w, err)
		return
	}
	tier, err := s.m.Store().KeyTier(r.Context(), key)
	if err != nil {
		writeError(w, err)
		return
	}
	out := api.QuotaKeyStatus{QuotaKey: key, Tier: tier, Quotas: []api.QuotaStatus{}}
	for _, p := range pools {
		st, err := s.m.Store().QuotaStatus(r.Context(), key, p)
		if err != nil {
			writeError(w, err)
			return
		}
		out.Quotas = append(out.Quotas, *st)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	st, err := s.m.Store().QuotaStatus(r.Context(), r.PathValue("key"), r.PathValue("pool"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handlePutQuota(w http.ResponseWriter, r *http.Request) {
	var q api.Quota
	if err := decode(r, &q); err != nil {
		writeError(w, err)
		return
	}
	key, pool := r.PathValue("key"), r.PathValue("pool")
	if err := s.m.Store().SetQuota(r.Context(), key, pool, q); err != nil {
		writeError(w, err)
		return
	}
	s.handleQuota(w, r)
}

// ---- tiers

var tierNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func (s *Server) handleTiers(w http.ResponseWriter, r *http.Request) {
	ts, err := s.m.Store().Tiers(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, api.TierList{Tiers: ts})
}

func (s *Server) handleGetTier(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.Store().GetTier(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, &sandbox.Error{Status: 404, Code: "tier_not_found", Msg: fmt.Sprintf("tier %q not found", r.PathValue("name"))})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) handlePutTier(w http.ResponseWriter, r *http.Request) {
	var t api.Tier
	if err := decode(r, &t); err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	if !tierNameRe.MatchString(name) || (t.Name != "" && t.Name != name) {
		writeError(w, &sandbox.Error{Status: 400, Code: "invalid_request", Msg: fmt.Sprintf("tier name must match %s (and the body's name, if given)", tierNameRe)})
		return
	}
	t.Name = name
	if err := s.m.Store().PutTier(r.Context(), t); err != nil {
		writeError(w, err)
		return
	}
	s.handleGetTier(w, r)
}

func (s *Server) handleDeleteTier(w http.ResponseWriter, r *http.Request) {
	err := s.m.Store().DeleteTier(r.Context(), r.PathValue("name"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, &sandbox.Error{Status: 404, Code: "tier_not_found", Msg: fmt.Sprintf("tier %q not found", r.PathValue("name"))})
	case errors.Is(err, store.ErrTierInUse):
		writeError(w, &sandbox.Error{Status: 409, Code: "conflict", Msg: err.Error()})
	case err != nil:
		writeError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSetKeyTier maps a quota key to a tier: the one mapping fired and
// fired-artifacts both use.
func (s *Server) handleSetKeyTier(w http.ResponseWriter, r *http.Request) {
	var req api.SetQuotaKeyRequest
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	err := s.m.Store().SetKeyTier(r.Context(), r.PathValue("key"), req.Tier)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, &sandbox.Error{Status: 404, Code: "tier_not_found", Msg: fmt.Sprintf("tier %q not found; create it with PUT /v1/tiers/%s", req.Tier, req.Tier)})
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.handleQuotas(w, r)
}

// ---- templates: builds live on one host each

// remoteTemplate forwards requests about a build that lives on another host.
func (s *Server) remoteTemplate(w http.ResponseWriter, r *http.Request) bool {
	if forwarded(r) {
		return false
	}
	local, host := s.m.LocalTemplate(r.Context(), r.PathValue("ref"))
	if local {
		return false
	}
	h, err := s.m.Store().GetHost(r.Context(), host)
	if err != nil || !h.Alive {
		writeError(w, &sandbox.Error{Status: 503, Code: "host_unavailable", Msg: fmt.Sprintf("template build %s is on host %s, which is not responding", r.PathValue("ref"), host)})
		return true
	}
	s.forward(w, r, h, nil)
	return true
}

// handleDeleteTemplate deletes one build (on its host), or a name on every
// host: the spec goes first so no host builds it again.
func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	if s.remoteTemplate(w, r) {
		return
	}
	ref := r.PathValue("ref")
	isBuild := false
	if t, err := s.m.Store().GetTemplate(r.Context(), ref); err == nil && t.ID == ref {
		isBuild = true
	}
	if forwarded(r) || isBuild {
		ids, err := s.m.DeleteTemplate(r.Context(), ref)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"deleted": ids})
		return
	}
	if err := s.m.DeleteTemplateSpec(r.Context(), ref); err != nil {
		writeError(w, err)
		return
	}
	hosts, err := s.m.Hosts(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	deleted := []string{}
	var problems []string
	found := false
	for _, h := range hosts {
		if !h.Alive {
			continue
		}
		ids, err := s.deleteTemplateOn(r.Context(), h, ref)
		var se *sandbox.Error
		switch {
		case err == nil:
			found = true
			deleted = append(deleted, ids...)
		case errors.As(err, &se) && se.Code == "template_not_found":
		default:
			found = true
			problems = append(problems, fmt.Sprintf("%s: %v", h.ID, err))
		}
	}
	if !found {
		writeError(w, &sandbox.Error{Status: 404, Code: "template_not_found", Msg: fmt.Sprintf("template %q not found", ref)})
		return
	}
	if len(deleted) == 0 && len(problems) > 0 {
		writeError(w, &sandbox.Error{Status: 409, Code: "conflict", Msg: strings.Join(problems, "; ")})
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted})
}

func (s *Server) deleteTemplateOn(ctx context.Context, h api.HostRecord, name string) ([]string, error) {
	if h.ID == s.m.HostID() {
		return s.m.DeleteTemplate(ctx, name)
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", h.URL+"/v1/templates/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(forwardedHeader, s.m.HostID())
	if s.token != "" {
		req.Header.Set("X-Fired-Token", s.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e api.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, &sandbox.Error{Status: resp.StatusCode, Code: e.Err.Code, Msg: e.Err.Message}
	}
	var out struct {
		Deleted []string `json:"deleted"`
	}
	return out.Deleted, json.NewDecoder(resp.Body).Decode(&out)
}
