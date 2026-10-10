package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/pkg/api"
	"github.com/Pushpavel/fired/pkg/client"
)

// TestCluster runs two fired hosts on one database: host a runs pool
// "chat", host b runs pool "artifacts". Every request goes to host a, which
// answers for the whole cluster.
func TestCluster(t *testing.T) {
	ctx := context.Background()
	db := dbFor(t, t.TempDir())
	host := func(id string, pools ...string) *env {
		return newEnv(t, t.TempDir(), func(c *config.Config) {
			c.DatabaseURL = db
			c.Cluster.HostID = id
			c.Cluster.Pools = pools
			c.Cluster.DefaultPool = "chat"
			c.Cluster.Heartbeat = api.Duration(200 * time.Millisecond)
			c.Cluster.HostTimeout = api.Duration(time.Second)
		})
	}
	a := host("a", "chat")
	defer a.close("")
	b := host("b", "artifacts")
	bUp := true
	defer func() {
		if bUp {
			b.close("")
		}
	}()
	c := a.c
	httpErr := func(err error, status int, code string) {
		t.Helper()
		var he *client.HTTPError
		if !errors.As(err, &he) || he.Status != status || he.Code() != code {
			t.Fatalf("want %d %s, got %v", status, code, err)
		}
	}

	// Events from every host arrive on host a's stream.
	evs := make(chan api.Event, 256)
	req, _ := http.NewRequestWithContext(ctx, "GET", a.srv.URL+"/v1/events", nil)
	req.Header.Set("X-Fired-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e api.Event
				if json.Unmarshal([]byte(data), &e) == nil {
					evs <- e
				}
			}
		}
	}()
	waitEvent := func(match func(api.Event) bool) api.Event {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case e := <-evs:
				if match(e) {
					return e
				}
			case <-deadline:
				t.Fatal("event not seen")
			}
		}
	}

	hosts := must(c.Hosts(ctx))(t)
	if len(hosts) != 2 || !hosts[0].Alive || !hosts[1].Alive || hosts[1].Pools[0] != "artifacts" {
		t.Fatalf("hosts: %+v", hosts)
	}

	// A template created on a is built by b too, from the cluster's spec.
	a.template("base", false)
	var builds []api.Template
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		builds = must(c.ListTemplates(ctx))(t)
		if len(builds) == 2 && builds[0].Status == api.TemplateReady && builds[1].Status == api.TemplateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("b didn't build the template: %+v", builds)
		}
	}
	if builds[0].Host == builds[1].Host {
		t.Fatalf("one build per host: %+v", builds)
	}

	// Placement: an artifacts sandbox created through a lands on b, and
	// everything about it works through a.
	sb := must(c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts", QuotaKey: "u1",
		Labels: map[string]string{"user": "u1"}}))(t)
	if sb.Host != "b" || sb.Pool != "artifacts" || sb.State != api.StateRunning {
		t.Fatalf("placed: %+v", sb)
	}
	if !b.mgr.Has(sb.ID) || a.mgr.Has(sb.ID) {
		t.Fatal("the sandbox should live on b only")
	}
	r := must(c.Exec(ctx, sb.ID, api.ExecRequest{Cmd: "echo hi from $(cat /etc/hostname 2>/dev/null || echo b)"}))(t)
	if !strings.HasPrefix(r.Stdout, "hi from") {
		t.Fatalf("forwarded exec: %+v", r)
	}
	must(c.WriteFile(ctx, sb.ID, "x.txt", strings.NewReader("via a"), 0o644))(t)
	if got := string(must(c.ReadFile(ctx, sb.ID, "x.txt"))(t)); got != "via a" {
		t.Fatalf("forwarded files: %q", got)
	}
	st := must(c.ExecStream(ctx, sb.ID, api.ExecRequest{Cmd: "echo streamed"}))(t)
	var out strings.Builder
	for {
		ch, data, ev, err := st.Recv()
		if err != nil {
			t.Fatalf("recv: %v (out=%q)", err, out.String())
		}
		if ch == api.StreamStdout {
			out.Write(data)
		}
		if ev != nil && ev.Type == "exit" {
			break
		}
	}
	st.Close()
	if !strings.Contains(out.String(), "streamed") {
		t.Fatalf("forwarded websocket: %q", out.String())
	}
	if l := must(c.ListSandboxes(ctx, map[string]string{"user": "u1"}, ""))(t); len(l) != 1 || l[0].Host != "b" {
		t.Fatalf("cluster list: %+v", l)
	}
	must(c.Suspend(ctx, sb.ID))(t)
	waitEvent(func(e api.Event) bool { return e.SandboxID == sb.ID && e.State == api.StateSuspended && e.Host == "b" })

	// Quotas hold across hosts: in the default tier, u1 may run 1 GB in
	// artifacts.
	must(c.PutTier(ctx, api.Tier{Name: api.DefaultTier, Pools: map[string]api.Quota{"artifacts": {MaxMemoryMB: 1024}}}))(t)
	must(c.Resume(ctx, sb.ID))(t) // 512 MB
	q := must(c.Quota(ctx, "u1", "artifacts"))(t)
	if q.RunningMB != 512 || q.Effective.MaxMemoryMB != 1024 {
		t.Fatalf("quota: %+v", q)
	}
	must(c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts", QuotaKey: "u1"}))(t)
	_, err = c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts", QuotaKey: "u1"})
	httpErr(err, 429, "quota_exceeded")
	// Moving u1 to a bigger tier (one mapping, set through any host) lets
	// it start more on any host.
	must(c.PutTier(ctx, api.Tier{Name: "pro", Pools: map[string]api.Quota{"artifacts": {MaxMemoryMB: 1536}}}))(t)
	if k := must(b.c.SetTier(ctx, "u1", "pro"))(t); k.Tier != "pro" {
		t.Fatalf("set tier: %+v", k)
	}
	must(c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts", QuotaKey: "u1"}))(t)
	_, err = c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts", QuotaKey: "u1"})
	httpErr(err, 429, "quota_exceeded")
	// A key's own override beats its tier.
	must(c.SetQuota(ctx, "u1", "artifacts", api.Quota{MaxMemoryMB: -1}))(t)
	must(c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts", QuotaKey: "u1"}))(t)
	// The default pool is a's.
	if d := must(c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base"}))(t); d.Host != "a" || d.Pool != "chat" {
		t.Fatalf("default pool: %+v", d)
	}
	_, err = c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "gpu"})
	httpErr(err, 503, "no_host")

	// When b goes away, its sandboxes are listed as such and calls fail fast.
	b.close("")
	bUp = false
	got := must(c.GetSandbox(ctx, sb.ID))(t)
	if !got.HostDown || got.Host != "b" {
		t.Fatalf("record of a down host's sandbox: %+v", got)
	}
	_, err = c.Exec(ctx, sb.ID, api.ExecRequest{Cmd: "true"})
	httpErr(err, 503, "host_unavailable")
	for _, s := range must(c.ListSandboxes(ctx, nil, ""))(t) {
		if s.Host == "b" && !s.HostDown {
			t.Fatalf("b's sandbox not marked: %+v", s)
		}
	}
	_, err = c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base", Pool: "artifacts"})
	httpErr(err, 503, "no_host")
	// u1's running memory on b no longer counts.
	if q := must(c.Quota(ctx, "u1", "artifacts"))(t); q.RunningMB != 0 {
		t.Fatalf("down host still counted: %+v", q)
	}
}
