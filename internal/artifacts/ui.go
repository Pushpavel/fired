package artifacts

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// The browser console (config `ui: true`), like fired's: a static page that
// talks to the same API as any other client, authenticated by a session
// cookie obtained by entering the API token once.
//
//   - The cookie holds an HMAC of the token, not the token, and is HttpOnly
//     and SameSite=Strict. Its name differs from fired's, since cookies are
//     shared across ports of one host.
//   - Cookie-authenticated requests must be same-origin (Sec-Fetch-Site /
//     Origin).
//   - Artifact files are user-generated HTML and JS, so the console shows
//     them on their own origin, {id}-{version}-{sig}.{domain}, where version
//     0 follows the current version. The HMAC sig in the hostname is the
//     capability. {domain} is `preview_domain` (a wildcard DNS name), or
//     else nip.io for the IP the console was reached on. A preview origin is
//     cross-site from the console, so it can't use the console session.
//   - A preview origin serves the artifact's view, as viewers get it.
//   - The Files tab renders md, csv, json, images and react files with
//     ui/view.html in a sandboxed frame (an opaque origin, no access to the
//     console), which asks the console for files; the console reads them
//     with the files API.

//go:embed ui
var uiFiles embed.FS

const sessionCookie = "fired_artifacts_session"

func (s *Service) uiRoutes(mux *http.ServeMux) {
	sub, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /ui/", http.StripPrefix("/ui", http.FileServerFS(sub)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.HandleFunc("GET /ui/session", s.handleUISession)
	mux.HandleFunc("POST /ui/login", s.handleUILogin)
	mux.HandleFunc("POST /ui/logout", s.handleUILogout)
	mux.HandleFunc("POST /ui/preview-link", s.handlePreviewLink)
	mux.HandleFunc("GET /ui/info", s.handleUIInfo)
}

// public reports paths that skip API authentication. The /ui/ handlers
// that need a session check it themselves.
func (s *Service) public(path string) bool {
	if path == "/healthz" {
		return true
	}
	return s.cfg.UI && (path == "/" || strings.HasPrefix(path, "/ui/"))
}

func (s *Service) mac(parts ...string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.Token))
	m.Write([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Service) sessionValue() string { return s.mac("fired-artifacts-ui-session") }

// previewSig is 12 hex chars; with "-{version}-" it leaves room for IDs of
// up to ~44 characters in a 63-character DNS label.
func (s *Service) previewSig(id string, n int) string {
	return s.mac("fired-artifacts-preview", id, strconv.Itoa(n))[:12]
}

func (s *Service) uiSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || !hmac.Equal([]byte(c.Value), []byte(s.sessionValue())) {
		return false
	}
	return sameOrigin(r)
}

func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

func (s *Service) handleUISession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"authenticated": s.cfg.Token == "" || s.uiSession(r)})
}

func (s *Service) handleUILogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req, 4096); err != nil {
		writeErr(w, err)
		return
	}
	if !sameOrigin(r) || !hmac.Equal([]byte(req.Token), []byte(s.cfg.Token)) {
		writeErr(w, errf(http.StatusUnauthorized, "unauthorized", "wrong API token"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.sessionValue(), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleUILogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// handleUIInfo tells the console the limits that shape what it offers.
func (s *Service) handleUIInfo(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeErr(w, errf(http.StatusUnauthorized, "unauthorized", "missing or invalid API token"))
		return
	}
	l := s.cfg.Limits
	writeJSON(w, 200, map[string]any{
		"fired_url":          s.cfg.Fired.URL,
		"broker":             s.cfg.Broker.URL != "",
		"runner_template":    s.cfg.Runner.Template,
		"runner_pool":        s.cfg.Runner.Pool,
		"runner_workdir":     s.cfg.Runner.Workdir,
		"default_active_for": l.DefaultActiveFor,
		"default_max_run":    l.DefaultMaxRun,
		"min_interval":       l.MinInterval,
		"max_inline_bytes":   l.MaxInlineBytes,
		"max_payload_bytes":  l.MaxPayloadBytes,
		"retain_outputs":     l.RetainOutputs,
	})
}

func (s *Service) handlePreviewLink(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeErr(w, errf(http.StatusUnauthorized, "unauthorized", "missing or invalid API token"))
		return
	}
	var req struct {
		Artifact string `json:"artifact"`
		Version  int    `json:"version"`
	}
	if err := decode(r, &req, 4096); err != nil {
		writeErr(w, err)
		return
	}
	if req.Version < 0 {
		writeErr(w, badRequest("version must be 0 (current) or a version number"))
		return
	}
	if _, err := s.Get(r.Context(), req.Artifact); err != nil {
		writeErr(w, err)
		return
	}
	label := fmt.Sprintf("%s-%d-%s", req.Artifact, req.Version, s.previewSig(req.Artifact, req.Version))
	if len(label) > 63 {
		writeErr(w, errf(http.StatusConflict, "conflict", "artifact ID %q is too long for a preview hostname", req.Artifact))
		return
	}
	domain, err := s.previewDomainFor(r)
	if err != nil {
		writeErr(w, errf(http.StatusConflict, "conflict", "%v", err))
		return
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := label + "." + domain
	if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "80" && p != "443" {
		host = net.JoinHostPort(host, p)
	}
	writeJSON(w, 200, map[string]string{"url": scheme + "://" + host + "/"})
}

// previewDomainFor is the configured preview domain, or nip.io for the IP
// address the client used to reach the console.
func (s *Service) previewDomainFor(r *http.Request) (string, error) {
	if d := strings.Trim(strings.ToLower(s.cfg.PreviewDomain), "."); d != "" {
		return d, nil
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var ip net.IP
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		ip = net.IPv4(127, 0, 0, 1)
	} else if ip = net.ParseIP(host); ip == nil {
		if addrs, err := net.DefaultResolver.LookupIPAddr(r.Context(), host); err == nil {
			for _, a := range addrs {
				if a.IP.To4() != nil {
					ip = a.IP
					break
				}
			}
		}
	}
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("can't build a nip.io preview address for %q; set preview_domain to a wildcard DNS name that points at fired-artifacts", host)
	}
	return strings.ReplaceAll(ip.To4().String(), ".", "-") + ".nip.io", nil
}

// previewTarget parses a preview hostname into its artifact and version
// and checks the signature.
func (s *Service) previewTarget(hostport string) (id string, n int, ok bool) {
	if !s.cfg.UI {
		return "", 0, false
	}
	host := strings.ToLower(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, domain, found := strings.Cut(host, ".")
	if !found {
		return "", 0, false
	}
	if d := strings.Trim(strings.ToLower(s.cfg.PreviewDomain), "."); d != "" {
		if domain != d {
			return "", 0, false
		}
	} else if !strings.HasSuffix(domain, ".nip.io") {
		return "", 0, false
	}
	i := strings.LastIndexByte(label, '-')
	if i <= 0 {
		return "", 0, false
	}
	rest, sig := label[:i], label[i+1:]
	j := strings.LastIndexByte(rest, '-')
	if j <= 0 {
		return "", 0, false
	}
	id = rest[:j]
	n, err := strconv.Atoi(rest[j+1:])
	if err != nil || n < 0 || !hmac.Equal([]byte(sig), []byte(s.previewSig(id, n))) {
		return "", 0, false
	}
	return id, n, true
}

// serveView serves an artifact's view, on a preview origin or routed by
// headers: "/" is the view's entry, as it is (a page renders itself; for
// other types the frontend renders it, told by X-Artifact-View), and other
// paths are files of the entry's folder by exact path (no index files,
// nothing outside it, not artifact.json). ?download on any of them is an
// attachment. Without a view there is nothing to serve.
func (s *Service) serveView(w http.ResponseWriter, r *http.Request, id string, n int) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, errf(http.StatusMethodNotAllowed, "method_not_allowed", "views are read-only"))
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.allowRead(id) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errf(http.StatusTooManyRequests, "rate_limited", "too many requests for this artifact"))
		return
	}
	v, err := s.Version(r.Context(), id, n, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	view := v.Spec.View
	if view == nil {
		writeErr(w, errf(http.StatusNotFound, "no_view", "%s has no view in version %d: set \"view\" in %s", id, v.Version, pa.SpecFile))
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case p == "":
		p = view.Entry
		w.Header().Set("X-Artifact-View", view.Type)
		w.Header().Set("X-Artifact-View-Name", path.Base(view.Entry))
	case strings.HasSuffix(p, "/"):
		writeErr(w, errf(http.StatusNotFound, "not_found", "%s is a folder; link to a file in it", r.URL.Path))
		return
	default:
		c, err := cleanPath(p)
		if err != nil {
			writeErr(w, badRequest("%v", err))
			return
		}
		// .output/ is at the root for every view; other paths are in the
		// entry's folder.
		if isOutput(c) {
			p = c
		} else {
			p = path.Join(path.Dir(view.Entry), c)
		}
		if p == pa.SpecFile {
			writeErr(w, errf(http.StatusNotFound, "not_found", "%s isn't part of the view", pa.SpecFile))
			return
		}
	}
	s.serveFile(w, r, id, v.Version, p, r.URL.Path, n != 0)
}
