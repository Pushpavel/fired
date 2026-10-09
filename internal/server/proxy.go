package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Pushpavel/fired/internal/sandbox"
)

// handlePortProxy forwards HTTP (and WebSocket upgrades) to an exposed port
// inside the sandbox, resuming it first unless "X-Fired-No-Wake: 1" is set.
//
// The Host header seen by the app is X-Forwarded-Host if present, else the
// incoming Host, so dev servers that check Host (Vite allowedHosts) see the
// preview domain your backend serves.
func (s *Server) handlePortProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port <= 0 || port > 65535 {
		writeError(w, &sandbox.Error{Status: 400, Code: "invalid_request", Msg: "bad port"})
		return
	}
	s.proxyPort(w, r, id, port, "/v1/sandboxes/"+id+"/ports/"+strconv.Itoa(port)+"/proxy")
}

// proxyPort forwards the part of the request path after prefix.
func (s *Server) proxyPort(w http.ResponseWriter, r *http.Request, id string, port int, prefix string) {
	if _, err := s.m.Get(id); err != nil {
		writeError(w, err)
		return
	}
	if !s.m.PortExposed(id, port) {
		writeError(w, &sandbox.Error{Status: 403, Code: "port_not_exposed", Msg: "port " + strconv.Itoa(port) + " is not exposed; POST /v1/sandboxes/{id}/ports first"})
		return
	}
	wake := r.Header.Get("X-Fired-No-Wake") == ""
	l, err := s.m.Acquire(r.Context(), id, wake)
	if err != nil {
		writeError(w, err)
		return
	}
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			l.Release()
		}
	}
	defer release()

	rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if rest == "" || rest[0] != '/' {
		rest = "/" + rest
	}
	unescaped, err := url.PathUnescape(rest)
	if err != nil {
		unescaped = rest
	}
	touch := func() { s.m.Touch(id) }

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = sandbox.PortHost(port)
			pr.Out.URL.Path = unescaped
			pr.Out.URL.RawPath = rest
			pr.Out.URL.RawQuery = r.URL.RawQuery
			host, _, _ := strings.Cut(pr.In.Header.Get("X-Forwarded-Host"), ",")
			host = strings.TrimSpace(host)
			if host == "" {
				host = pr.In.Host
			}
			pr.Out.Host = host
			// Keep forwarding headers set by the caller (Rewrite strips them).
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Get(h); v != "" {
					pr.Out.Header.Set(h, v)
				}
			}
			pr.Out.Header.Del("X-Fired-Token")
			pr.Out.Header.Del("X-Fired-No-Wake")
			stripCookie(pr.Out.Header, sessionCookie)
			if a := pr.Out.Header.Get("Authorization"); s.token != "" && a == "Bearer "+s.token {
				pr.Out.Header.Del("Authorization")
			}
		},
		Transport:     l.Transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				// Long-lived WebSocket: don't pin the sandbox as busy;
				// traffic keeps it alive instead.
				release()
				if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
					resp.Body = &touchRWC{ReadWriteCloser: rwc, touch: touch}
				}
				return nil
			}
			resp.Body = &touchReader{ReadCloser: resp.Body, touch: touch}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, sandbox.ErrPortNotListening) {
				writeError(w, &sandbox.Error{Status: 502, Code: "port_not_listening", Msg: err.Error()})
				return
			}
			writeError(w, &sandbox.Error{Status: 502, Code: "proxy_error", Msg: err.Error()})
		},
	}
	rp.ServeHTTP(w, r)
}

type touchReader struct {
	io.ReadCloser
	touch func()
	last  time.Time
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 && time.Since(t.last) > time.Second {
		t.last = time.Now()
		t.touch()
	}
	return n, err
}

type touchRWC struct {
	io.ReadWriteCloser
	touch func()
	last  atomic.Int64
}

func (t *touchRWC) bump() {
	now := time.Now().UnixNano()
	if now-t.last.Load() > int64(time.Second) {
		t.last.Store(now)
		t.touch()
	}
}

func (t *touchRWC) Read(p []byte) (int, error) {
	n, err := t.ReadWriteCloser.Read(p)
	if n > 0 {
		t.bump()
	}
	return n, err
}

func (t *touchRWC) Write(p []byte) (int, error) {
	n, err := t.ReadWriteCloser.Write(p)
	if n > 0 {
		t.bump()
	}
	return n, err
}
