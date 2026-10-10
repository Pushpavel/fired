package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/internal/events"
	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/internal/server"
	firedstore "github.com/Pushpavel/fired/internal/store"
	"github.com/Pushpavel/fired/internal/store/storetest"
	"github.com/Pushpavel/fired/internal/vmm/fake"
	"github.com/Pushpavel/fired/pkg/api"
	pa "github.com/Pushpavel/fired/pkg/artifacts"
	"github.com/Pushpavel/fired/pkg/client"
)

var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fired-agent-bin")
	if err != nil {
		panic(err)
	}
	agentBin = filepath.Join(dir, "fired-agent")
	if out, err := exec.Command("go", "build", "-o", agentBin, "../../cmd/fired-agent").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build agent: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const firedToken = "fired-token"

// startFired runs a real fired daemon (fake driver: the guest agent as a
// host process) with a "base" template.
func startFired(t *testing.T) (*client.Client, string) {
	t.Helper()
	data := t.TempDir()
	cfg := config.Default()
	cfg.Driver = "fake"
	cfg.Fake.AgentBinary = agentBin
	cfg.Data = config.DataConfig{HotDir: filepath.Join(data, "hot"),
		ColdDir: filepath.Join(data, "cold"), TemplatesDir: filepath.Join(data, "tpl")}
	cfg.DatabaseURL = storetest.URL(t)
	cfg.Cluster.HostID = "test-host"
	cfg.Auth.Token = firedToken
	cfg.Capacity.MaxRunningMemoryMB = 8192
	cfg.Defaults.MemoryMB = 512
	cfg.Defaults.DiskMB = 1024
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.Data.HotDir, 0o755)
	log := testLogger()
	st, err := firedstore.Open(context.Background(), cfg.DatabaseURL, cfg.Cluster.HostID)
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
	srv := httptest.NewServer(server.New(mgr, firedToken, log).Handler())
	t.Cleanup(func() {
		srv.Close()
		mgr.Shutdown(context.Background())
		st.Close()
	})
	c := client.New(srv.URL, firedToken)
	snap := false
	tpl, err := c.CreateTemplate(context.Background(), api.CreateTemplateRequest{Name: "base", Snapshot: &snap})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		tpl, err = c.GetTemplate(context.Background(), tpl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tpl.Status == api.TemplateReady {
			break
		}
		if tpl.Status == api.TemplateFailed || time.Now().After(deadline) {
			t.Fatalf("template: %+v", tpl)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return c, srv.URL
}

type brokerStub struct {
	mu   sync.Mutex
	reqs []pa.CredentialsRequest
	fail bool
}

func (b *brokerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer broker-token" {
		http.Error(w, "unauthorized", 401)
		return
	}
	var req pa.CredentialsRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	fail := b.fail
	b.mu.Unlock()
	if fail {
		writeJSON(w, 403, api.Error{Err: api.ErrorBody{Code: "owner_gone", Message: "the owner was removed"}})
		return
	}
	writeJSON(w, 200, pa.CredentialsResponse{SecretEnv: map[string]string{"API_TOKEN": "s3cret-" + req.RunID}})
}

const genScript = `#!/bin/sh
set -e
mkdir -p out .state
n=$(cat .state/count 2>/dev/null || echo 0)
n=$((n+1))
echo $n > .state/count
token=missing
case "$API_TOKEN" in s3cret-$ARTIFACT_RUN_ID) token=ok ;; esac
echo "run $n token=$token" > out/result.txt
cp "$PWD/../tmp/fired-artifacts/trigger.json" out/trigger.json
echo "hello from job"
echo "a warning" >&2
`

func waitRun(t *testing.T, c *pa.Client, id, runID string) *pa.Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		r, err := c.Run(context.Background(), id, runID)
		if err != nil {
			t.Fatal(err)
		}
		switch r.Status {
		case pa.RunPending, pa.RunRunning:
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return r
	}
	t.Fatalf("run %s did not finish", runID)
	return nil
}

func TestRunsInFired(t *testing.T) {
	if testing.Short() {
		t.Skip("starts fired")
	}
	fc, firedURL := startFired(t)
	bs := &brokerStub{}
	broker := httptest.NewServer(bs)
	defer broker.Close()
	h := newHarness(t, func(c *Config) {
		c.Fired = FiredConfig{URL: firedURL, Token: firedToken}
		c.Broker = BrokerConfig{URL: broker.URL, Token: "broker-token"}
		c.SchedulerInterval = pa.Duration(100 * time.Millisecond)
		c.Runner.Template = "base"
	})
	h.svc.now = func() time.Time { return time.Now().UTC() }
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := h.c

	var evMu sync.Mutex
	var evs []pa.Event
	go func() {
		_ = c.Events(ctx, "", func(e pa.Event) {
			evMu.Lock()
			evs = append(evs, e)
			evMu.Unlock()
		})
	}()
	time.Sleep(100 * time.Millisecond)

	script := genScript
	res := must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{
		Files: []pa.FileOp{{Path: "gen.sh", Content: &script, Mode: 0o755}, pa.Text("README.md", "source")},
		Spec: &pa.Spec{Jobs: map[string]pa.Job{
			"gen":  {Run: "./gen.sh", Writes: []string{"out"}, State: ".state"},
			"same": {Run: "true", Writes: []string{"out"}},
			"fail": {Run: "echo boom >&2; exit 3", Writes: []string{"out"}},
			"slow": {Run: "sleep 30", Timeout: pa.Duration(2 * time.Second)},
			"leak": {Run: "mkdir -p out && echo \"$API_TOKEN\" > out/leak.txt && echo \"$API_TOKEN\"", Writes: []string{"out"}},
		}},
		RunNow: []string{"gen"}, Notify: "session-1",
	}}))(t)
	id := res.Artifact.ID

	runs := must(c.Runs(ctx, id, "gen", ""))(t)
	if len(runs) != 1 {
		t.Fatalf("run_now: %+v", runs)
	}
	r := waitRun(t, c, id, runs[0].ID)
	if r.Status != pa.RunSucceeded || r.Version != 1 || r.ResultVersion != 2 || r.ExitCode == nil || *r.ExitCode != 0 {
		logs, _ := c.RunLogs(ctx, id, r.ID)
		t.Fatalf("first run: %+v logs: %+v", r, logs)
	}
	v := must(c.Version(ctx, id, 2))(t)
	if v.Origin != pa.OriginRun || v.RunID != r.ID || v.Job != "gen" {
		t.Fatalf("run version: %+v", v)
	}
	if got := strings.Join(paths(v), ","); got != "README.md,artifact.json,gen.sh,out/result.txt,out/trigger.json" {
		t.Fatalf("run version files: %s", got)
	}
	if b := must(c.ReadFile(ctx, id, "out/result.txt", 0))(t); string(b) != "run 1 token=ok\n" {
		t.Fatalf("result: %q", b)
	}
	var trig map[string]any
	_ = json.Unmarshal(must(c.ReadFile(ctx, id, "out/trigger.json", 0))(t), &trig)
	if trig["type"] != "manual" || trig["run_id"] != r.ID {
		t.Fatalf("trigger file: %+v", trig)
	}
	logs := must(c.RunLogs(ctx, id, r.ID))(t)
	if !strings.Contains(logs.Stdout, "hello from job") || !strings.Contains(logs.Stderr, "a warning") {
		t.Fatalf("logs: %+v", logs)
	}
	bs.mu.Lock()
	if len(bs.reqs) != 1 || bs.reqs[0].Artifact != id || bs.reqs[0].Job != "gen" || bs.reqs[0].QuotaKey != "u1" {
		t.Fatalf("broker requests: %+v", bs.reqs)
	}
	bs.mu.Unlock()

	t.Run("state carries over", func(t *testing.T) {
		r := waitRun(t, c, id, must(c.Trigger(ctx, id, "gen", json.RawMessage(`{"x":1}`)))(t).ID)
		if r.Status != pa.RunSucceeded || r.ResultVersion != 3 {
			t.Fatalf("second run: %+v", r)
		}
		if b := must(c.ReadFile(ctx, id, "out/result.txt", 0))(t); string(b) != "run 2 token=ok\n" {
			t.Fatalf("result: %q", b)
		}
		if _, err := c.ReadFile(ctx, id, ".state/count", 0); !client.IsNotFound(err) {
			t.Fatalf("state leaked into the files: %v", err)
		}
	})

	t.Run("no change, no version", func(t *testing.T) {
		r := waitRun(t, c, id, must(c.Trigger(ctx, id, "same", nil))(t).ID)
		if r.Status != pa.RunSucceeded || r.ResultVersion != 0 {
			t.Fatalf("unchanged run: %+v", r)
		}
		if cur := must(c.Get(ctx, id))(t).CurrentVersion; cur != 3 {
			t.Fatalf("current version %d", cur)
		}
	})

	t.Run("failure", func(t *testing.T) {
		r := waitRun(t, c, id, must(c.Trigger(ctx, id, "fail", nil))(t).ID)
		if r.Status != pa.RunFailed || r.Reason != "exit status 3" || r.ResultVersion != 0 {
			t.Fatalf("failed run: %+v", r)
		}
		if l := must(c.RunLogs(ctx, id, r.ID))(t); !strings.Contains(l.Stderr, "boom") {
			t.Fatalf("logs: %+v", l)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		r := waitRun(t, c, id, must(c.Trigger(ctx, id, "slow", nil))(t).ID)
		if r.Status != pa.RunTimedOut {
			t.Fatalf("slow run: %+v", r)
		}
	})

	t.Run("credential values are never published", func(t *testing.T) {
		r := waitRun(t, c, id, must(c.Trigger(ctx, id, "leak", nil))(t).ID)
		if r.Status != pa.RunFailed || r.Reason != "result rejected: out/leak.txt contains the value of credential API_TOKEN" {
			t.Fatalf("leaking run: %+v", r)
		}
		if _, err := c.ReadFile(ctx, id, "out/leak.txt", 0); !client.IsNotFound(err) {
			t.Fatalf("leak published: %v", err)
		}
		if l := must(c.RunLogs(ctx, id, r.ID))(t); strings.Contains(l.Stdout, "s3cret") {
			t.Fatalf("logs contain the secret: %+v", l)
		}
	})

	t.Run("credentials refused", func(t *testing.T) {
		bs.mu.Lock()
		bs.fail = true
		bs.mu.Unlock()
		defer func() { bs.mu.Lock(); bs.fail = false; bs.mu.Unlock() }()
		r := waitRun(t, c, id, must(c.Trigger(ctx, id, "gen", nil))(t).ID)
		if r.Status != pa.RunFailed || !strings.Contains(r.Reason, "owner_gone") {
			t.Fatalf("refused run: %+v", r)
		}
	})

	t.Run("events", func(t *testing.T) {
		evMu.Lock()
		defer evMu.Unlock()
		var notified, started, version bool
		for _, e := range evs {
			if e.Type == "run.finished" && e.Notify == "session-1" && e.Status == pa.RunSucceeded {
				notified = true
			}
			if e.Type == "run.finished" && e.Notify != "" && e.Version != 1 {
				t.Errorf("notify on a later run: %+v", e)
			}
			started = started || e.Type == "run.started"
			version = version || (e.Type == "artifact.version" && e.Origin == pa.OriginRun)
		}
		if !notified || !started || !version {
			t.Fatalf("events (notified %v started %v version %v): %+v", notified, started, version, evs)
		}
	})

	t.Run("delete removes the runner sandbox", func(t *testing.T) {
		if l := must(fc.ListSandboxes(ctx, map[string]string{labelArtifact: id}, ""))(t); len(l) != 1 {
			t.Fatalf("runner sandboxes: %+v", l)
		}
		if err := c.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			l := must(fc.ListSandboxes(ctx, map[string]string{labelArtifact: id}, ""))(t)
			if len(l) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("runner sandbox not removed: %+v", l)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

func TestFromSandbox(t *testing.T) {
	fc, firedURL := startFired(t)
	h := newHarness(t, func(c *Config) { c.Fired.URL = firedURL; c.Fired.Token = firedToken })
	ctx := context.Background()
	c := h.c
	sb := must(fc.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base"}))(t)
	write := func(p, content string, mode uint32) {
		t.Helper()
		must(fc.WriteFile(ctx, sb.ID, p, strings.NewReader(content), mode))(t)
	}
	write("dist/index.html", "<h1>build 1</h1>", 0o644)
	write("dist/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	write("dist/sub/a.css", "h1{}", 0o644)
	write("dist/node_modules/x.js", "big", 0o644)
	from := func(dst string, src pa.SandboxSource) pa.FileOp {
		if src.ID == "" {
			src.ID = sb.ID
		}
		return pa.FileOp{Path: dst, FromSandbox: &src}
	}
	tree := func(v *pa.Version) string {
		var out []string
		for _, f := range v.Files {
			out = append(out, fmt.Sprintf("%s:%o", f.Path, f.Mode))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	// Mirror dist/ into site/, next to an inline file.
	res := must(c.Create(ctx, pa.CreateArtifactRequest{ID: "built", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{
		pa.Text("README.md", "# built"),
		from("site", pa.SandboxSource{Path: "dist", Exclude: []string{"node_modules"}}),
	}}}))(t)
	v := must(c.Version(ctx, "built", res.Publish.Version.Version))(t)
	if got := tree(v); got != "README.md:644,site/index.html:644,site/run.sh:755,site/sub/a.css:644" {
		t.Fatalf("v1 tree: %s", got)
	}
	if got := string(must(c.ReadFile(ctx, "built", "site/index.html", 0))(t)); got != "<h1>build 1</h1>" {
		t.Fatalf("content: %q", got)
	}

	// The next build: a file removed in the sandbox disappears from site/.
	if err := fc.DeleteFile(ctx, sb.ID, "dist/sub", true); err != nil {
		t.Fatal(err)
	}
	write("dist/index.html", "<h1>build 2</h1>", 0o644)
	r2 := must(c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{
		from("site", pa.SandboxSource{FiredURL: firedURL, Path: "/workspace/dist", Exclude: []string{"node_modules"}})}}))(t)
	v = must(c.Version(ctx, "built", r2.Version.Version))(t)
	if got := tree(v); got != "README.md:644,site/index.html:644,site/run.sh:755" {
		t.Fatalf("v2 tree: %s", got)
	}

	// A file holding a secret this sandbox was given fails the publish.
	must(fc.Exec(ctx, sb.ID, api.ExecRequest{Cmd: "true", SecretEnv: map[string]string{"API_KEY": "sk-live-0123456789"}}))(t)
	write("dist/.env", "API_KEY=sk-live-0123456789\n", 0o644)
	_, err := c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 2, Files: []pa.FileOp{from("", pa.SandboxSource{Path: "dist"})}})
	wantCode(t, err, 422, "secret_found")
	if !strings.Contains(err.Error(), ".env") || !strings.Contains(err.Error(), "API_KEY") || strings.Contains(err.Error(), "sk-live") {
		t.Fatalf("secret error should name the file and secret, not the value: %v", err)
	}
	// Excluding it works; "" mirrors the whole tree, so README.md goes.
	r3 := must(c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 2, Files: []pa.FileOp{
		from("", pa.SandboxSource{Path: "dist", Exclude: []string{".env", "node_modules"}})}}))(t)
	v = must(c.Version(ctx, "built", r3.Version.Version))(t)
	if got := tree(v); got != "index.html:644,run.sh:755" {
		t.Fatalf("root mirror: %s", got)
	}

	// Bad sources.
	_, err = c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 3, Files: []pa.FileOp{from("x", pa.SandboxSource{Path: "nope"})}})
	wantCode(t, err, 404, "not_found")
	_, err = c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 3, Files: []pa.FileOp{from("x", pa.SandboxSource{ID: "sb-missing", Path: "dist"})}})
	wantCode(t, err, 404, "not_found")
	_, err = c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 3, Files: []pa.FileOp{from("x", pa.SandboxSource{FiredURL: "ftp://x", Path: "dist"})}})
	wantCode(t, err, 400, "invalid_request")

	// A file that changes between the scan and its download fails the
	// publish: the proxy edits it right after fired answers the scan.
	// (A changed file whose scanned content the store already has isn't
	// downloaded at all, so the scanned bytes are published; only new
	// content can race.)
	write("dist/new.txt", "scanned", 0o644)
	target, _ := url.Parse(firedURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if strings.HasSuffix(resp.Request.URL.Path, "/files/scan") {
			write("dist/new.txt", "changed after the scan", 0o644)
		}
		return nil
	}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	_, err = c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 3, Files: []pa.FileOp{
		from("", pa.SandboxSource{FiredURL: ps.URL, Path: "dist", Exclude: []string{".env", "node_modules"}})}})
	wantCode(t, err, 409, "changed")
	if a := must(c.Get(ctx, "built"))(t); a.CurrentVersion != 3 {
		t.Fatalf("failed publishes made versions: %d", a.CurrentVersion)
	}
}
