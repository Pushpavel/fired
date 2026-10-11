package artifacts

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
	"github.com/Pushpavel/fired/pkg/client"
)

const testToken = "artifacts-token"

type harness struct {
	t   *testing.T
	svc *Service
	srv *httptest.Server
	c   *pa.Client
	now time.Time
}

func testLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHarness starts the service without its background loops; tests drive
// the scheduler and GC directly with a controllable clock.
func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Token = testToken
	cfg.Fired.URL = "http://127.0.0.1:1" // unused unless a test starts runs
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, svc: svc, now: time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)}
	svc.now = func() time.Time { return h.now }
	h.srv = httptest.NewServer(svc.Handler())
	h.c = pa.NewClient(h.srv.URL, testToken)
	t.Cleanup(func() {
		h.srv.Close()
		svc.Close()
	})
	return h
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

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var he *client.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("want %d %s, got %v", status, code, err)
	}
	if he.Status != status || he.Code() != code {
		t.Fatalf("want %d %s, got %d %s: %s", status, code, he.Status, he.Code(), he.Body.Err.Message)
	}
}

func paths(v *pa.Version) []string {
	var out []string
	for _, f := range v.Files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := http.Get(h.srv.URL + "/v1/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestPublishSemantics(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c

	res := must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Labels: map[string]string{"session": "s1"},
		Meta: json.RawMessage(`{"title":"Report"}`),
		Publish: &pa.PublishRequest{Files: []pa.FileOp{
			pa.Text("index.html", "<h1>hi</h1>"), pa.Text("data/a.csv", "x,y\n1,2\n"),
		}, Label: "first"}}))(t)
	id := res.Artifact.ID
	if res.Artifact.CurrentVersion != 1 || res.Publish.Version.Version != 1 {
		t.Fatalf("create: %+v", res)
	}
	if v := res.Publish.Version.Spec.View; v == nil || v.Type != "html" || v.Entry != "index.html" {
		t.Fatalf("default spec: %+v", res.Publish.Version.Spec)
	}

	t.Run("serve", func(t *testing.T) {
		b := must(c.ReadFile(ctx, id, "index.html", 0))(t)
		if string(b) != "<h1>hi</h1>" {
			t.Fatalf("read: %q", b)
		}
		req, _ := http.NewRequest("GET", h.srv.URL+"/v1/artifacts/"+id+"/files/", nil)
		req.Header.Set("X-Fired-Token", testToken)
		resp := must(http.DefaultClient.Do(req))(t)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "<h1>hi</h1>" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("index: %d %q %v", resp.StatusCode, body, resp.Header)
		}
		etag := resp.Header.Get("ETag")
		req.Header.Set("If-None-Match", etag)
		resp = must(http.DefaultClient.Do(req))(t)
		resp.Body.Close()
		if resp.StatusCode != 304 {
			t.Fatalf("conditional get: %d", resp.StatusCode)
		}
		if _, err := c.ReadFile(ctx, id, "missing.txt", 0); !client.IsNotFound(err) {
			t.Fatalf("missing file: %v", err)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		_, err := c.Publish(ctx, id, pa.PublishRequest{BaseVersion: 2, Files: []pa.FileOp{pa.Text("x.txt", "x")}})
		wantCode(t, err, 409, "conflict")
		// Left out, base_version reads as 0, which is never a current version.
		_, err = c.Publish(ctx, id, pa.PublishRequest{Files: []pa.FileOp{pa.Text("x.txt", "x")}})
		wantCode(t, err, 400, "invalid_request")
		_, err = c.Restore(ctx, id, pa.RestoreRequest{Version: 1})
		wantCode(t, err, 400, "invalid_request")
		_, err = c.Restore(ctx, id, pa.RestoreRequest{BaseVersion: 1})
		wantCode(t, err, 400, "invalid_request")
	})

	t.Run("unchanged", func(t *testing.T) {
		v1 := must(c.Version(ctx, id, 1))(t)
		var same []pa.FileOp
		for _, f := range v1.Files {
			same = append(same, pa.FileOp{Path: f.Path, Hash: f.Hash})
		}
		for _, req := range []pa.PublishRequest{
			{BaseVersion: 1, Files: same, Label: "same files"},
			{BaseVersion: 1},
			{BaseVersion: 1, Files: []pa.FileOp{{Path: "nope.txt", Delete: true}}},
		} {
			r := must(c.Publish(ctx, id, req))(t)
			if !r.Unchanged || r.Version.Version != 1 || r.Version.Label != v1.Label {
				t.Fatalf("unchanged publish: %+v", r)
			}
		}
		r := must(c.Restore(ctx, id, pa.RestoreRequest{Version: 1, BaseVersion: 1}))(t)
		if !r.Unchanged || r.Version.Version != 1 {
			t.Fatalf("restore of the current files: %+v", r)
		}
		if n := len(must(c.Versions(ctx, id))(t)); n != 1 {
			t.Fatalf("unchanged publishes made versions: %d", n)
		}
	})

	t.Run("patch", func(t *testing.T) {
		r := must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{
			pa.Text("b.txt", "b"), {Path: "data/a.csv", Delete: true}, pa.Text("b.txt", "b2"),
		}}))(t)
		v := must(c.Version(ctx, id, r.Version.Version))(t)
		if got := strings.Join(paths(v), ","); got != "artifact.json,b.txt,index.html" || v.Parent != 1 {
			t.Fatalf("patched tree: %s parent %d", got, v.Parent)
		}
		if b := must(c.ReadFile(ctx, id, "b.txt", 0))(t); string(b) != "b2" {
			t.Fatalf("later entries win: %q", b)
		}
	})

	t.Run("replace_dirs mirrors a folder", func(t *testing.T) {
		must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: 2, Files: []pa.FileOp{pa.Text("app/x.js", "1"), pa.Text("app/y.js", "2")}}))(t)
		r := must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: 3, ReplaceDirs: []string{"app"}, Files: []pa.FileOp{pa.Text("app/x.js", "3")}}))(t)
		v := must(c.Version(ctx, id, r.Version.Version))(t)
		if got := strings.Join(paths(v), ","); got != "app/x.js,artifact.json,b.txt,index.html" {
			t.Fatalf("mirrored tree: %s", got)
		}
	})

	t.Run("blobs", func(t *testing.T) {
		data := bytes.Repeat([]byte("blob data "), 1000)
		op := must(c.Upload(ctx, "big.bin", data, 0o755))(t)
		if m := must(c.MissingBlobs(ctx, []string{op.Hash}))(t); len(m) != 0 {
			t.Fatalf("still missing: %v", m)
		}
		cur := must(c.Get(ctx, id))(t).CurrentVersion
		must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: cur, Files: []pa.FileOp{op}}))(t)
		if b := must(c.ReadFile(ctx, id, "big.bin", 0))(t); !bytes.Equal(b, data) {
			t.Fatal("blob content differs")
		}
		err := c.PutBlob(ctx, pa.HashBytes([]byte("other")), strings.NewReader("not other"))
		wantCode(t, err, 422, "hash_mismatch")
		_, err = c.Publish(ctx, id, pa.PublishRequest{BaseVersion: cur + 1, Files: []pa.FileOp{{Path: "z", Hash: pa.HashBytes([]byte("never uploaded"))}}})
		wantCode(t, err, 422, "missing_blob")
	})

	t.Run("invalid", func(t *testing.T) {
		cur := must(c.Get(ctx, id))(t).CurrentVersion
		bad := []pa.PublishRequest{
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text("../x", "x")}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text("/abs", "x")}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text("index.html/x", "x")}},
			{BaseVersion: cur, Files: []pa.FileOp{{Path: "x"}}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"views":[]}`)}, Spec: &pa.Spec{}},
			{BaseVersion: cur, Spec: &pa.Spec{View: &pa.View{Type: "html", Entry: "nope.html"}}},
			{BaseVersion: cur, Spec: &pa.Spec{View: &pa.View{Type: "flash", Entry: "index.html"}}},
			{BaseVersion: cur, Spec: &pa.Spec{Views: []pa.View{{Type: "html", Entry: "index.html"}, {Type: "text", Entry: "index.html"}}}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"view":{"type":"html","entry":"index.html"},"views":[{"type":"html","entry":"index.html"}]}`)}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"jobs":{"a":{"run":"x","schedul":"* * * * *"}}}`)}},
			{BaseVersion: cur, Spec: &pa.Spec{Jobs: map[string]pa.Job{"a": {Run: "x", Schedule: "61 * * * *"}}}},
			{BaseVersion: cur, Spec: &pa.Spec{Jobs: map[string]pa.Job{"a": {Run: "x", Writes: []string{pa.SpecFile}}}}},
			{BaseVersion: cur, Spec: &pa.Spec{Jobs: map[string]pa.Job{"a": {Run: "x", Writes: []string{"out"}, State: "out/state"}}}},
			{BaseVersion: cur, Spec: &pa.Spec{Jobs: map[string]pa.Job{"Bad Name": {Run: "x"}}}},
		}
		for i, req := range bad {
			if _, err := c.Publish(ctx, id, req); err == nil {
				t.Errorf("case %d: expected an error", i)
			} else {
				wantCode(t, err, 400, "invalid_request")
			}
		}
		if v := must(c.Get(ctx, id))(t).CurrentVersion; v != cur {
			t.Fatalf("failed publishes changed the version: %d → %d", cur, v)
		}
	})

	t.Run("spec written as artifact.json", func(t *testing.T) {
		cur := must(c.Get(ctx, id))(t).CurrentVersion
		r := must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: cur, Spec: &pa.Spec{View: &pa.View{Type: "md", Entry: "b.txt"}}}))(t)
		b := must(c.ReadFile(ctx, id, pa.SpecFile, 0))(t)
		spec, err := ParseSpec(b)
		if err != nil || spec.View == nil || spec.View.Type != "md" || !bytes.Contains(b, []byte(`"view"`)) {
			t.Fatalf("artifact.json: %s %v", b, err)
		}
		if r.Version.Spec.View.Entry != "b.txt" {
			t.Fatalf("version spec: %+v", r.Version.Spec)
		}
		// A later publish without spec keeps artifact.json and re-validates it.
		_, err = c.Publish(ctx, id, pa.PublishRequest{BaseVersion: r.Version.Version, Files: []pa.FileOp{{Path: "b.txt", Delete: true}}})
		wantCode(t, err, 400, "invalid_request")
	})

	t.Run("restore and fork", func(t *testing.T) {
		cur := must(c.Get(ctx, id))(t).CurrentVersion
		r := must(c.Restore(ctx, id, pa.RestoreRequest{Version: 1, BaseVersion: cur}))(t)
		v := must(c.Version(ctx, id, 0))(t)
		if v.Version != r.Version.Version || v.Origin != pa.OriginRestore || strings.Join(paths(v), ",") != "artifact.json,data/a.csv,index.html" {
			t.Fatalf("restore: %+v %v", v, paths(v))
		}
		f := must(c.Fork(ctx, id, pa.ForkRequest{Version: 2, QuotaKey: "u2"}))(t)
		if f.ForkedFrom == nil || f.ForkedFrom.Artifact != id || f.ForkedFrom.Version != 2 || f.CurrentVersion != 1 {
			t.Fatalf("fork: %+v", f)
		}
		fv := must(c.Version(ctx, f.ID, 1))(t)
		if fv.Origin != pa.OriginFork || strings.Join(paths(fv), ",") != "artifact.json,b.txt,index.html" {
			t.Fatalf("fork tree: %v", paths(fv))
		}
		vs := must(c.Versions(ctx, id))(t)
		if len(vs) != v.Version {
			t.Fatalf("versions: %d", len(vs))
		}
	})

	t.Run("export", func(t *testing.T) {
		resp := must(func() (*http.Response, error) {
			req, _ := http.NewRequest("GET", h.srv.URL+"/v1/artifacts/"+id+"/versions/1/export", nil)
			req.Header.Set("X-Fired-Token", testToken)
			return http.DefaultClient.Do(req)
		}())(t)
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "artifact.json,data/a.csv,index.html" {
			t.Fatalf("zip: %v", names)
		}
	})

	t.Run("list and update", func(t *testing.T) {
		l := must(c.List(ctx, "", map[string]string{"session": "s1"}))(t)
		if len(l) != 1 || l[0].ID != id {
			t.Fatalf("list: %+v", l)
		}
		ra := 3
		a := must(c.Update(ctx, id, pa.UpdateArtifactRequest{RetainRuns: &ra, Meta: json.RawMessage(`{"title":"New","n":12345678901234567890}`)}))(t)
		if a.RetainRuns != 3 || string(a.Meta) != `{"n":12345678901234567890,"title":"New"}` {
			t.Fatalf("update: %+v %s", a, a.Meta)
		}
		// Labels and meta merge: null removes a key, the rest are kept.
		str := func(s string) *string { return &s }
		a = must(c.Update(ctx, id, pa.UpdateArtifactRequest{
			Labels: map[string]*string{"session": nil, "stage": str("draft")},
			Meta:   json.RawMessage(`{"n":null,"description":"d"}`)}))(t)
		if len(a.Labels) != 1 || a.Labels["stage"] != "draft" || string(a.Meta) != `{"description":"d","title":"New"}` || a.RetainRuns != 3 {
			t.Fatalf("merge: %+v %s", a, a.Meta)
		}
		a = must(c.Update(ctx, id, pa.UpdateArtifactRequest{Labels: map[string]*string{"stage": nil}, Meta: json.RawMessage(`null`)}))(t)
		if a.Labels != nil || a.Meta != nil {
			t.Fatalf("clear: %+v %s", a, a.Meta)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := c.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(ctx, id); !client.IsNotFound(err) {
			t.Fatalf("after delete: %v", err)
		}
	})
}

func TestStorageQuota(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	// Every version has an artifact.json: here the default, "{}\n" (3 bytes).
	must(h.c.SetQuota(ctx, "small", pa.Quota{StorageBytes: 12}))(t)
	_, err := h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "small", Publish: &pa.PublishRequest{
		Files: []pa.FileOp{pa.Text("a.txt", "0123456789abcdef")}}})
	wantCode(t, err, 413, "quota_exceeded")
	if l := must(h.c.List(ctx, "small", nil))(t); len(l) != 0 {
		t.Fatalf("failed create left an artifact: %+v", l)
	}
	// Identical content is stored once, so a second copy is free.
	must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "small", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a.txt", "12345678")}}}))(t)
	must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "small", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("b.txt", "12345678")}}}))(t)
	st := must(h.c.Quota(ctx, "small"))(t)
	if st.StorageUsed != 11 {
		t.Fatalf("storage used: %d", st.StorageUsed)
	}
}

func TestJobsWindowsAndTriggers(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	spec := &pa.Spec{Jobs: map[string]pa.Job{
		"refresh": {Run: "python refresh.py", Writes: []string{"out"}, State: ".state", Schedule: "*/10 * * * *"},
		"long":    {Run: "sleep 1", Timeout: pa.Duration(2 * time.Hour)},
		"hook":    {Run: "handle", OnBusy: pa.OnBusyQueue, Queue: &pa.QueueSpec{Max: 2}},
		"monthly": {Run: "report", Schedule: "@monthly", ActiveFor: pa.Duration(720 * time.Hour)},
	}}
	res := must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{
		Files: []pa.FileOp{pa.Text("README.md", "hi")}, Spec: spec}}))(t)
	id := res.Artifact.ID
	jobs := map[string]pa.JobStatus{}
	for _, j := range res.Publish.Jobs {
		jobs[j.Name] = j
	}
	if j := jobs["refresh"]; j.State != pa.JobScheduled || j.ActiveUntil == nil || !j.ActiveUntil.Equal(h.now.Add(12*time.Hour)) ||
		j.NextRun == nil || !j.NextRun.Equal(h.now.Add(10*time.Minute)) {
		t.Fatalf("refresh: %+v", j)
	}
	if j := jobs["long"]; j.State != pa.JobBlocked || len(j.NeedsApproval) != 1 {
		t.Fatalf("long: %+v", j)
	}
	if j := jobs["hook"]; j.State != pa.JobManual {
		t.Fatalf("hook: %+v", j)
	}
	if j := jobs["monthly"]; j.State != pa.JobScheduled || len(j.NeedsApproval) != 1 || !j.ActiveUntil.Equal(h.now.Add(12*time.Hour)) {
		t.Fatalf("monthly runs in the default window until approved: %+v", j)
	}

	// A blocked job can't be triggered until a grant covers its timeout.
	_, err := c.Trigger(ctx, id, "long", nil)
	wantCode(t, err, 409, "needs_approval")
	g := must(c.Grant(ctx, id, "long", pa.Grant{MaxRun: pa.Duration(3 * time.Hour)}))(t)
	if g.State != pa.JobManual || len(g.NeedsApproval) != 0 {
		t.Fatalf("after grant: %+v", g)
	}
	must(c.Trigger(ctx, id, "long", nil))(t)

	// latest: a new trigger replaces the pending one.
	r1 := must(c.Trigger(ctx, id, "refresh", json.RawMessage(`{"n":1}`)))(t)
	r2 := must(c.Trigger(ctx, id, "refresh", json.RawMessage(`{"n":2}`)))(t)
	if old := must(c.Run(ctx, id, r1.ID))(t); old.Status != pa.RunSuperseded {
		t.Fatalf("first trigger: %+v", old)
	}
	if p := must(c.Runs(ctx, id, "refresh", pa.RunPending))(t); len(p) != 1 || p[0].ID != r2.ID {
		t.Fatalf("pending refresh runs: %+v", p)
	}

	// queue: triggers accumulate up to max, then 429.
	must(c.Trigger(ctx, id, "hook", json.RawMessage(`{"e":1}`)))(t)
	must(c.Trigger(ctx, id, "hook", json.RawMessage(`{"e":2}`)))(t)
	_, err = c.Trigger(ctx, id, "hook", json.RawMessage(`{"e":3}`))
	wantCode(t, err, 429, "queue_full")

	// The schedule window expires; publishing re-arms it.
	h.now = h.now.Add(13 * time.Hour)
	js := must(c.Jobs(ctx, id))(t)
	for _, j := range js {
		if j.Name == "refresh" && j.State != pa.JobExpired {
			t.Fatalf("refresh after 13h: %+v", j)
		}
	}
	cur := must(c.Get(ctx, id))(t).CurrentVersion
	pr := must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: cur, Files: []pa.FileOp{pa.Text("README.md", "hi again")}}))(t)
	for _, j := range pr.Jobs {
		if j.Name == "refresh" && j.State != pa.JobScheduled {
			t.Fatalf("refresh after republish: %+v", j)
		}
	}

	// A grant extends the window beyond the default.
	until := h.now.Add(30 * 24 * time.Hour)
	g = must(c.Grant(ctx, id, "monthly", pa.Grant{ActiveUntil: until}))(t)
	if g.State != pa.JobScheduled || len(g.NeedsApproval) != 0 || !g.ActiveUntil.Equal(until) {
		t.Fatalf("monthly after grant: %+v", g)
	}

	// A fork copies jobs but not their activation.
	f := must(c.Fork(ctx, id, pa.ForkRequest{QuotaKey: "u2"}))(t)
	for _, j := range must(c.Jobs(ctx, f.ID))(t) {
		if j.Name == "refresh" && j.State != pa.JobExpired {
			t.Fatalf("forked refresh should be inactive: %+v", j)
		}
	}
}

func TestScheduleFiring(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	must(h.c.PutTier(ctx, pa.Tier{Name: pa.DefaultTier, Limits: pa.Quota{MinInterval: pa.Duration(15 * time.Minute)}}))(t)
	res := must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{
		Spec: &pa.Spec{Jobs: map[string]pa.Job{"poll": {Run: "true", Schedule: "*/5 * * * *"}}}}}))(t)
	id := res.Artifact.ID
	fire := func() {
		if err := h.svc.fireSchedules(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs := func() []pa.Run { return must(h.c.Runs(ctx, id, "poll", ""))(t) }

	fire()
	if len(runs()) != 0 {
		t.Fatal("fired before it was due")
	}
	h.now = h.now.Add(5 * time.Minute)
	fire()
	if r := runs(); len(r) != 1 || r[0].Status != pa.RunPending || r[0].Trigger != pa.TriggerSchedule {
		t.Fatalf("after 5m: %+v", r)
	}
	// The quota's 15m minimum interval skips the next two slots.
	h.now = h.now.Add(5 * time.Minute)
	fire()
	h.now = h.now.Add(5 * time.Minute)
	fire()
	if r := runs(); len(r) != 1 {
		t.Fatalf("min interval not applied: %+v", r)
	}
	h.now = h.now.Add(5 * time.Minute)
	fire()
	r := runs()
	if len(r) != 2 || r[1].Status != pa.RunSuperseded {
		t.Fatalf("after 20m (latest replaces pending): %+v", r)
	}
	// Missed slots while down fire once.
	h.now = h.now.Add(3 * time.Hour)
	fire()
	if n := len(runs()); n != 3 {
		t.Fatalf("catch-up fired %d times", n-2)
	}
}

func TestGC(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Limits.DeleteAfter = pa.Duration(24 * time.Hour) })
	ctx := context.Background()
	keep := must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u", DeleteAfter: -1, Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "keep")}}}))(t)
	drop := must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "drop")}}}))(t)
	dropHash := pa.HashBytes([]byte("drop"))
	h.now = h.now.Add(25 * time.Hour)
	h.svc.GC(ctx)
	if _, err := h.c.Get(ctx, drop.Artifact.ID); !client.IsNotFound(err) {
		t.Fatalf("inactive artifact not deleted: %v", err)
	}
	if _, err := h.c.Get(ctx, keep.Artifact.ID); err != nil {
		t.Fatalf("delete_after -1 artifact deleted: %v", err)
	}
	// The blob is unreferenced now but within the grace period.
	if !h.svc.blobs.Has(dropHash) {
		t.Fatal("blob removed inside the grace period")
	}
	old := h.now.Add(-3 * time.Hour) // GC compares mtimes with the service clock
	_ = os.Chtimes(h.svc.blobs.path(dropHash), old, old)
	h.svc.GC(ctx)
	if h.svc.blobs.Has(dropHash) {
		t.Fatal("unreferenced blob kept")
	}
	if !h.svc.blobs.Has(pa.HashBytes([]byte("keep"))) {
		t.Fatal("referenced blob removed")
	}
}

func TestCreateNeedsFirstVersion(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	events, cancel := h.svc.bus.subscribe()
	defer cancel()

	_, err := c.Create(ctx, pa.CreateArtifactRequest{ID: "empty", QuotaKey: "u"})
	wantCode(t, err, 400, "invalid_request")
	_, err = c.Create(ctx, pa.CreateArtifactRequest{ID: "empty", QuotaKey: "u", Publish: &pa.PublishRequest{}})
	wantCode(t, err, 400, "invalid_request")
	_, err = c.Create(ctx, pa.CreateArtifactRequest{ID: "empty", QuotaKey: "u", Publish: &pa.PublishRequest{BaseVersion: 3, Files: []pa.FileOp{pa.Text("a", "x")}}})
	wantCode(t, err, 400, "invalid_request")
	// A publish that fails validation inside the transaction leaves nothing.
	_, err = c.Create(ctx, pa.CreateArtifactRequest{ID: "empty", QuotaKey: "u", Publish: &pa.PublishRequest{RunNow: []string{"nope"}, Files: []pa.FileOp{pa.Text("a", "x")}}})
	wantCode(t, err, 400, "invalid_request")
	if list := must(c.List(ctx, "", nil))(t); len(list) != 0 {
		t.Fatalf("failed creates left artifacts: %+v", list)
	}
	select {
	case e := <-events:
		t.Fatalf("failed creates emitted %+v", e)
	default:
	}

	res := must(c.Create(ctx, pa.CreateArtifactRequest{ID: "one", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "x")}}}))(t)
	if res.Artifact.CurrentVersion != 1 || res.Publish.Version.Version != 1 {
		t.Fatalf("create: %+v", res)
	}
	for _, want := range []string{"artifact.created", "artifact.version"} {
		if e := <-events; e.Type != want {
			t.Fatalf("event %s, want %s", e.Type, want)
		}
	}
	_, err = c.Create(ctx, pa.CreateArtifactRequest{ID: "one", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "y")}}})
	wantCode(t, err, 409, "exists")

	// No version may be empty. artifact.json counts (a jobs-only artifact
	// is fine) and can't be deleted, but a publish of nothing is refused.
	_, err = c.Publish(ctx, "one", pa.PublishRequest{BaseVersion: 1, Replace: true})
	wantCode(t, err, 400, "invalid_request")
	_, err = c.Publish(ctx, "one", pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{{Path: pa.SpecFile, Delete: true}}})
	wantCode(t, err, 400, "invalid_request")
	if a := must(c.Get(ctx, "one"))(t); a.CurrentVersion != 1 {
		t.Fatalf("refused publishes changed the artifact: %+v", a)
	}
}

// An artifact has one view. The earlier "views" list is read when it has one
// entry, and versions stored with several keep the first.
func TestOneView(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	r := must(c.Create(ctx, pa.CreateArtifactRequest{ID: "legacy", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{
		pa.Text("README.md", "# hi"), pa.Text(pa.SpecFile, `{"views":[{"type":"md","entry":"README.md"}]}`)}}}))(t)
	if v := r.Publish.Version.Spec; v.View == nil || v.View.Type != "md" || v.Views != nil {
		t.Fatalf("legacy views: %+v", v)
	}
	if v := loadedSpec(pa.Spec{Views: []pa.View{{Type: "md", Entry: "a"}, {Type: "csv", Entry: "b"}}}); v.View == nil || v.View.Entry != "a" || v.Views != nil {
		t.Fatalf("loaded: %+v", v)
	}
	if _, err := ParseSpec([]byte(`{"views":[{"type":"md","entry":"a"},{"type":"csv","entry":"b"}]}`)); err == nil || !strings.Contains(err.Error(), "one view") {
		t.Fatalf("two views: %v", err)
	}
}

// Every version has an artifact.json, with the title and description: the
// default is written when the files bring none, title and description can be
// set without replacing the rest, and they travel with restores and forks.
func TestSpecFileAlwaysThere(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	str := func(s string) *string { return &s }
	r := must(c.Create(ctx, pa.CreateArtifactRequest{ID: "page", QuotaKey: "u", Publish: &pa.PublishRequest{
		Files: []pa.FileOp{pa.Text("index.html", "hi")}, Title: str("Page")}}))(t)
	if r.Artifact.Title != "Page" || r.Publish.Version.Spec.Title != "Page" || r.Publish.Version.Spec.View.Entry != "index.html" {
		t.Fatalf("create: %+v %+v", r.Artifact, r.Publish.Version.Spec)
	}
	b := must(c.ReadFile(ctx, "page", pa.SpecFile, 0))(t)
	if !bytes.Contains(b, []byte(`"title": "Page"`)) || !bytes.Contains(b, []byte(`"view"`)) {
		t.Fatalf("artifact.json: %s", b)
	}

	// A folder mirrored at the root without artifact.json keeps the current one.
	p := must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 1, Replace: true, Files: []pa.FileOp{pa.Text("index.html", "v2")}}))(t)
	if p.Version.Spec.Title != "Page" {
		t.Fatalf("replace dropped artifact.json: %+v", p.Version.Spec)
	}

	// Title and description change only those fields; the view stays.
	p = must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 2, Description: str("About it")}))(t)
	if p.Unchanged || p.Version.Spec.Title != "Page" || p.Version.Spec.Description != "About it" || p.Version.Spec.View == nil {
		t.Fatalf("description: %+v", p.Version.Spec)
	}
	must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 3, Title: str("Renamed")}))(t)
	if a := must(c.Get(ctx, "page"))(t); a.Title != "Renamed" || a.Description != "About it" {
		t.Fatalf("rename: %+v", a)
	}
	// Spec replaces artifact.json; Title still wins over it.
	p = must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 4, Title: str("Renamed"), Spec: &pa.Spec{Title: "Ignored"}}))(t)
	if sp := p.Version.Spec; sp.Title != "Renamed" || sp.Description != "" || sp.View != nil {
		t.Fatalf("spec and title: %+v", sp)
	}

	// Restore and fork carry the version's title; a fork can rename.
	must(c.Restore(ctx, "page", pa.RestoreRequest{Version: 1, BaseVersion: 5}))(t)
	if a := must(c.Get(ctx, "page"))(t); a.Title != "Page" || a.Description != "" {
		t.Fatalf("restore: %+v", a)
	}
	f := must(c.Fork(ctx, "page", pa.ForkRequest{QuotaKey: "u2", Version: 4, Title: "Copy"}))(t)
	if f.Title != "Copy" || f.Description != "About it" {
		t.Fatalf("fork: %+v", f)
	}
	if fv := must(c.Version(ctx, f.ID, 1))(t); fv.Spec.Title != "Copy" {
		t.Fatalf("fork spec: %+v", fv.Spec)
	}
}
