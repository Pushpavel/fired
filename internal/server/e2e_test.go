package server_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/internal/events"
	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/internal/server"
	"github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/internal/store/storetest"
	"github.com/Pushpavel/fired/internal/vmm/fake"
	"github.com/Pushpavel/fired/pkg/api"
	"github.com/Pushpavel/fired/pkg/client"
)

const token = "test-token"

var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fired-agent-bin")
	if err != nil {
		panic(err)
	}
	agentBin = filepath.Join(dir, "fired-agent")
	out, err := exec.Command("go", "build", "-o", agentBin, "../../cmd/fired-agent").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build agent: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// dbs keeps one database per data dir, so a restarted daemon (same data
// dir) finds its state again.
var dbs sync.Map

func dbFor(t *testing.T, data string) string {
	if u, ok := dbs.Load(data); ok {
		return u.(string)
	}
	u := storetest.URL(t)
	dbs.Store(data, u)
	return u
}

type env struct {
	t    *testing.T
	cfg  config.Config
	st   *store.Store
	mgr  *sandbox.Manager
	srv  *httptest.Server
	c    *client.Client
	data string
}

func newEnv(t *testing.T, data string, mutate func(*config.Config)) *env {
	t.Helper()
	cfg := config.Default()
	cfg.Driver = "fake"
	cfg.Fake.AgentBinary = agentBin
	cfg.Data = config.DataConfig{
		HotDir: filepath.Join(data, "hot"), ColdDir: filepath.Join(data, "cold"), TemplatesDir: filepath.Join(data, "tpl"),
	}
	cfg.DatabaseURL = dbFor(t, data)
	cfg.Cluster.HostID = "test-host"
	cfg.Auth.Token = token
	cfg.Capacity.MaxRunningMemoryMB = 8192
	cfg.Defaults.MemoryMB = 512
	cfg.Defaults.DiskMB = 1024
	cfg.Egress.AllowPrivateCIDRs = []string{"127.0.0.0/8"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Cluster.AdvertiseURL = "http://" + ln.Addr().String()
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.Data.HotDir, 0o755)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	st, err := store.Open(context.Background(), cfg.DatabaseURL, cfg.Cluster.HostID)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := fake.New(agentBin, cfg.Data.HotDir, log)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := sandbox.New(sandbox.Options{Config: cfg, Store: st, Driver: drv, Bus: events.NewBus(), Secrets: redact.NewRegistry(), Log: log, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.StartCluster()
	h := server.New(mgr, token, log)
	if cfg.UI {
		h.EnableUI(cfg.PreviewDomain)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	e := &env{t: t, cfg: cfg, st: st, mgr: mgr, srv: srv, c: client.New(srv.URL, token), data: data}
	return e
}

func (e *env) close(action string) {
	e.srv.Close()
	e.mgr.Shutdown(context.Background())
	_ = e.st.MarkDown(context.Background())
	e.st.Close()
}

func (e *env) template(name string, snapshot bool) *api.Template {
	e.t.Helper()
	ctx := context.Background()
	t, err := e.c.CreateTemplate(ctx, api.CreateTemplateRequest{Name: name, Snapshot: &snapshot,
		Env: map[string]string{"TEMPLATE_VAR": "from-template"}, Run: runIf(snapshot)})
	if err != nil {
		e.t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		t, err = e.c.GetTemplate(ctx, t.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		if t.Status == api.TemplateReady {
			return t
		}
		if t.Status == api.TemplateFailed {
			e.t.Fatalf("template build failed: %s", t.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatal("template build timed out")
	return nil
}

func runIf(snapshot bool) []string {
	if snapshot {
		return []string{"echo baked > baked.txt"}
	}
	return nil
}

func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, t.TempDir(), nil)
	defer e.close("")
	resp, err := http.Get(e.srv.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp, err = http.Get(e.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t, t.TempDir(), nil)
	defer e.close("")
	ctx := context.Background()
	c := e.c
	tpl := e.template("base", true)
	if !tpl.HasSnapshot {
		t.Fatal("expected template snapshot")
	}

	sb := must(c.CreateSandbox(ctx, api.CreateSandboxRequest{
		Template: "base", Env: map[string]string{"FOO": "bar"}, Labels: map[string]string{"user": "42"},
	}))(t)
	if sb.State != api.StateRunning {
		t.Fatalf("state %s", sb.State)
	}
	id := sb.ID

	t.Run("exec", func(t *testing.T) {
		r := must(c.Exec(ctx, id, api.ExecRequest{Cmd: "echo hello $FOO $TEMPLATE_VAR; echo oops >&2; exit 3"}))(t)
		if r.ExitCode != 3 || strings.TrimSpace(r.Stdout) != "hello bar from-template" || strings.TrimSpace(r.Stderr) != "oops" {
			t.Fatalf("unexpected result %+v", r)
		}
		r = must(c.Exec(ctx, id, api.ExecRequest{Args: []string{"printf", "%s-%s", "a b", "c"}}))(t)
		if r.Stdout != "a b-c" {
			t.Fatalf("args exec: %q", r.Stdout)
		}
		r = must(c.Exec(ctx, id, api.ExecRequest{Cmd: "cat baked.txt"}))(t)
		if strings.TrimSpace(r.Stdout) != "baked" {
			t.Fatalf("template run output missing: %+v", r)
		}
		r = must(c.Exec(ctx, id, api.ExecRequest{Cmd: "sleep 5", TimeoutS: 1}))(t)
		if !r.TimedOut {
			t.Fatalf("expected timeout: %+v", r)
		}
		r = must(c.Exec(ctx, id, api.ExecRequest{Cmd: "cat", Stdin: "from-stdin"}))(t)
		if r.Stdout != "from-stdin" {
			t.Fatalf("stdin: %q", r.Stdout)
		}
	})

	t.Run("secrets", func(t *testing.T) {
		r := must(c.Exec(ctx, id, api.ExecRequest{
			Cmd:         `echo "tok=$GH_TOKEN"; cat "$SECRET_DIR/creds"; echo "cfg=$P4TICKETS"`,
			SecretEnv:   map[string]string{"GH_TOKEN": "ghs_supersecret123", "P4TICKETS": "$SECRET_DIR/creds"},
			SecretFiles: map[string]string{"creds": "file-secret-456"},
		}))(t)
		if strings.Contains(r.Stdout, "ghs_supersecret123") || strings.Contains(r.Stdout, "file-secret-456") {
			t.Fatalf("secret leaked: %q", r.Stdout)
		}
		if !strings.Contains(r.Stdout, "tok=[REDACTED:GH_TOKEN]") || !strings.Contains(r.Stdout, "[REDACTED:creds]") {
			t.Fatalf("expected redaction markers: %q", r.Stdout)
		}
		if !strings.Contains(r.Stdout, "/run/fired/secrets/") || strings.Contains(r.Stdout, "$SECRET_DIR") {
			t.Fatalf("SECRET_DIR not expanded: %q", r.Stdout)
		}
		// Secrets registered earlier are redacted from later output too.
		r = must(c.Exec(ctx, id, api.ExecRequest{Cmd: "echo ghs_supersecret123 | base64"}))(t)
		if strings.Contains(r.Stdout, "Z2hzX3N1cGVyc2VjcmV0MTIz") {
			t.Fatalf("base64 secret leaked: %q", r.Stdout)
		}
	})

	t.Run("files", func(t *testing.T) {
		fi := must(c.WriteFile(ctx, id, "dir/hello.txt", strings.NewReader("hi there"), 0o600))(t)
		if fi.Mode != "0600" || fi.Size != 8 {
			t.Fatalf("write: %+v", fi)
		}
		b := must(c.ReadFile(ctx, id, "dir/hello.txt"))(t)
		if string(b) != "hi there" {
			t.Fatalf("read: %q", b)
		}
		ls := must(c.ListDir(ctx, id, "dir"))(t)
		if len(ls.Entries) != 1 || ls.Entries[0].Name != "hello.txt" || ls.Entries[0].Path != "/workspace/dir/hello.txt" {
			t.Fatalf("list: %+v", ls)
		}
		if err := c.Move(ctx, id, "dir/hello.txt", "dir2/moved.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ReadFile(ctx, id, "dir/hello.txt"); !client.IsNotFound(err) {
			t.Fatalf("expected not found after move, got %v", err)
		}
		r := must(c.Exec(ctx, id, api.ExecRequest{Cmd: "cat dir2/moved.txt"}))(t)
		if r.Stdout != "hi there" {
			t.Fatalf("exec sees file: %q", r.Stdout)
		}
		// tar.gz round trip
		rc := must(c.DownloadArchive(ctx, id, "dir2"))(t)
		tgz, _ := io.ReadAll(rc)
		rc.Close()
		names := tarNames(t, tgz)
		if !contains(names, "moved.txt") {
			t.Fatalf("archive entries: %v", names)
		}
		if err := c.UploadArchive(ctx, id, "restored", bytes.NewReader(tgz)); err != nil {
			t.Fatal(err)
		}
		b = must(c.ReadFile(ctx, id, "restored/moved.txt"))(t)
		if string(b) != "hi there" {
			t.Fatalf("restored: %q", b)
		}
		if err := c.DeleteFile(ctx, id, "restored", true); err != nil {
			t.Fatal(err)
		}
		// manifest: hashes without downloading
		must(c.WriteFile(ctx, id, "dir2/skip/x.txt", strings.NewReader("x"), 0o644))(t)
		m := must(c.Manifest(ctx, id, "dir2", "skip"))(t)
		if len(m.Files) != 1 || m.Files[0].Path != "moved.txt" || m.Files[0].Size != 8 ||
			m.Files[0].SHA256 != "9b96a1fe1d548cbbc960cc6a0286668fd74a763667b06366fb2324269fcabaa4" {
			t.Fatalf("manifest: %+v", m)
		}
		m = must(c.Manifest(ctx, id, "dir2/moved.txt"))(t)
		if len(m.Files) != 1 || m.Files[0].Path != "moved.txt" {
			t.Fatalf("single-file manifest: %+v", m)
		}
		// scan: the same manifest, checked for this sandbox's secrets
		// (GH_TOKEN and creds were registered by the secrets subtest).
		must(c.WriteFile(ctx, id, "pub/ok.txt", strings.NewReader("nothing to see"), 0o755))(t)
		must(c.WriteFile(ctx, id, "pub/sub/leak.env", strings.NewReader("TOKEN=Z2hzX3N1cGVyc2VjcmV0MTIz\n"), 0o644))(t)
		must(c.WriteFile(ctx, id, "pub/skip/creds.txt", strings.NewReader("file-secret-456"), 0o644))(t)
		sc := must(c.ScanFiles(ctx, id, "pub", "skip"))(t)
		if sc.SecretsChecked < 2 || len(sc.Files) != 2 || !strings.HasSuffix(sc.Base, "/pub") || sc.Base != sc.Path {
			t.Fatalf("scan: %+v", sc)
		}
		if len(sc.Found) != 1 || sc.Found[0].Path != "sub/leak.env" || sc.Found[0].Secret != "GH_TOKEN" {
			t.Fatalf("scan found (base64 of GH_TOKEN in sub/leak.env): %+v", sc.Found)
		}
		for _, f := range sc.Files {
			if f.Path == "ok.txt" && f.Mode != "0755" {
				t.Fatalf("scan entry: %+v", f)
			}
		}
		m = must(c.Manifest(ctx, id, "pub", "skip"))(t)
		for i, f := range m.Files {
			if g := sc.Files[i]; g.Path != f.Path || g.SHA256 != f.SHA256 || g.Size != f.Size {
				t.Fatalf("scan and manifest disagree: %+v vs %+v", g, f)
			}
		}
		one := must(c.ScanFiles(ctx, id, "pub/ok.txt"))(t)
		if len(one.Files) != 1 || one.Files[0].Path != "ok.txt" || one.Base == one.Path {
			t.Fatalf("single-file scan: %+v", one)
		}
		if _, err := c.ScanFiles(ctx, id, "pub/missing"); !client.IsNotFound(err) {
			t.Fatalf("scan of a missing path: %v", err)
		}
	})

	t.Run("background", func(t *testing.T) {
		r := must(c.Exec(ctx, id, api.ExecRequest{Cmd: "for i in 1 2 3; do echo line$i; sleep 0.1; done; echo done >&2", Background: true}))(t)
		if r.PID == 0 {
			t.Fatal("no pid")
		}
		var out strings.Builder
		var so, se int64
		for i := 0; i < 50; i++ {
			o := must(c.ProcessOutput(ctx, id, r.PID, so, se, 500))(t)
			out.WriteString(o.Stdout)
			so, se = o.StdoutOffset, o.StderrOffset
			if !o.Running && o.ExitCode != nil {
				break
			}
		}
		if out.String() != "line1\nline2\nline3\n" {
			t.Fatalf("output: %q", out.String())
		}
		ps := must(c.Processes(ctx, id))(t)
		found := false
		for _, p := range ps {
			if p.PID == r.PID && !p.Running && p.ExitCode != nil && *p.ExitCode == 0 {
				found = true
			}
		}
		if !found {
			t.Fatalf("process not listed as finished: %+v", ps)
		}
	})

	t.Run("ports", func(t *testing.T) {
		port := freePort(t)
		must(c.WriteFile(ctx, id, "site/index.html", strings.NewReader("<h1>preview</h1>"), 0))(t)
		r := must(c.Exec(ctx, id, api.ExecRequest{Cmd: fmt.Sprintf("cd site && exec python3 -m http.server %d --bind 127.0.0.1", port), Background: true}))(t)
		defer c.Kill(ctx, id, r.PID, "KILL")
		url := c.ProxyURL(id, port, "/index.html?x=1")
		// Not exposed yet.
		if code, _ := get(t, url, nil); code != 403 {
			t.Fatalf("want 403 before expose, got %d", code)
		}
		if err := c.ExposePort(ctx, id, port); err != nil {
			t.Fatal(err)
		}
		var body string
		var code int
		for i := 0; i < 50; i++ {
			code, body = get(t, url, map[string]string{"X-Forwarded-Host": "p3000-abc.preview.example.com"})
			if code == 200 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if code != 200 || body != "<h1>preview</h1>" {
			t.Fatalf("proxy: %d %q", code, body)
		}
		// Unlistened port → 502 port_not_listening.
		other := freePort(t)
		_ = c.ExposePort(ctx, id, other)
		if code, body := get(t, c.ProxyURL(id, other, "/"), nil); code != 502 || !strings.Contains(body, "port_not_listening") {
			t.Fatalf("want 502 port_not_listening, got %d %s", code, body)
		}
	})

	t.Run("egress", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "upstream-ok") }))
		defer up.Close()
		upHost := strings.TrimPrefix(up.URL, "http://")
		curl := `curl -s --noproxy '' -x "$HTTP_PROXY" -o - -w ' %{http_code}' ` + up.URL + "/"
		r := must(c.Exec(ctx, id, api.ExecRequest{Cmd: curl}))(t)
		if !strings.HasSuffix(strings.TrimSpace(r.Stdout), "403") {
			t.Fatalf("expected egress denied, got %+v", r)
		}
		must(c.UpdateSandbox(ctx, id, api.UpdateSandboxRequest{Egress: &api.EgressPolicy{Allow: []string{upHost}}}))(t)
		r = must(c.Exec(ctx, id, api.ExecRequest{Cmd: curl}))(t)
		if strings.TrimSpace(r.Stdout) != "upstream-ok 200" {
			t.Fatalf("expected egress allowed, got %+v", r)
		}
	})

	t.Run("stream", func(t *testing.T) {
		s := must(c.ExecStream(ctx, id, api.ExecRequest{Cmd: "cat; echo end $GH_TOKEN", SecretEnv: map[string]string{"GH_TOKEN": "ghs_supersecret123"}}))(t)
		defer s.Close()
		if err := s.Stdin([]byte("ping\n")); err != nil {
			t.Fatal(err)
		}
		if err := s.Control(api.StreamControl{Type: "eof"}); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		exit := -1
		for exit < 0 {
			ch, data, ev, err := s.Recv()
			if err != nil {
				t.Fatalf("recv: %v (out=%q)", err, out.String())
			}
			if ch == api.StreamStdout {
				out.Write(data)
			}
			if ev != nil && ev.Type == "exit" {
				exit = ev.ExitCode
			}
			if ev != nil && ev.Type == "error" {
				t.Fatal(ev.Message)
			}
		}
		if out.String() != "ping\nend [REDACTED:GH_TOKEN]\n" || exit != 0 {
			t.Fatalf("stream out %q exit %d", out.String(), exit)
		}
		// PTY
		p := must(c.ExecStream(ctx, id, api.ExecRequest{Cmd: "tty >/dev/null && echo is-a-tty", PTY: true}))(t)
		defer p.Close()
		out.Reset()
		for {
			ch, data, ev, err := p.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if ch == api.StreamStdout {
				out.Write(data)
			}
			if ev != nil && ev.Type == "exit" {
				break
			}
		}
		if !strings.Contains(out.String(), "is-a-tty") {
			t.Fatalf("pty output %q", out.String())
		}
	})

	t.Run("lifecycle", func(t *testing.T) {
		must(c.WriteFile(ctx, id, "persist.txt", strings.NewReader("still here"), 0))(t)
		s := must(c.Suspend(ctx, id))(t)
		if s.State != api.StateSuspended {
			t.Fatalf("suspend: %s", s.State)
		}
		// Any use resumes it.
		b := must(c.ReadFile(ctx, id, "persist.txt"))(t)
		if string(b) != "still here" {
			t.Fatalf("after resume: %q", b)
		}
		if s := must(c.GetSandbox(ctx, id))(t); s.State != api.StateRunning {
			t.Fatalf("expected running, got %s", s.State)
		}
		// Proxy with no-wake on a suspended sandbox returns 503.
		must(c.Suspend(ctx, id))(t)
		if code, _ := get(t, c.ProxyURL(id, 1234, "/"), map[string]string{"X-Fired-No-Wake": "1"}); code != 403 {
			// port 1234 isn't exposed → 403 takes precedence
			t.Fatalf("want 403, got %d", code)
		}
		if err := c.ExposePort(ctx, id, 1234); err != nil {
			t.Fatal(err)
		}
		if code, body := get(t, c.ProxyURL(id, 1234, "/"), map[string]string{"X-Fired-No-Wake": "1"}); code != 503 || !strings.Contains(body, "not_running") {
			t.Fatalf("want 503 not_running, got %d %s", code, body)
		}
		a := must(c.Archive(ctx, id))(t)
		if a.State != api.StateArchived {
			t.Fatalf("archive: %s", a.State)
		}
		if _, err := os.Stat(filepath.Join(e.cfg.Data.ColdDir, id+".fired.zst")); err != nil {
			t.Fatalf("archive file: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.cfg.Data.HotDir, "vms", id)); !os.IsNotExist(err) {
			t.Fatalf("hot workdir should be gone: %v", err)
		}
		r := must(c.Exec(ctx, id, api.ExecRequest{Cmd: "cat persist.txt"}))(t)
		if r.Stdout != "still here" {
			t.Fatalf("after unarchive: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(e.cfg.Data.ColdDir, id+".fired.zst")); !os.IsNotExist(err) {
			t.Fatalf("stale archive should be removed after resume")
		}
	})

	t.Run("reaper", func(t *testing.T) {
		ttl := api.TTL{IdleSuspend: api.Duration(300 * time.Millisecond), ArchiveAfter: api.Duration(300 * time.Millisecond)}
		must(c.UpdateSandbox(ctx, id, api.UpdateSandboxRequest{TTL: &ttl}))(t)
		time.Sleep(400 * time.Millisecond)
		e.mgr.ReapOnce(ctx)
		if s := must(c.GetSandbox(ctx, id))(t); s.State != api.StateSuspended {
			t.Fatalf("expected idle suspend, got %s", s.State)
		}
		time.Sleep(400 * time.Millisecond)
		e.mgr.ReapOnce(ctx)
		if s := must(c.GetSandbox(ctx, id))(t); s.State != api.StateArchived {
			t.Fatalf("expected archive, got %s", s.State)
		}
		ttl = api.TTL{IdleSuspend: -1, ArchiveAfter: -1}
		must(c.UpdateSandbox(ctx, id, api.UpdateSandboxRequest{TTL: &ttl}))(t)
	})

	t.Run("list-and-delete", func(t *testing.T) {
		l := must(c.ListSandboxes(ctx, map[string]string{"user": "42"}, ""))(t)
		if len(l) != 1 || l[0].ID != id {
			t.Fatalf("list: %+v", l)
		}
		if l := must(c.ListSandboxes(ctx, map[string]string{"user": "7"}, ""))(t); len(l) != 0 {
			t.Fatalf("label filter: %+v", l)
		}
		if err := c.DeleteTemplate(ctx, "base"); err == nil {
			t.Fatal("template in use should not be deletable")
		}
		if err := c.DeleteSandbox(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetSandbox(ctx, id); !client.IsNotFound(err) {
			t.Fatalf("expected 404 after delete, got %v", err)
		}
		if err := c.DeleteTemplate(ctx, "base"); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRestart checks that state survives a daemon restart (container upgrade).
func TestRestart(t *testing.T) {
	data := t.TempDir()
	e := newEnv(t, data, nil)
	ctx := context.Background()
	e.template("t1", false)
	sb := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "t1", ID: "keepme"}))(t)
	must(e.c.WriteFile(ctx, sb.ID, "x.txt", strings.NewReader("survives"), 0))(t)
	e.close("suspend")

	e2 := newEnv(t, data, nil)
	defer e2.close("")
	s := must(e2.c.GetSandbox(ctx, "keepme"))(t)
	if s.State != api.StateSuspended {
		t.Fatalf("after restart: %s", s.State)
	}
	r := must(e2.c.Exec(ctx, "keepme", api.ExecRequest{Cmd: "cat x.txt"}))(t)
	if r.Stdout != "survives" {
		t.Fatalf("got %+v", r)
	}
}

// TestCapacityEviction checks that idle sandboxes are suspended to make room.
func TestCapacityEviction(t *testing.T) {
	e := newEnv(t, t.TempDir(), func(c *config.Config) { c.Capacity.MaxRunningMemoryMB = 1024 })
	defer e.close("")
	ctx := context.Background()
	e.template("small", false)
	a := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "small", MemoryMB: 512}))(t)
	b := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "small", MemoryMB: 512}))(t)
	time.Sleep(10 * time.Millisecond)
	must(e.c.Exec(ctx, b.ID, api.ExecRequest{Cmd: "true"}))(t) // b more recently used
	cc := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "small", MemoryMB: 512}))(t)
	if cc.State != api.StateRunning {
		t.Fatal("third sandbox should run")
	}
	if s := must(e.c.GetSandbox(ctx, a.ID))(t); s.State != api.StateSuspended {
		t.Fatalf("LRU sandbox should be evicted, got %s", s.State)
	}
	if s := must(e.c.GetSandbox(ctx, b.ID))(t); s.State != api.StateRunning {
		t.Fatalf("recent sandbox should keep running, got %s", s.State)
	}
	h := must(e.c.Host(ctx))(t)
	if h.MemoryRunningMB != 1024 {
		t.Fatalf("running memory %d", h.MemoryRunningMB)
	}
	_, err := e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "small", MemoryMB: 2048})
	if he, ok := err.(*client.HTTPError); !ok || he.Code() != "capacity" {
		t.Fatalf("oversized sandbox should fail with capacity, got %v", err)
	}
	if s := must(e.c.GetSandbox(ctx, b.ID))(t); s.State != api.StateRunning {
		t.Fatalf("a request that can't fit must not evict anything, got %s", s.State)
	}
	if l := must(e.c.ListSandboxes(ctx, nil, ""))(t); len(l) != 3 {
		t.Fatalf("failed create should leave no record, have %d sandboxes", len(l))
	}
}

// Suspended sandboxes' snapshots fill the hot tier: fired archives the
// least recently used ones to keep capacity.min_free_hot_mb free.
func TestHotStorageRoom(t *testing.T) {
	e := newEnv(t, t.TempDir(), func(c *config.Config) { c.Capacity.MinFreeHotMB = 1000 })
	defer e.close("")
	ctx := context.Background()
	e.template("small", false)
	var ids []string
	for i := 0; i < 3; i++ {
		sb := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "small", MemoryMB: 512}))(t)
		ids = append(ids, sb.ID)
		time.Sleep(10 * time.Millisecond)
	}
	for _, id := range ids {
		must(e.c.Suspend(ctx, id))(t)
	}
	// Each sandbox not archived takes 2000 MB of a 5000 MB hot tier.
	e.mgr.SetHotFree(func() (int64, bool) {
		free := int64(5000)
		for _, id := range ids {
			if s, err := e.mgr.Get(id); err == nil && s.State != api.StateArchived && s.State != api.StateArchiving {
				free -= 2000
			}
		}
		return free, true
	})
	e.mgr.ReapOnce(ctx)
	want := []api.SandboxState{api.StateArchived, api.StateSuspended, api.StateSuspended}
	for i, id := range ids {
		if s := must(e.c.GetSandbox(ctx, id))(t); s.State != want[i] {
			t.Fatalf("sandbox %d (oldest first): %s, want %s", i, s.State, want[i])
		}
	}
	// Starting one makes room for itself: the next oldest goes.
	must(e.c.Resume(ctx, ids[2]))(t)
	if s := must(e.c.GetSandbox(ctx, ids[1]))(t); s.State != api.StateArchived {
		t.Fatalf("to resume the newest, the next oldest should be archived: %s", s.State)
	}
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Fired-Token", token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func freePort(t *testing.T) int {
	l, err := (&net0{}).listen()
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().String()
	l.Close()
	n, _ := strconv.Atoi(p[strings.LastIndex(p, ":")+1:])
	return n
}

func tarNames(t *testing.T, tgz []byte) []string {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	return names
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
