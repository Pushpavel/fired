package server

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
	"strconv"
	"strings"

	"github.com/Pushpavel/fired/internal/sandbox"
)

// The browser console (config `ui: true`). It is a static page that talks
// to the same API as any other client, authenticated by a session cookie
// obtained by entering the API token once:
//
//   - The cookie holds an HMAC of the token, not the token, and is HttpOnly
//     and SameSite=Strict.
//   - Cookie-authenticated requests must be same-origin (Sec-Fetch-Site /
//     Origin), so other sites and other ports on the same host can't ride
//     on the session.
//   - Port previews get their own origin, so apps run at "/" with working
//     absolute paths, routers, HMR, cookies and storage:
//     p{port}-{sandbox}-{sig}.{domain}. The HMAC sig in the hostname is the
//     capability (no cookie needed, so previews also work inside the
//     console's iframe). {domain} is `preview_domain` (a wildcard DNS name),
//     or else nip.io for the IP the console was reached on, e.g.
//     p3000-web-1a2b3c4d5e6f.127-0-0-1.nip.io:8443. A preview origin is
//     cross-site from the console, so it can't use the console session, and
//     the session cookie is never forwarded to sandbox apps.

//go:embed ui
var uiFiles embed.FS

const sessionCookie = "fired_session"

// EnableUI serves the console at /ui/ and port previews on
// p{port}-{sandbox}-{sig}.{previewDomain}; empty previewDomain uses nip.io.
func (s *Server) EnableUI(previewDomain string) {
	s.ui = true
	s.previewDomain = strings.Trim(strings.ToLower(previewDomain), ".")
	sub, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServerFS(sub)
	s.mux.Handle("GET /ui/", http.StripPrefix("/ui", files))
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	s.mux.HandleFunc("GET /ui/session", s.handleUISession)
	s.mux.HandleFunc("POST /ui/login", s.handleUILogin)
	s.mux.HandleFunc("POST /ui/logout", s.handleUILogout)
	s.mux.HandleFunc("POST /ui/preview-link", s.handlePreviewLink)
}

// public reports paths that skip API authentication.
func (s *Server) public(path string) bool {
	if path == "/healthz" {
		return true
	}
	if !s.ui {
		return false
	}
	// /ui/preview-link checks the session itself.
	return path == "/" || strings.HasPrefix(path, "/ui/")
}

func (s *Server) mac(parts ...string) string {
	m := hmac.New(sha256.New, []byte(s.token))
	m.Write([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) sessionValue() string { return s.mac("fired-ui-session") }

// previewSig is 12 hex chars so the label stays within DNS's 63: "p65535-"
// + a 40-char sandbox id + "-" + sig = 60.
func (s *Server) previewSig(id string, port int) string {
	return s.mac("fired-preview", id, strconv.Itoa(port))[:12]
}

// uiSession reports whether r carries a valid session cookie from a
// same-origin context.
func (s *Server) uiSession(r *http.Request) bool {
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

func (s *Server) handleUISession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"authenticated": s.token == "" || s.uiSession(r)})
}

func (s *Server) handleUILogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !sameOrigin(r) || !hmac.Equal([]byte(req.Token), []byte(s.token)) {
		writeError(w, &sandbox.Error{Status: 401, Code: "unauthorized", Msg: "wrong API token"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.sessionValue(), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePreviewLink(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, &sandbox.Error{Status: 401, Code: "unauthorized", Msg: "missing or invalid API token"})
		return
	}
	var req struct {
		Sandbox string `json:"sandbox"`
		Port    int    `json:"port"`
	}
	if err := decode(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if _, err := s.m.Get(req.Sandbox); err != nil {
		writeError(w, err)
		return
	}
	domain, err := s.previewDomainFor(r)
	if err != nil {
		writeError(w, &sandbox.Error{Status: 409, Code: "conflict", Msg: err.Error()})
		return
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := fmt.Sprintf("p%d-%s-%s.%s", req.Port, req.Sandbox, s.previewSig(req.Sandbox, req.Port), domain)
	if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "80" && p != "443" {
		host = net.JoinHostPort(host, p)
	}
	writeJSON(w, 200, map[string]string{"url": scheme + "://" + host + "/"})
}

// previewDomainFor is the configured preview domain, or nip.io for the IP
// address the client used to reach the console.
func (s *Server) previewDomainFor(r *http.Request) (string, error) {
	if s.previewDomain != "" {
		return s.previewDomain, nil
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
		return "", fmt.Errorf("can't build a nip.io preview address for %q; set preview_domain to a wildcard DNS name that points at fired", host)
	}
	return strings.ReplaceAll(ip.To4().String(), ".", "-") + ".nip.io", nil
}

// previewTarget parses a preview hostname into its sandbox and port and
// checks the signature.
func (s *Server) previewTarget(hostport string) (id string, port int, ok bool) {
	if !s.ui {
		return "", 0, false
	}
	host := strings.ToLower(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, domain, found := strings.Cut(host, ".")
	if !found || !strings.HasPrefix(label, "p") {
		return "", 0, false
	}
	if s.previewDomain != "" {
		if domain != s.previewDomain {
			return "", 0, false
		}
	} else if !strings.HasSuffix(domain, ".nip.io") {
		return "", 0, false
	}
	portStr, rest, ok1 := strings.Cut(label[1:], "-")
	i := strings.LastIndexByte(rest, '-')
	if !ok1 || i <= 0 {
		return "", 0, false
	}
	id, sig := rest[:i], rest[i+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 || !hmac.Equal([]byte(sig), []byte(s.previewSig(id, port))) {
		return "", 0, false
	}
	return id, port, true
}

// stripCookie removes one cookie from a request's Cookie headers.
func stripCookie(h http.Header, name string) {
	cs := h.Values("Cookie")
	if len(cs) == 0 {
		return
	}
	var keep []string
	for _, line := range cs {
		for _, part := range strings.Split(line, ";") {
			if k, _, _ := strings.Cut(strings.TrimSpace(part), "="); k != name && strings.TrimSpace(part) != "" {
				keep = append(keep, strings.TrimSpace(part))
			}
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}
