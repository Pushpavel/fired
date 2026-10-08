package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
)

// DialFunc opens a raw connection to the guest agent.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client is the host-side client of the guest agent.
type Client struct {
	dial DialFunc
	HTTP *http.Client
}

// BaseURL is a placeholder host; requests are routed by the dialer.
const BaseURL = "http://agent"

func NewClient(dial DialFunc) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 0, // exec can run for a long time
		DisableCompression:    true,
	}
	return &Client{dial: dial, HTTP: &http.Client{Transport: tr}}
}

// Dial exposes the raw dialer (used for port tunnels).
func (c *Client) Dial(ctx context.Context) (net.Conn, error) { return c.dial(ctx) }

func (c *Client) Close() {
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return DecodeError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// DecodeError converts an agent error response to *api.Error.
func DecodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e api.Error
	if json.Unmarshal(b, &e) == nil && e.Err.Code != "" {
		return &StatusError{Status: resp.StatusCode, Err: e}
	}
	return &StatusError{Status: resp.StatusCode, Err: api.Error{Err: api.ErrorBody{Code: "agent_error", Message: fmt.Sprintf("%s: %s", resp.Status, bytes.TrimSpace(b))}}}
}

// StatusError is an error returned by the agent with its HTTP status.
type StatusError struct {
	Status int
	Err    api.Error
}

func (e *StatusError) Error() string { return e.Err.Error() }

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	return &h, c.do(ctx, "GET", "/health", nil, &h)
}

// WaitReady polls the agent until it answers or the timeout expires.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration, done <-chan struct{}) (*Health, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		h, err := c.Health(hctx)
		cancel()
		if err == nil {
			return h, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return nil, fmt.Errorf("VM exited before the agent became ready (see sandbox log)")
		case <-time.After(25 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("guest agent not ready after %s: %v", timeout, lastErr)
}

func (c *Client) Sync(ctx context.Context, req SyncRequest) (*SyncResponse, error) {
	var r SyncResponse
	return &r, c.do(ctx, "POST", "/sys/sync", req, &r)
}

func (c *Client) Exec(ctx context.Context, req *api.ExecRequest) (*api.ExecResult, error) {
	var r api.ExecResult
	return &r, c.do(ctx, "POST", "/exec", req, &r)
}

func (c *Client) Processes(ctx context.Context) (*api.ProcessList, error) {
	var r api.ProcessList
	return &r, c.do(ctx, "GET", "/processes", nil, &r)
}

func (c *Client) Process(ctx context.Context, pid int) (*api.Process, error) {
	var r api.Process
	return &r, c.do(ctx, "GET", fmt.Sprintf("/processes/%d", pid), nil, &r)
}

func (c *Client) ProcessOutput(ctx context.Context, pid int, q url.Values) (*api.ProcessOutput, error) {
	var r api.ProcessOutput
	return &r, c.do(ctx, "GET", fmt.Sprintf("/processes/%d/output?%s", pid, q.Encode()), nil, &r)
}

func (c *Client) Kill(ctx context.Context, pid int, signal string) (*api.Process, error) {
	var r api.Process
	q := url.Values{"signal": {signal}}
	return &r, c.do(ctx, "DELETE", fmt.Sprintf("/processes/%d?%s", pid, q.Encode()), nil, &r)
}

// Forward sends an arbitrary request to the agent (used to stream files and
// archives through the daemon without buffering).
func (c *Client) Forward(ctx context.Context, method, pathAndQuery string, body io.Reader, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, BaseURL+pathAndQuery, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return c.HTTP.Do(req)
}
