package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PolicyFunc returns the effective policy for a sandbox, or false if the
// sandbox is unknown.
type PolicyFunc func(sandboxID string) (Policy, bool)

type Options struct {
	AllowedPorts      []int
	AllowPrivateCIDRs []string
	UpstreamProxy     string
	Log               *slog.Logger
	// OnActivity is called (throttled by the caller) when bytes flow.
	OnActivity func(sandboxID string)
}

type Proxy struct {
	policy    PolicyFunc
	ports     []int
	allowNets []*net.IPNet
	upstream  *url.URL
	log       *slog.Logger
	activity  func(string)
	transport *http.Transport
}

func New(policy PolicyFunc, o Options) (*Proxy, error) {
	p := &Proxy{policy: policy, ports: o.AllowedPorts, log: o.Log, activity: o.OnActivity}
	if p.log == nil {
		p.log = slog.Default()
	}
	if len(p.ports) == 0 {
		p.ports = []int{443, 80}
	}
	for _, c := range o.AllowPrivateCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("egress.allow_private_cidrs: %w", err)
		}
		p.allowNets = append(p.allowNets, n)
	}
	if o.UpstreamProxy != "" {
		u, err := url.Parse(o.UpstreamProxy)
		if err != nil {
			return nil, fmt.Errorf("egress.upstream_proxy: %w", err)
		}
		p.upstream = u
	}
	p.transport = &http.Transport{
		DialContext:           p.dial,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		Proxy: func(*http.Request) (*url.URL, error) {
			return p.upstream, nil
		},
	}
	return p, nil
}

var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// blockedIP reports whether ip is a non-public address that sandboxes may not reach.
func (p *Proxy) blockedIP(ip net.IP) bool {
	for _, n := range p.allowNets {
		if n.Contains(ip) {
			return false
		}
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() || cgnat.Contains(ip)
}

// dial connects to addr, refusing blocked IPs after DNS resolution (so DNS
// rebinding cannot be used to reach the host).
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout: 15 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("unexpected dial address %q", address)
			}
			// The upstream proxy itself is trusted configuration.
			if p.upstream != nil {
				return nil
			}
			if p.blockedIP(ip) {
				return &blockedError{ip: ip}
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, addr)
}

type blockedError struct{ ip net.IP }

func (e *blockedError) Error() string {
	return fmt.Sprintf("destination %s is a private or loopback address", e.ip)
}

// ServeConn handles one proxy client connection from a sandbox.
func (p *Proxy) ServeConn(sandboxID string, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Minute))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		pol, ok := p.policy(sandboxID)
		if !ok {
			writeStatus(c, http.StatusForbidden, "fired egress: unknown sandbox")
			return
		}
		if req.Method == http.MethodConnect {
			p.handleConnect(sandboxID, pol, c, br, req)
			return
		}
		if !p.handleHTTP(sandboxID, pol, c, req) {
			return
		}
	}
}

func splitHostPortDefault(hostport string, def int) (string, int, error) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		var aerr *net.AddrError
		if errors.As(err, &aerr) && strings.Contains(aerr.Err, "missing port") {
			return strings.Trim(hostport, "[]"), def, nil
		}
		return "", 0, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func (p *Proxy) check(sandboxID string, pol Policy, host string, port int) error {
	if !pol.Allows(host, port, p.ports) {
		p.log.Info("egress denied", "sandbox", sandboxID, "host", host, "port", port)
		return fmt.Errorf("fired egress: %s:%d is not in this sandbox's allowlist", host, port)
	}
	if ip := net.ParseIP(host); ip != nil && p.blockedIP(ip) {
		p.log.Info("egress denied (private address)", "sandbox", sandboxID, "host", host)
		return fmt.Errorf("fired egress: %s is a private or loopback address", host)
	}
	return nil
}

func (p *Proxy) handleConnect(sandboxID string, pol Policy, c net.Conn, br *bufio.Reader, req *http.Request) {
	host, port, err := splitHostPortDefault(req.Host, 443)
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "fired egress: bad CONNECT target")
		return
	}
	if err := p.check(sandboxID, pol, host, port); err != nil {
		writeStatus(c, http.StatusForbidden, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	upstream, err := p.connect(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	cancel()
	if err != nil {
		var be *blockedError
		if errors.As(err, &be) {
			writeStatus(c, http.StatusForbidden, "fired egress: "+be.Error())
		} else {
			writeStatus(c, http.StatusBadGateway, "fired egress: "+err.Error())
		}
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent after the CONNECT request (e.g. a TLS hello)
	// may already be buffered.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := upstream.Write(b); err != nil {
			return
		}
		_, _ = br.Discard(n)
	}
	p.pipe(sandboxID, c, upstream)
}

// connect dials the target directly or through the upstream proxy.
func (p *Proxy) connect(ctx context.Context, target string) (net.Conn, error) {
	if p.upstream == nil {
		return p.dial(ctx, "tcp", target)
	}
	uc, err := p.dial(ctx, "tcp", p.upstream.Host)
	if err != nil {
		return nil, err
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if p.upstream.User != nil {
		pw, _ := p.upstream.User.Password()
		req.SetBasicAuth(p.upstream.User.Username(), pw)
	}
	if err := req.Write(uc); err != nil {
		uc.Close()
		return nil, err
	}
	br := bufio.NewReader(uc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		uc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		uc.Close()
		return nil, fmt.Errorf("upstream proxy: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: uc, r: br}, nil
	}
	return uc, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (p *Proxy) handleHTTP(sandboxID string, pol Policy, c net.Conn, req *http.Request) bool {
	if req.URL == nil || !req.URL.IsAbs() || req.URL.Scheme != "http" {
		writeStatus(c, http.StatusBadRequest, "fired egress: only absolute http:// URLs and CONNECT are supported")
		return false
	}
	host, port, err := splitHostPortDefault(req.URL.Host, 80)
	if err != nil {
		writeStatus(c, http.StatusBadRequest, "fired egress: bad host")
		return false
	}
	if err := p.check(sandboxID, pol, host, port); err != nil {
		writeStatus(c, http.StatusForbidden, err.Error())
		return !req.Close
	}
	req.RequestURI = ""
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		var be *blockedError
		if errors.As(err, &be) {
			writeStatus(c, http.StatusForbidden, "fired egress: "+be.Error())
		} else {
			writeStatus(c, http.StatusBadGateway, "fired egress: "+err.Error())
		}
		return !req.Close
	}
	defer resp.Body.Close()
	if p.activity != nil {
		p.activity(sandboxID)
	}
	if err := resp.Write(c); err != nil {
		return false
	}
	return !req.Close && !resp.Close
}

func (p *Proxy) pipe(sandboxID string, a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if p.activity != nil {
					p.activity(sandboxID)
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

func writeStatus(w io.Writer, code int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nX-Fired-Egress: denied\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}
