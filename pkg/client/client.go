// Package client is a Go client for the fired API.
//
//	c := client.New("https://sandbox-host:8443", os.Getenv("FIRED_API_TOKEN"))
//	sb, _ := c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "node"})
//	res, _ := c.Exec(ctx, sb.ID, api.ExecRequest{Cmd: "node -v"})
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/Pushpavel/fired/pkg/api"
)

type Client struct {
	base  string
	token string
	HTTP  *http.Client
}

// New creates a client. baseURL is e.g. "http://10.0.0.5:8443".
func New(baseURL, token string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, HTTP: &http.Client{}}
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("X-Fired-Token", c.token)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeErr(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// HTTPError is returned for non-2xx responses.
type HTTPError struct {
	Status int
	Body   api.Error
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("fired: %d %s: %s", e.Status, e.Body.Err.Code, e.Body.Err.Message)
}

// Code is the machine-readable error code (e.g. "not_found", "capacity").
func (e *HTTPError) Code() string { return e.Body.Err.Code }

func decodeErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	e := &HTTPError{Status: resp.StatusCode}
	if json.Unmarshal(b, &e.Body) != nil || e.Body.Err.Code == "" {
		e.Body.Err = api.ErrorBody{Code: "http_error", Message: strings.TrimSpace(string(b))}
	}
	return e
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	he, ok := err.(*HTTPError)
	return ok && he.Status == http.StatusNotFound
}

func sb(id string) string { return "/v1/sandboxes/" + url.PathEscape(id) }

// ---- templates

func (c *Client) CreateTemplate(ctx context.Context, req api.CreateTemplateRequest) (*api.Template, error) {
	var t api.Template
	return &t, c.do(ctx, "POST", "/v1/templates", req, &t)
}

func (c *Client) GetTemplate(ctx context.Context, ref string) (*api.Template, error) {
	var t api.Template
	return &t, c.do(ctx, "GET", "/v1/templates/"+url.PathEscape(ref), nil, &t)
}

func (c *Client) ListTemplates(ctx context.Context) ([]api.Template, error) {
	var l api.TemplateList
	return l.Templates, c.do(ctx, "GET", "/v1/templates", nil, &l)
}

func (c *Client) DeleteTemplate(ctx context.Context, ref string) error {
	return c.do(ctx, "DELETE", "/v1/templates/"+url.PathEscape(ref), nil, nil)
}

// ---- sandboxes

func (c *Client) CreateSandbox(ctx context.Context, req api.CreateSandboxRequest) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, "POST", "/v1/sandboxes", req, &s)
}

func (c *Client) GetSandbox(ctx context.Context, id string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, "GET", sb(id), nil, &s)
}

// ListSandboxes filters by labels (all must match) and optional state.
func (c *Client) ListSandboxes(ctx context.Context, labels map[string]string, state api.SandboxState) ([]api.Sandbox, error) {
	q := url.Values{}
	for k, v := range labels {
		q.Add("label", k+"="+v)
	}
	if state != "" {
		q.Set("state", string(state))
	}
	var l api.SandboxList
	return l.Sandboxes, c.do(ctx, "GET", "/v1/sandboxes?"+q.Encode(), nil, &l)
}

func (c *Client) UpdateSandbox(ctx context.Context, id string, req api.UpdateSandboxRequest) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, "PATCH", sb(id), req, &s)
}

func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", sb(id), nil, nil)
}

func (c *Client) Resume(ctx context.Context, id string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, "POST", sb(id)+"/resume", nil, &s)
}

func (c *Client) Suspend(ctx context.Context, id string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, "POST", sb(id)+"/suspend", nil, &s)
}

func (c *Client) Archive(ctx context.Context, id string) (*api.Sandbox, error) {
	var s api.Sandbox
	return &s, c.do(ctx, "POST", sb(id)+"/archive", nil, &s)
}

// ---- exec

func (c *Client) Exec(ctx context.Context, id string, req api.ExecRequest) (*api.ExecResult, error) {
	var r api.ExecResult
	return &r, c.do(ctx, "POST", sb(id)+"/exec", req, &r)
}

func (c *Client) Processes(ctx context.Context, id string) ([]api.Process, error) {
	var l api.ProcessList
	return l.Processes, c.do(ctx, "GET", sb(id)+"/processes", nil, &l)
}

// ProcessOutput reads background process output after the given offsets.
// waitMS > 0 long-polls until new output arrives or the process exits.
func (c *Client) ProcessOutput(ctx context.Context, id string, pid int, sinceStdout, sinceStderr int64, waitMS int) (*api.ProcessOutput, error) {
	q := url.Values{
		"since_stdout": {strconv.FormatInt(sinceStdout, 10)},
		"since_stderr": {strconv.FormatInt(sinceStderr, 10)},
	}
	if waitMS > 0 {
		q.Set("wait_ms", strconv.Itoa(waitMS))
	}
	var o api.ProcessOutput
	return &o, c.do(ctx, "GET", fmt.Sprintf("%s/processes/%d/output?%s", sb(id), pid, q.Encode()), nil, &o)
}

func (c *Client) Kill(ctx context.Context, id string, pid int, signal string) (*api.Process, error) {
	var p api.Process
	return &p, c.do(ctx, "DELETE", fmt.Sprintf("%s/processes/%d?signal=%s", sb(id), pid, url.QueryEscape(signal)), nil, &p)
}

// Stream is an interactive exec session.
type Stream struct {
	conn *websocket.Conn
	ctx  context.Context
}

// ExecStream starts a streaming exec (optionally with a PTY).
func (c *Client) ExecStream(ctx context.Context, id string, req api.ExecRequest) (*Stream, error) {
	u := strings.Replace(c.base, "http", "ws", 1) + sb(id) + "/exec/stream"
	h := http.Header{}
	if c.token != "" {
		h.Set("X-Fired-Token", c.token)
	}
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.HTTP, HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(16 << 20)
	b, _ := json.Marshal(req)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		conn.CloseNow()
		return nil, err
	}
	return &Stream{conn: conn, ctx: ctx}, nil
}

// Recv returns the next output chunk (channel 1 = stdout, 2 = stderr) or event.
func (s *Stream) Recv() (channel byte, data []byte, ev *api.StreamEvent, err error) {
	t, p, err := s.conn.Read(s.ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	if t == websocket.MessageBinary {
		if len(p) == 0 {
			return 0, nil, nil, nil
		}
		return p[0], p[1:], nil, nil
	}
	var e api.StreamEvent
	if err := json.Unmarshal(p, &e); err != nil {
		return 0, nil, nil, err
	}
	return 0, nil, &e, nil
}

func (s *Stream) Stdin(p []byte) error { return s.conn.Write(s.ctx, websocket.MessageBinary, p) }

func (s *Stream) Control(c api.StreamControl) error {
	b, _ := json.Marshal(c)
	return s.conn.Write(s.ctx, websocket.MessageText, b)
}

func (s *Stream) Close() error { return s.conn.Close(websocket.StatusNormalClosure, "") }

// ---- files

// ReadFile returns file contents.
func (c *Client) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	rc, err := c.OpenFile(ctx, id, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// OpenFile streams file contents.
func (c *Client) OpenFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, "GET", sb(id)+"/files?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeErr(resp)
	}
	if resp.Header.Get("X-Fired-Type") == "dir" {
		resp.Body.Close()
		return nil, fmt.Errorf("%s is a directory", path)
	}
	return resp.Body, nil
}

func (c *Client) ListDir(ctx context.Context, id, path string) (*api.DirListing, error) {
	var l api.DirListing
	return &l, c.do(ctx, "GET", sb(id)+"/files?path="+url.QueryEscape(path), nil, &l)
}

func (c *Client) Stat(ctx context.Context, id, path string) (*api.FileInfo, error) {
	var fi api.FileInfo
	return &fi, c.do(ctx, "GET", sb(id)+"/files?stat=1&path="+url.QueryEscape(path), nil, &fi)
}

// WriteFile writes content (mode 0 = 0644). Parent directories are created.
func (c *Client) WriteFile(ctx context.Context, id, path string, content io.Reader, mode uint32) (*api.FileInfo, error) {
	q := url.Values{"path": {path}}
	if mode != 0 {
		q.Set("mode", strconv.FormatUint(uint64(mode), 8))
	}
	req, err := c.newRequest(ctx, "PUT", sb(id)+"/files?"+q.Encode(), content)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, decodeErr(resp)
	}
	var fi api.FileInfo
	return &fi, json.NewDecoder(resp.Body).Decode(&fi)
}

func (c *Client) DeleteFile(ctx context.Context, id, path string, recursive bool) error {
	q := url.Values{"path": {path}}
	if recursive {
		q.Set("recursive", "1")
	}
	return c.do(ctx, "DELETE", sb(id)+"/files?"+q.Encode(), nil, nil)
}

func (c *Client) Mkdir(ctx context.Context, id, path string) error {
	return c.do(ctx, "POST", sb(id)+"/files/mkdir?path="+url.QueryEscape(path), nil, nil)
}

func (c *Client) Move(ctx context.Context, id, from, to string) error {
	return c.do(ctx, "POST", sb(id)+"/files/move", api.MoveRequest{From: from, To: to}, nil)
}

// DownloadArchive streams a tar.gz of path (excluding base names matching excludes).
func (c *Client) DownloadArchive(ctx context.Context, id, path string, excludes ...string) (io.ReadCloser, error) {
	q := url.Values{"path": {path}, "exclude": excludes}
	req, err := c.newRequest(ctx, "GET", sb(id)+"/files/archive?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeErr(resp)
	}
	return resp.Body, nil
}

// UploadArchive extracts a tar.gz into path.
func (c *Client) UploadArchive(ctx context.Context, id, path string, tgz io.Reader) error {
	req, err := c.newRequest(ctx, "PUT", sb(id)+"/files/archive?path="+url.QueryEscape(path), tgz)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeErr(resp)
	}
	return nil
}

// ---- ports

func (c *Client) ExposePort(ctx context.Context, id string, port int) error {
	return c.do(ctx, "POST", sb(id)+"/ports", api.ExposePortRequest{Port: port}, nil)
}

func (c *Client) UnexposePort(ctx context.Context, id string, port int) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("%s/ports/%d", sb(id), port), nil, nil)
}

// ProxyURL is the URL that forwards to a port inside the sandbox. Requests
// must carry the API token (X-Fired-Token); your backend adds it.
func (c *Client) ProxyURL(id string, port int, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("%s%s/ports/%d/proxy%s", c.base, sb(id), port, path)
}

// ---- host

func (c *Client) Host(ctx context.Context) (*api.HostInfo, error) {
	var h api.HostInfo
	return &h, c.do(ctx, "GET", "/v1/host", nil, &h)
}
