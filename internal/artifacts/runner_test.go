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
	"slices"
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

// genScript is the test job: the trigger payload's "mode" picks what it
// does. It keeps a count in _private/state and publishes stats/result.
const genScript = `#!/bin/sh
set -e
trig="$PWD/../tmp/fired-artifacts/trigger.json"
mode=$(sed -n 's/.*"mode":"\([a-z]*\)".*/\1/p' "$trig")
db() { curl -sf -H "Authorization: Bearer $FIRED_DB_TOKEN" -H 'Content-Type: application/json' "$@"; }
case "$mode" in
fail) echo boom >&2; exit 3 ;;
slow) sleep 30 ;;
same) exit 0 ;;
leak) echo "$API_TOKEN"; db -X PUT -d "{\"v\":\"$API_TOKEN\"}" "$FIRED_DB_URL/leak/x"; exit 0 ;;
esac
echo edited > README.md
n=$(db "$FIRED_DB_URL/_private/state" 2>/dev/null | sed -n 's/.*"n": *\([0-9]*\).*/\1/p')
n=$((${n:-0}+1))
db -X PUT -d "{\"n\":$n}" "$FIRED_DB_URL/_private/state" >/dev/null
token=missing
case "$API_TOKEN" in s3cret-$ARTIFACT_RUN_ID) token=ok ;; esac
db -X PUT -d "{\"run\":$n,\"token\":\"$token\"}" "$FIRED_DB_URL/stats/result" >/dev/null
db -X PUT -d @"$trig" "$FIRED_DB_URL/runs/last" >/dev/null
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
	// Jobs here are host processes (the fake driver): they reach the
	// service directly, at an address db_url doesn't accept.
	h.svc.cfg.Runner.DBURL = h.srv.URL
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
		Files:  []pa.FileOp{{Path: "gen.sh", Content: &script, Mode: 0o755}, pa.Text("README.md", "source")},
		Spec:   &pa.Spec{Job: &pa.Job{Run: "./gen.sh", Timeout: pa.Duration(2 * time.Second)}},
		RunNow: true, Notify: "session-1",
	}}))(t)
	id := res.Artifact.ID
	trigger := func(mode string) *pa.Run {
		t.Helper()
		return waitRun(t, c, id, must(c.Trigger(ctx, id, json.RawMessage(`{"mode":"`+mode+`"}`)))(t).ID)
	}
	result := func() string {
		t.Helper()
		return string(must(c.GetDoc(ctx, id, "stats/result"))(t).Data)
	}

	runs := must(c.Runs(ctx, id, ""))(t)
	if len(runs) != 1 {
		t.Fatalf("run_now: %+v", runs)
	}
	r := waitRun(t, c, id, runs[0].ID)
	if r.Status != pa.RunSucceeded || r.Version != 1 || r.DBWrites != 3 || r.ExitCode == nil || *r.ExitCode != 0 || r.Trigger != pa.TriggerPublish {
		logs, _ := c.RunLogs(ctx, id, r.ID)
		t.Fatalf("first run: %+v logs: %+v", r, logs)
	}
	// A run writes the db, never the artifact.
	if a := must(c.Get(ctx, id))(t); a.CurrentVersion != 1 {
		t.Fatalf("after a run: version %d", a.CurrentVersion)
	}
	if b := must(c.ReadFile(ctx, id, "README.md", 0))(t); string(b) != "source" {
		t.Fatalf("a run changed the source: %q", b)
	}
	if got := result(); got != `{"run":1,"token":"ok"}` {
		t.Fatalf("result: %s", got)
	}
	var trig map[string]any
	_ = json.Unmarshal(must(c.GetDoc(ctx, id, "runs/last"))(t).Data, &trig)
	if trig["type"] != pa.TriggerPublish || trig["run_id"] != r.ID {
		t.Fatalf("trigger file: %+v", trig)
	}
	logs := must(c.RunLogs(ctx, id, r.ID))(t)
	if !strings.Contains(logs.Stdout, "hello from job") || !strings.Contains(logs.Stderr, "a warning") {
		t.Fatalf("logs: %+v", logs)
	}
	bs.mu.Lock()
	if len(bs.reqs) != 1 || bs.reqs[0].Artifact != id || bs.reqs[0].QuotaKey != "u1" {
		t.Fatalf("broker requests: %+v", bs.reqs)
	}
	bs.mu.Unlock()

	t.Run("each run reads what the last one kept", func(t *testing.T) {
		r := trigger("gen")
		if r.Status != pa.RunSucceeded || r.DBWrites != 3 {
			t.Fatalf("second run: %+v", r)
		}
		if got := result(); got != `{"run":2,"token":"ok"}` {
			t.Fatalf("result: %s", got)
		}
	})

	t.Run("no writes", func(t *testing.T) {
		if r := trigger("same"); r.Status != pa.RunSucceeded || r.DBWrites != 0 {
			t.Fatalf("run without writes: %+v", r)
		}
	})

	t.Run("edits don't conflict with runs", func(t *testing.T) {
		p := must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{pa.Text("README.md", "edited")}}))(t)
		if p.Version.Version != 2 || p.Job == nil {
			t.Fatalf("publish: %+v", p)
		}
	})

	t.Run("failure", func(t *testing.T) {
		r := trigger("fail")
		if r.Status != pa.RunFailed || r.Reason != "exit status 3" {
			t.Fatalf("failed run: %+v", r)
		}
		if l := must(c.RunLogs(ctx, id, r.ID))(t); !strings.Contains(l.Stderr, "boom") {
			t.Fatalf("logs: %+v", l)
		}
	})

	t.Run("a job's token opens its artifact's db while it runs", func(t *testing.T) {
		other := must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a.txt", "a")}}}))(t).Artifact.ID
		run := must(c.Trigger(ctx, id, json.RawMessage(`{"mode":"slow"}`)))(t)
		jc := pa.NewClient(h.srv.URL, h.svc.runToken(run.ID))
		deadline := time.Now().Add(20 * time.Second)
		for {
			if r := must(c.Run(ctx, id, run.ID))(t); r.Status == pa.RunRunning {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the slow run didn't start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if d := must(jc.GetDoc(ctx, id, "_private/state"))(t); string(d.Data) != `{"n":2}` {
			t.Fatalf("the job reads _private/: %s", d.Data)
		}
		_, err := jc.Get(ctx, id)
		wantCode(t, err, 401, "unauthorized")
		_, err = jc.GetDoc(ctx, other, "a/b")
		wantCode(t, err, 401, "unauthorized")
		r := waitRun(t, c, id, run.ID)
		if r.Status != pa.RunTimedOut {
			t.Fatalf("slow run: %+v", r)
		}
		_, err = jc.GetDoc(ctx, id, "stats/result")
		wantCode(t, err, 401, "unauthorized")
	})

	t.Run("credential values are never written", func(t *testing.T) {
		r := trigger("leak")
		if r.Status != pa.RunFailed || r.Reason != "exit status 22" {
			t.Fatalf("leaking run: %+v", r)
		}
		_, err := c.GetDoc(ctx, id, "leak/x")
		wantCode(t, err, 404, "no_document")
		if l := must(c.RunLogs(ctx, id, r.ID))(t); strings.Contains(l.Stdout, "s3cret") {
			t.Fatalf("logs contain the secret: %+v", l)
		}
	})

	t.Run("credentials refused", func(t *testing.T) {
		bs.mu.Lock()
		bs.fail = true
		bs.mu.Unlock()
		defer func() { bs.mu.Lock(); bs.fail = false; bs.mu.Unlock() }()
		if r := trigger("gen"); r.Status != pa.RunFailed || !strings.Contains(r.Reason, "owner_gone") {
			t.Fatalf("refused run: %+v", r)
		}
	})

	t.Run("a new allowlist makes a new runner", func(t *testing.T) {
		before := must(fc.ListSandboxes(ctx, map[string]string{labelArtifact: id}, ""))(t)
		h.svc.cfg.Runner.Egress = []string{"example.com"}
		defer func() { h.svc.cfg.Runner.Egress = nil }()
		if r := trigger("gen"); r.Status != pa.RunSucceeded {
			t.Fatalf("run: %+v", r)
		}
		after := must(fc.ListSandboxes(ctx, map[string]string{labelArtifact: id}, ""))(t)
		if len(before) != 1 || len(after) != 1 || after[0].ID == before[0].ID || !slices.Contains(after[0].Egress.Allow, "example.com") {
			t.Fatalf("runner before %+v, after %+v", before, after)
		}
	})

	t.Run("a fork copies the db but not _private/", func(t *testing.T) {
		f := must(c.Fork(ctx, id, pa.ForkRequest{QuotaKey: "u2"}))(t)
		if d := must(c.GetDoc(ctx, f.ID, "stats/result"))(t); string(d.Data) != `{"run":3,"token":"ok"}` || d.Rev != 1 {
			t.Fatalf("fork: %+v", d)
		}
		_, err := c.GetDoc(ctx, f.ID, "_private/state")
		wantCode(t, err, 404, "no_document")
	})

	t.Run("events", func(t *testing.T) {
		evMu.Lock()
		defer evMu.Unlock()
		var notified, started, data bool
		for _, e := range evs {
			if e.Type == "run.finished" && e.Notify == "session-1" && e.Status == pa.RunSucceeded {
				notified = true
			}
			if e.Type == "run.finished" && e.Notify != "" && e.Version != 1 {
				t.Errorf("notify on a later run: %+v", e)
			}
			started = started || e.Type == "run.started"
			data = data || (e.Type == "db.changed" && e.Artifact == id && len(e.Paths) == 1)
		}
		if !notified || !started || !data {
			t.Fatalf("events (notified %v started %v data %v): %+v", notified, started, data, evs)
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
	if got := tree(v); got != "README.md:644,artifact.json:644,site/index.html:644,site/run.sh:755,site/sub/a.css:644" {
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
	if got := tree(v); got != "README.md:644,artifact.json:644,site/index.html:644,site/run.sh:755" {
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
	// Excluding it works; "" mirrors the whole tree, so README.md goes
	// (artifact.json is kept: every version has one).
	r3 := must(c.Publish(ctx, "built", pa.PublishRequest{BaseVersion: 2, Files: []pa.FileOp{
		from("", pa.SandboxSource{Path: "dist", Exclude: []string{".env", "node_modules"}})}}))(t)
	v = must(c.Version(ctx, "built", r3.Version.Version))(t)
	if got := tree(v); got != "artifact.json:644,index.html:644,run.sh:755" {
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

// TestRunQuota: runner sandboxes are charged to the artifact's quota key in
// the runner pool, so fired's per-user limits apply to runs. A run fired
// refuses waits (pending) and that key backs off, instead of failing.
func TestRunQuota(t *testing.T) {
	if testing.Short() {
		t.Skip("starts fired")
	}
	fc, firedURL := startFired(t)
	h := newHarness(t, func(c *Config) {
		c.Fired = FiredConfig{URL: firedURL, Token: firedToken}
		c.SchedulerInterval = pa.Duration(100 * time.Millisecond)
		c.Runner.QuotaBackoff = pa.Duration(300 * time.Millisecond)
	})
	h.svc.now = func() time.Time { return time.Now().UTC() }
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := h.c
	spec := &pa.Spec{Job: &pa.Job{Run: "echo ran"}}
	must(c.Create(ctx, pa.CreateArtifactRequest{ID: "quota-art", QuotaKey: "u9", Publish: &pa.PublishRequest{
		Files: []pa.FileOp{pa.Text("a", "x")}, Spec: spec}}))(t)

	// u9 may not run anything in the artifacts pool.
	must(fc.SetQuota(ctx, "u9", "artifacts", api.Quota{MaxMemoryMB: 1}))(t)
	run := must(c.Trigger(ctx, "quota-art", nil))(t)
	time.Sleep(time.Second)
	if r := must(c.Run(ctx, "quota-art", run.ID))(t); r.Status != pa.RunPending {
		t.Fatalf("over quota, the run should wait: %+v", r)
	}
	runners := must(fc.ListSandboxes(ctx, map[string]string{labelArtifact: "quota-art"}, ""))(t)
	for _, sb := range runners {
		if sb.QuotaKey != "u9" || sb.Pool != "artifacts" {
			t.Fatalf("runner not charged to the artifact's key: %+v", sb)
		}
	}
	// Lifting the limit lets it run.
	must(fc.SetQuota(ctx, "u9", "artifacts", api.Quota{MaxMemoryMB: -1}))(t)
	if r := waitRun(t, c, "quota-art", run.ID); r.Status != pa.RunSucceeded {
		t.Fatalf("run: %+v", r)
	}
	q := must(fc.Quota(ctx, "u9", "artifacts"))(t)
	if q.RunningMB == 0 {
		t.Fatalf("the running runner should count for u9: %+v", q)
	}
}

// TestTierFromFired: a key's tier is fired's one mapping; what the tier
// means for storage is defined here.
func TestTierFromFired(t *testing.T) {
	fc, firedURL := startFired(t)
	h := newHarness(t, func(c *Config) { c.Fired = FiredConfig{URL: firedURL, Token: firedToken} })
	ctx := context.Background()
	c := h.c
	must(c.PutTier(ctx, pa.Tier{Name: pa.DefaultTier, Limits: pa.Quota{StorageBytes: 1000}}))(t)
	must(c.PutTier(ctx, pa.Tier{Name: "pro", Limits: pa.Quota{StorageBytes: 100}}))(t)
	must(fc.PutTier(ctx, api.Tier{Name: "pro"}))(t)
	must(fc.SetTier(ctx, "u-pro", "pro"))(t)

	st := must(c.Quota(ctx, "u-pro"))(t)
	if st.Tier != "pro" || st.Effective.StorageBytes != 100 {
		t.Fatalf("pro key: %+v", st)
	}
	if st := must(c.Quota(ctx, "u-free"))(t); st.Tier != pa.DefaultTier || st.Effective.StorageBytes != 1000 {
		t.Fatalf("unmapped key: %+v", st)
	}
	big := strings.Repeat("x", 500)
	_, err := c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u-pro", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", big)}}})
	wantCode(t, err, 413, "quota_exceeded")
	must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u-free", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", big)}}}))(t)
}
