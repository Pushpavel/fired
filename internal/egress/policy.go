// Package egress implements the per-sandbox outbound HTTP(S) proxy.
//
// Sandboxes have no network interface. The guest agent exposes
// 127.0.0.1:3128 inside the VM and forwards each connection over vsock to
// the host, where this proxy checks the destination against the sandbox's
// allowlist and refuses private/loopback addresses (so a sandbox can never
// reach services on the host or LAN unless explicitly allowed).
package egress

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

type rule struct {
	any      bool   // "*"
	wildcard bool   // "*.example.com" (subdomains only)
	host     string // lowercase host or IP
	port     int    // 0 = any port from AllowedPorts
}

// Policy is a compiled allowlist.
type Policy struct {
	rules []rule
}

// Compile parses allow patterns: "example.com", "*.example.com",
// "example.com:8443", "*", "10.1.2.3:22".
func Compile(patterns []string) (Policy, error) {
	var p Policy
	for _, raw := range patterns {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		var r rule
		host := s
		if h, ps, err := net.SplitHostPort(s); err == nil {
			n, err := strconv.Atoi(ps)
			if err != nil || n <= 0 || n > 65535 {
				return Policy{}, fmt.Errorf("egress pattern %q: bad port", raw)
			}
			host, r.port = h, n
		}
		switch {
		case host == "*":
			r.any = true
		case strings.HasPrefix(host, "*."):
			r.wildcard = true
			r.host = strings.TrimPrefix(host, "*.")
		default:
			r.host = host
		}
		if !r.any && (r.host == "" || strings.ContainsAny(r.host, "*/ ")) {
			return Policy{}, fmt.Errorf("egress pattern %q: invalid host", raw)
		}
		p.rules = append(p.rules, r)
	}
	return p, nil
}

// Validate returns an error if any pattern is malformed.
func Validate(patterns []string) error {
	_, err := Compile(patterns)
	return err
}

// Allows reports whether host:port may be reached. allowedPorts applies to
// rules without an explicit port.
func (p Policy) Allows(host string, port int, allowedPorts []int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range p.rules {
		if r.port != 0 {
			if r.port != port {
				continue
			}
		} else if !slices.Contains(allowedPorts, port) {
			continue
		}
		switch {
		case r.any:
			return true
		case r.wildcard:
			if strings.HasSuffix(host, "."+r.host) {
				return true
			}
		default:
			if host == r.host {
				return true
			}
		}
	}
	return false
}

// Merge returns a policy containing the rules of both.
func Merge(a, b Policy) Policy {
	return Policy{rules: append(append([]rule{}, a.rules...), b.rules...)}
}
