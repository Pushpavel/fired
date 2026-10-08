package egress

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicy(t *testing.T) {
	p, err := Compile([]string{"github.com", "*.npmjs.org", "git.internal:8443", "10.1.2.3:22"})
	if err != nil {
		t.Fatal(err)
	}
	ports := []int{443, 80}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"github.com", 443, true},
		{"GitHub.com.", 443, true},
		{"api.github.com", 443, false},
		{"registry.npmjs.org", 443, true},
		{"npmjs.org", 443, false},
		{"github.com", 22, false},
		{"git.internal", 8443, true},
		{"git.internal", 443, false},
		{"10.1.2.3", 22, true},
		{"evil.com", 443, false},
	}
	for _, c := range cases {
		if got := p.Allows(c.host, c.port, ports); got != c.want {
			t.Errorf("Allows(%s, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	all, _ := Compile([]string{"*"})
	if !all.Allows("anything.example", 443, ports) || all.Allows("anything.example", 25, ports) {
		t.Error("* should allow any host on allowed ports only")
	}
	if _, err := Compile([]string{"bad host"}); err == nil {
		t.Error("expected error for invalid pattern")
	}
}

// proxyRequest sends one raw request through the proxy and returns the status.
func proxyRequest(t *testing.T, p *Proxy, id, raw string) (int, string) {
	t.Helper()
	a, b := net.Pipe()
	go p.ServeConn(id, b)
	defer a.Close()
	go io.WriteString(a, raw)
	resp, err := http.ReadResponse(bufio.NewReader(a), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, string(body)
}

func TestProxyBlocksPrivateAndUnlisted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	_, port, _ := net.SplitHostPort(host)
	pol, _ := Compile([]string{"127.0.0.1:" + port, "localtest.me:" + port})
	policy := func(id string) (Policy, bool) { return pol, id == "sb" }

	// Default: loopback blocked even though the allowlist matches.
	p, _ := New(policy, Options{})
	if code, body := proxyRequest(t, p, "sb", "GET "+up.URL+"/ HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); code != 403 || !strings.Contains(body, "private") {
		t.Fatalf("loopback should be blocked: %d %s", code, body)
	}
	// Explicitly allowed private range.
	p, _ = New(policy, Options{AllowPrivateCIDRs: []string{"127.0.0.0/8"}})
	if code, body := proxyRequest(t, p, "sb", "GET "+up.URL+"/ HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n"); code != 200 || body != "ok" {
		t.Fatalf("allowed request failed: %d %s", code, body)
	}
	// Not in the allowlist.
	if code, _ := proxyRequest(t, p, "sb", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); code != 403 {
		t.Fatalf("unlisted CONNECT should be denied, got %d", code)
	}
	// Unknown sandbox.
	if code, _ := proxyRequest(t, p, "other", "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); code != 403 {
		t.Fatalf("unknown sandbox should be denied, got %d", code)
	}
	// CONNECT tunnel to an allowed destination.
	a, b := net.Pipe()
	go p.ServeConn("sb", b)
	go io.WriteString(a, "CONNECT "+host+" HTTP/1.1\r\nHost: "+host+"\r\n\r\nGET / HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(a)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", err, resp)
	}
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp2.Body)
	if string(body) != "ok" {
		t.Fatalf("tunnelled body %q", body)
	}
	a.Close()
}
