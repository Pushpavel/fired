package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pushpavel/fired/internal/guest"
)

// portTransport is an http.Transport whose connections are tunnels to a
// TCP port inside the guest. Requests must use the host "p<PORT>.sandbox".
type portTransport struct {
	*http.Transport
}

// PortHost is the synthetic URL host for a guest port.
func PortHost(port int) string { return "p" + strconv.Itoa(port) + ".sandbox" }

func newPortTransport(agent *guest.Client) *portTransport {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			port, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(host, "p"), ".sandbox"))
			if err != nil {
				return nil, fmt.Errorf("bad port host %q", host)
			}
			return DialGuestPort(ctx, agent, port)
		},
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	return &portTransport{tr}
}

func (t *portTransport) Close() { t.CloseIdleConnections() }

// ErrPortNotListening is returned when nothing listens on the guest port.
var ErrPortNotListening = errors.New("nothing is listening on that port inside the sandbox")

// DialGuestPort opens a raw TCP tunnel to 127.0.0.1:port inside the guest.
func DialGuestPort(ctx context.Context, agent *guest.Client, port int) (net.Conn, error) {
	c, err := agent.Dial(ctx)
	if err != nil {
		return nil, err
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		c.Close()
		if resp.StatusCode == http.StatusBadGateway {
			return nil, ErrPortNotListening
		}
		return nil, fmt.Errorf("tunnel: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_ = c.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufConn{Conn: c, r: br}, nil
	}
	return c, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// agentErr maps agent errors onto API errors.
func agentErr(err error) error {
	var se *guest.StatusError
	if errors.As(err, &se) {
		return &Error{Status: se.Status, Code: se.Err.Err.Code, Msg: se.Err.Err.Message}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &Error{Status: http.StatusBadGateway, Code: "agent_unreachable", Msg: err.Error()}
}

// AgentErr is exported for the API layer.
func AgentErr(err error) error { return agentErr(err) }
