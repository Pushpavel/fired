package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
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
//   - Port previews are served under /v1/preview/{id}/{port}/{sig}/ where sig
//     is an HMAC capability for that one port, with a CSP sandbox so the
//     sandboxed app gets an opaque origin and can't call the API with the
//     session. The session cookie is never forwarded to sandbox apps.

//go:embed ui
var uiFiles embed.FS

const sessionCookie = "fired_session"

// EnableUI serves the console at /ui/.
func (s *Server) EnableUI() {
	s.ui = true
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
	s.mux.HandleFunc("/v1/preview/{id}/{port}/{sig}", s.handlePreview)
	s.mux.HandleFunc("/v1/preview/{id}/{port}/{sig}/{path...}", s.handlePreview)
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
	return path == "/" || strings.HasPrefix(path, "/ui/") || strings.HasPrefix(path, "/v1/preview/")
}

func (s *Server) mac(parts ...string) string {
	m := hmac.New(sha256.New, []byte(s.token))
	m.Write([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) sessionValue() string { return s.mac("fired-ui-session") }

func (s *Server) previewSig(id string, port int) string {
	return s.mac("fired-preview", id, strconv.Itoa(port))[:32]
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
	writeJSON(w, 200, map[string]string{"url": "/v1/preview/" + req.Sandbox + "/" + strconv.Itoa(req.Port) + "/" + s.previewSig(req.Sandbox, req.Port) + "/"})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id, sig := r.PathValue("id"), r.PathValue("sig")
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port <= 0 || port > 65535 || !hmac.Equal([]byte(sig), []byte(s.previewSig(id, port))) {
		writeError(w, &sandbox.Error{Status: 401, Code: "unauthorized", Msg: "invalid preview link"})
		return
	}
	// Opaque origin: the app can't read the console's cookies or call the
	// API with them.
	w.Header().Add("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads")
	s.proxyPort(w, r, id, port, "/v1/preview/"+id+"/"+strconv.Itoa(port)+"/"+sig)
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
