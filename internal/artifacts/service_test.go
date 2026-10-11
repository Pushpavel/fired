package artifacts

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Pushpavel/fired/internal/store/storetest"
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
	cfg.DatabaseURL = storetest.URL(t)
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
	// No page unless one is set, index.html or not.
	if p := res.Publish.Version.Spec.View; p != nil {
		t.Fatalf("view without one set: %+v", p)
	}

	t.Run("serve", func(t *testing.T) {
		b := must(c.ReadFile(ctx, id, "index.html", 0))(t)
		if string(b) != "<h1>hi</h1>" {
			t.Fatalf("read: %q", b)
		}
		get := func(p string) *http.Response {
			req, _ := http.NewRequest("GET", h.srv.URL+"/v1/artifacts/"+id+"/files/"+p, nil)
			req.Header.Set("X-Fired-Token", testToken)
			return must(http.DefaultClient.Do(req))(t)
		}
		// No index files: a folder isn't a file.
		if resp := get(""); resp.StatusCode != 400 {
			t.Fatalf("empty path: %d", resp.StatusCode)
		}
		resp := get("index.html")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "<h1>hi</h1>" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("index: %d %q %v", resp.StatusCode, body, resp.Header)
		}
		req, _ := http.NewRequest("GET", h.srv.URL+"/v1/artifacts/"+id+"/files/index.html", nil)
		req.Header.Set("X-Fired-Token", testToken)
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
			{BaseVersion: cur, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "nope.html"}}},
			{BaseVersion: cur, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "b.txt"}}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"view":{"type":"html","entry":"index.html"}}`)}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"jobs":{"a":{"run":"x","schedul":"* * * * *"}}}`)}},
			{BaseVersion: cur, Spec: &pa.Spec{Job: &pa.Job{Run: "x", Schedule: "61 * * * *"}}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"job":{"run":"x","writes":["out"]}}`)}},
			{BaseVersion: cur, Spec: &pa.Spec{Job: &pa.Job{Run: " "}}},
			{BaseVersion: cur, Files: []pa.FileOp{pa.Text(pa.SpecFile, `{"jobs":{"a":{"run":"x"}}}`)}},
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
		r := must(c.Publish(ctx, id, pa.PublishRequest{BaseVersion: cur, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "index.html"}}}))(t)
		b := must(c.ReadFile(ctx, id, pa.SpecFile, 0))(t)
		spec, err := ParseSpec(b)
		if err != nil || spec.View == nil || spec.View.Entry != "index.html" {
			t.Fatalf("artifact.json: %s %v", b, err)
		}
		if r.Version.Spec.View.Entry != "index.html" {
			t.Fatalf("version spec: %+v", r.Version.Spec)
		}
		// A later publish without spec keeps artifact.json and re-validates it.
		_, err = c.Publish(ctx, id, pa.PublishRequest{BaseVersion: r.Version.Version, Files: []pa.FileOp{{Path: "index.html", Delete: true}}})
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
		a := must(c.Update(ctx, id, pa.UpdateArtifactRequest{Meta: json.RawMessage(`{"title":"New","n":12345678901234567890}`)}))(t)
		if string(a.Meta) != `{"n":12345678901234567890,"title":"New"}` {
			t.Fatalf("update: %+v %s", a, a.Meta)
		}
		// Labels and meta merge: null removes a key, the rest are kept.
		str := func(s string) *string { return &s }
		a = must(c.Update(ctx, id, pa.UpdateArtifactRequest{
			Labels: map[string]*string{"session": nil, "stage": str("draft")},
			Meta:   json.RawMessage(`{"n":null,"description":"d"}`)}))(t)
		if len(a.Labels) != 1 || a.Labels["stage"] != "draft" || string(a.Meta) != `{"description":"d","title":"New"}` {
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

func TestJobWindowsAndTriggers(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	mk := func(job pa.Job) (string, *pa.JobStatus) {
		t.Helper()
		res := must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{
			Files: []pa.FileOp{pa.Text("README.md", "hi")}, Spec: &pa.Spec{Job: &job}}}))(t)
		return res.Artifact.ID, res.Publish.Job
	}
	refresh, j := mk(pa.Job{Run: "python refresh.py", Schedule: "*/10 * * * *"})
	if j.State != pa.JobScheduled || j.ActiveUntil == nil || !j.ActiveUntil.Equal(h.now.Add(12*time.Hour)) ||
		j.NextRun == nil || !j.NextRun.Equal(h.now.Add(10*time.Minute)) {
		t.Fatalf("refresh: %+v", j)
	}
	long, j := mk(pa.Job{Run: "sleep 1", Timeout: pa.Duration(2 * time.Hour)})
	if j.State != pa.JobBlocked || len(j.NeedsApproval) != 1 {
		t.Fatalf("long: %+v", j)
	}
	hook, j := mk(pa.Job{Run: "handle", OnBusy: pa.OnBusyQueue, Queue: &pa.QueueSpec{Max: 2}})
	if j.State != pa.JobManual {
		t.Fatalf("hook: %+v", j)
	}
	monthly, j := mk(pa.Job{Run: "report", Schedule: "@monthly", ActiveFor: pa.Duration(720 * time.Hour)})
	if j.State != pa.JobScheduled || len(j.NeedsApproval) != 1 || !j.ActiveUntil.Equal(h.now.Add(12*time.Hour)) {
		t.Fatalf("monthly runs in the default window until approved: %+v", j)
	}
	none := must(c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "a")}}}))(t)
	if none.Publish.Job != nil {
		t.Fatalf("no job, but a status: %+v", none.Publish.Job)
	}
	_, err := c.Job(ctx, none.Artifact.ID)
	wantCode(t, err, 404, "no_job")

	// A blocked job can't be triggered until a grant covers its timeout.
	_, err = c.Trigger(ctx, long, nil)
	wantCode(t, err, 409, "needs_approval")
	g := must(c.Grant(ctx, long, pa.Grant{MaxRun: pa.Duration(3 * time.Hour)}))(t)
	if g.State != pa.JobManual || len(g.NeedsApproval) != 0 {
		t.Fatalf("after grant: %+v", g)
	}
	must(c.Trigger(ctx, long, nil))(t)

	// latest: a new trigger replaces the pending one.
	r1 := must(c.Trigger(ctx, refresh, json.RawMessage(`{"n":1}`)))(t)
	r2 := must(c.Trigger(ctx, refresh, json.RawMessage(`{"n":2}`)))(t)
	if old := must(c.Run(ctx, refresh, r1.ID))(t); old.Status != pa.RunSuperseded {
		t.Fatalf("first trigger: %+v", old)
	}
	if p := must(c.Runs(ctx, refresh, pa.RunPending))(t); len(p) != 1 || p[0].ID != r2.ID {
		t.Fatalf("pending refresh runs: %+v", p)
	}

	// queue: triggers accumulate up to max, then 429.
	must(c.Trigger(ctx, hook, json.RawMessage(`{"e":1}`)))(t)
	must(c.Trigger(ctx, hook, json.RawMessage(`{"e":2}`)))(t)
	_, err = c.Trigger(ctx, hook, json.RawMessage(`{"e":3}`))
	wantCode(t, err, 429, "queue_full")

	// The schedule window expires; publishing re-arms it.
	h.now = h.now.Add(13 * time.Hour)
	if j := must(c.Job(ctx, refresh))(t); j.State != pa.JobExpired {
		t.Fatalf("refresh after 13h: %+v", j)
	}
	pr := must(c.Publish(ctx, refresh, pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{pa.Text("README.md", "hi again")}}))(t)
	if pr.Job == nil || pr.Job.State != pa.JobScheduled {
		t.Fatalf("refresh after republish: %+v", pr.Job)
	}

	// A grant extends the window beyond the default.
	until := h.now.Add(30 * 24 * time.Hour)
	g = must(c.Grant(ctx, monthly, pa.Grant{ActiveUntil: until}))(t)
	if g.State != pa.JobScheduled || len(g.NeedsApproval) != 0 || !g.ActiveUntil.Equal(until) {
		t.Fatalf("monthly after grant: %+v", g)
	}

	// A fork copies the job but not its activation.
	f := must(c.Fork(ctx, refresh, pa.ForkRequest{QuotaKey: "u2"}))(t)
	if j := must(c.Job(ctx, f.ID))(t); j.State != pa.JobExpired {
		t.Fatalf("forked job should be inactive: %+v", j)
	}
}

func TestScheduleFiring(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	must(h.c.PutTier(ctx, pa.Tier{Name: pa.DefaultTier, Limits: pa.Quota{MinInterval: pa.Duration(15 * time.Minute)}}))(t)
	res := must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{
		Spec: &pa.Spec{Job: &pa.Job{Run: "true", Schedule: "*/5 * * * *"}}}}))(t)
	id := res.Artifact.ID
	daily := must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{
		Spec: &pa.Spec{Job: &pa.Job{Run: "true", Schedule: "0 3 * * *"}}}}))(t)
	next := func(id string) *time.Time { return must(h.c.Job(ctx, id))(t).NextRun }
	// 03:00 tomorrow is after the 12h window: no next run to show.
	if n := next(daily.Artifact.ID); n != nil {
		t.Fatalf("next run past the window: %v", n)
	}
	fire := func() {
		if err := h.svc.fireSchedules(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs := func() []pa.Run { return must(h.c.Runs(ctx, id, ""))(t) }

	fire()
	if len(runs()) != 0 {
		t.Fatal("fired before it was due")
	}
	h.now = h.now.Add(5 * time.Minute)
	fire()
	if r := runs(); len(r) != 1 || r[0].Status != pa.RunPending || r[0].Trigger != pa.TriggerSchedule {
		t.Fatalf("after 5m: %+v", r)
	}
	// The quota's 15m minimum interval skips the next two slots, and
	// next_run says so.
	if n := next(id); n == nil || !n.Equal(h.now.Add(15*time.Minute)) {
		t.Fatalf("next run after firing: %v, want %v", n, h.now.Add(15*time.Minute))
	}
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
	// Intervals count from the slot, not from when the scheduler got to
	// it: with a 5m minimum, a */5 schedule fired 3s late keeps its pace.
	must(h.c.PutTier(ctx, pa.Tier{Name: pa.DefaultTier, Limits: pa.Quota{MinInterval: pa.Duration(5 * time.Minute)}}))(t)
	h.svc.tiers = map[string]tierEntry{}
	h.now = h.now.Add(15*time.Minute + 3*time.Second) // the 10:35 slot, 3s late
	fire()
	if n := next(id); n == nil || !n.Equal(h.now.Truncate(time.Minute).Add(5*time.Minute)) {
		t.Fatalf("next run after a late firing: %v (now %v)", n, h.now)
	}
	h.now = h.now.Add(5 * time.Minute)
	before := len(runs())
	fire()
	if len(runs()) != before+1 {
		t.Fatal("the next slot after a late firing was skipped")
	}
	// Missed slots while down fire once.
	before = len(runs())
	h.now = h.now.Add(3*time.Hour + 4*time.Minute + 55*time.Second) // 13:44:58, 2s before a slot
	fire()
	if n := len(runs()); n != before+1 {
		t.Fatalf("catch-up fired %d times", n-before)
	}
	// The slot 2s after a catch-up firing is too close to it.
	if n := next(id); n == nil || n.Sub(h.now) < 5*time.Minute-h.svc.fireSlack() {
		t.Fatalf("next run %v right after a catch-up at %v", n, h.now)
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

	// An old unreferenced blob that /blobs/missing reports as present is
	// about to be referenced: GC gives it the grace period again, so the
	// publish that follows finds it.
	orphan := must(h.svc.blobs.PutBytes([]byte("orphan")))(t)
	_ = os.Chtimes(h.svc.blobs.path(orphan), old, old)
	if m := must(h.c.MissingBlobs(ctx, []string{orphan}))(t); len(m) != 0 {
		t.Fatalf("missing: %v", m)
	}
	h.svc.GC(ctx)
	must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{{Path: "a", Hash: orphan}}}}))(t)
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
	_, err = c.Create(ctx, pa.CreateArtifactRequest{ID: "empty", QuotaKey: "u", Publish: &pa.PublishRequest{RunNow: true, Files: []pa.FileOp{pa.Text("a", "x")}}})
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
	if r.Artifact.Title != "Page" || r.Publish.Version.Spec.Title != "Page" || r.Publish.Version.Spec.View != nil {
		t.Fatalf("create: %+v %+v", r.Artifact, r.Publish.Version.Spec)
	}
	b := must(c.ReadFile(ctx, "page", pa.SpecFile, 0))(t)
	if string(b) != "{\n  \"title\": \"Page\"\n}\n" {
		t.Fatalf("artifact.json: %q", b)
	}
	must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 1, Spec: &pa.Spec{Title: "Page", View: &pa.View{Type: pa.ViewPage, Entry: "index.html"}}}))(t)

	// A folder mirrored at the root without artifact.json keeps the current one.
	p := must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 2, Replace: true, Files: []pa.FileOp{pa.Text("index.html", "v2")}}))(t)
	if p.Version.Spec.Title != "Page" || p.Version.Spec.View == nil {
		t.Fatalf("replace dropped artifact.json: %+v", p.Version.Spec)
	}

	// Title and description change only those fields; the view stays.
	p = must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 3, Description: str("About it")}))(t)
	if p.Unchanged || p.Version.Spec.Title != "Page" || p.Version.Spec.Description != "About it" || p.Version.Spec.View == nil {
		t.Fatalf("description: %+v", p.Version.Spec)
	}
	must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 4, Title: str("Renamed")}))(t)
	if a := must(c.Get(ctx, "page"))(t); a.Title != "Renamed" || a.Description != "About it" {
		t.Fatalf("rename: %+v", a)
	}
	// Spec replaces artifact.json; Title still wins over it.
	p = must(c.Publish(ctx, "page", pa.PublishRequest{BaseVersion: 5, Title: str("Renamed"), Spec: &pa.Spec{Title: "Ignored"}}))(t)
	if sp := p.Version.Spec; sp.Title != "Renamed" || sp.Description != "" || sp.View != nil {
		t.Fatalf("spec and title: %+v", sp)
	}

	// Restore and fork carry the version's title; a fork can rename.
	must(c.Restore(ctx, "page", pa.RestoreRequest{Version: 1, BaseVersion: 6}))(t)
	if a := must(c.Get(ctx, "page"))(t); a.Title != "Page" || a.Description != "" {
		t.Fatalf("restore: %+v", a)
	}
	f := must(c.Fork(ctx, "page", pa.ForkRequest{QuotaKey: "u2", Version: 5, Title: "Copy"}))(t)
	if f.Title != "Copy" || f.Description != "About it" {
		t.Fatalf("fork: %+v", f)
	}
	if fv := must(c.Version(ctx, f.ID, 1))(t); fv.Spec.Title != "Copy" {
		t.Fatalf("fork spec: %+v", fv.Spec)
	}
}

// There is no page unless artifact.json sets one: files coming and going
// (a sandbox folder mirrored at the root) never change it.
func TestPageOnlyWhenSet(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	mirror := func(base int, files ...string) *pa.PublishResult {
		t.Helper()
		var ops []pa.FileOp
		for _, f := range files {
			ops = append(ops, pa.Text(f, f))
		}
		return must(c.Publish(ctx, "mirror", pa.PublishRequest{BaseVersion: base, Replace: true, Files: ops}))(t)
	}
	must(c.Create(ctx, pa.CreateArtifactRequest{ID: "mirror", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("page.html", "page.html")}}}))(t)
	if r := mirror(1, "page.html"); !r.Unchanged {
		t.Fatalf("same files: %+v", r)
	}
	if r := mirror(1, "page.html", "index.html"); r.Version.Spec.View != nil {
		t.Fatalf("index.html added: %+v", r.Version.Spec)
	}
	must(c.Publish(ctx, "mirror", pa.PublishRequest{BaseVersion: 2, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "index.html"}}}))(t)
	if r := mirror(3, "page.html", "index.html"); !r.Unchanged {
		t.Fatalf("set view kept: %+v", r)
	}
	_, err := c.Publish(ctx, "mirror", pa.PublishRequest{BaseVersion: 3, Replace: true, Files: []pa.FileOp{pa.Text("page.html", "x")}})
	wantCode(t, err, 400, "invalid_request")
}

// Viewers get the page at "/" and the files of its folder, nothing else,
// whether routed by headers or on a preview origin.
func TestServeView(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	must(h.c.Create(ctx, pa.CreateArtifactRequest{ID: "site", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{
		pa.Text("src/build.py", "secret"), pa.Text("dist/home.html", "home"), pa.Text("dist/about.html", "about"),
		pa.Text("dist/docs/index.html", "docs"),
	}}}))(t)
	get := func(p string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", h.srv.URL+p, nil)
		req.Header.Set("X-Fired-Token", testToken)
		req.Header.Set("X-Artifact", "site")
		resp := must(http.DefaultClient.Do(req))(t)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, body := get("/"); code != 404 || !strings.Contains(body, "no_view") {
		t.Fatalf("no page: %d %s", code, body)
	}
	must(h.c.Publish(ctx, "site", pa.PublishRequest{BaseVersion: 1, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "dist/home.html"}}}))(t)
	for p, want := range map[string]string{"/": "home", "/about.html": "about", "/docs/index.html": "docs"} {
		if code, body := get(p); code != 200 || body != want {
			t.Fatalf("%s: %d %q", p, code, body)
		}
	}
	for _, p := range []string{"/docs/", "/../src/build.py", "/src/build.py", "/artifact.json", "/home"} {
		if code, body := get(p); (code != 404 && code != 400) || strings.Contains(body, "dist") {
			t.Fatalf("%s: %d %s", p, code, body)
		}
	}
}

// A single-file view: "/" is the file as it is, with its type for the
// frontend; files next to it are reachable; ?download is an attachment.
func TestSingleFileView(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	md := &pa.View{Type: pa.ViewMarkdown, Entry: "notes/README.md"}
	must(h.c.Create(ctx, pa.CreateArtifactRequest{ID: "notes", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{
		pa.Text("notes/README.md", "# hi\n![d](d.svg)"), pa.Text("notes/d.svg", "<svg/>"), pa.Text("other.txt", "x"),
	}, Spec: &pa.Spec{View: md}}}))(t)
	get := func(p string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", h.srv.URL+p, nil)
		req.Header.Set("X-Fired-Token", testToken)
		req.Header.Set("X-Artifact", "notes")
		return must(http.DefaultClient.Do(req))(t)
	}
	r := get("/")
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || string(b) != "# hi\n![d](d.svg)" || r.Header.Get("X-Artifact-View") != "markdown" ||
		r.Header.Get("X-Artifact-View-Name") != "README.md" || r.Header.Get("Content-Disposition") != "" {
		t.Fatalf("entry: %d %q %v", r.StatusCode, b, r.Header)
	}
	if r := get("/d.svg"); r.StatusCode != 200 || r.Header.Get("X-Artifact-View") != "" {
		t.Fatalf("asset: %d %v", r.StatusCode, r.Header)
	}
	if r := get("/other.txt"); r.StatusCode != 404 {
		t.Fatalf("outside the folder: %d", r.StatusCode)
	}
	if r := get("/?download"); r.Header.Get("Content-Disposition") != `attachment; filename=README.md` {
		t.Fatalf("download: %v", r.Header)
	}

	// Types take their own kind of file; code takes any.
	for _, v := range []pa.View{{Type: "markdown", Entry: "notes/d.svg"}, {Type: "slides", Entry: "notes/README.md"}, {Type: "page", Entry: "notes/README.md"}} {
		_, err := h.c.Publish(ctx, "notes", pa.PublishRequest{BaseVersion: 1, Spec: &pa.Spec{View: &v}})
		wantCode(t, err, 400, "invalid_request")
	}
	must(h.c.Publish(ctx, "notes", pa.PublishRequest{BaseVersion: 1, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewCode, Entry: "other.txt"}}}))(t)
}

// The file limit counts what the publish brings, not the artifact.json the
// service writes; an oversized body is a 413 that says so.
func TestRequestLimits(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Limits.MaxFilesPerVersion = 2; c.Limits.MaxInlineBytes = 1 << 10 })
	ctx := context.Background()
	must(h.c.Create(ctx, pa.CreateArtifactRequest{ID: "two", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "a"), pa.Text("b", "b")}}}))(t)
	_, err := h.c.Create(ctx, pa.CreateArtifactRequest{ID: "three", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", "a"), pa.Text("b", "b"), pa.Text("c", "c")}}})
	wantCode(t, err, 413, "too_many_files")
	_, err = h.c.Create(ctx, pa.CreateArtifactRequest{ID: "big", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{pa.Text("a", strings.Repeat("x", 4<<20))}}})
	wantCode(t, err, 413, "too_large")
}

// Large files are stored as content-defined chunks: a new version of a big
// file shares most of them, uploads send only the missing ones, storage
// counts chunks, and GC follows file → chunks.
func TestChunkedContent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.c
	rnd := mrand.New(mrand.NewPCG(1, 2))
	data1 := make([]byte, 3<<20)
	for i := range data1 {
		data1[i] = byte(rnd.Uint32())
	}
	op := must(c.Upload(ctx, "big.bin", data1, 0))(t)
	must(c.Create(ctx, pa.CreateArtifactRequest{ID: "big", QuotaKey: "u", Publish: &pa.PublishRequest{Files: []pa.FileOp{op}}}))(t)
	chunks1, size, err := chunkList(ctx, h.svc.st.db, op.Hash)
	if err != nil || len(chunks1) < 2 || size != int64(len(data1)) || h.svc.blobs.Has(op.Hash) {
		t.Fatalf("stored as %d chunks, size %d, whole blob %v: %v", len(chunks1), size, h.svc.blobs.Has(op.Hash), err)
	}
	if b := must(c.ReadFile(ctx, "big", "big.bin", 0))(t); !bytes.Equal(b, data1) {
		t.Fatal("chunked file reads back different")
	}

	// Appending: only the last chunk(s) are new, so only they go up.
	data2 := append(append([]byte{}, data1...), bytes.Repeat([]byte("appended row\n"), 8000)...)
	var want []string
	_ = pa.SplitChunks(bytes.NewReader(data2), func(b []byte) error { want = append(want, pa.HashBytes(b)); return nil })
	missing := must(c.MissingBlobs(ctx, want))(t)
	if len(missing) == 0 || len(missing) > 2 {
		t.Fatalf("an append should miss 1-2 of %d chunks, missing %d", len(want), len(missing))
	}
	op2 := must(c.Upload(ctx, "big.bin", data2, 0))(t)
	must(c.Publish(ctx, "big", pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{op2}}))(t)
	if b := must(c.ReadFile(ctx, "big", "big.bin", 0))(t); !bytes.Equal(b, data2) {
		t.Fatal("v2 reads back different")
	}
	q := must(c.Quota(ctx, "u"))(t)
	if q.StorageUsed > int64(len(data2))+int64(len(data1))/2 || q.StorageLogical < int64(len(data1)+len(data2)) {
		t.Fatalf("storage used %d, logical %d: chunks aren't shared", q.StorageUsed, q.StorageLogical)
	}

	// A whole-file upload is chunked by the service, sharing chunks too.
	data3 := append([]byte{}, data1...)
	data3[len(data3)/2] ^= 0xff
	h3 := pa.HashBytes(data3)
	if err := c.PutBlob(ctx, h3, bytes.NewReader(data3)); err != nil {
		t.Fatal(err)
	}
	chunks3, _, _ := chunkList(ctx, h.svc.st.db, h3)
	shared := 0
	for i := range chunks3 {
		if i < len(chunks1) && chunks3[i] == chunks1[i] {
			shared++
		}
	}
	if shared < len(chunks1)-2 {
		t.Fatalf("one changed byte should change 1-2 chunks; shared %d of %d", shared, len(chunks1))
	}
	err = c.PutBlob(ctx, pa.HashBytes([]byte("other")), bytes.NewReader(data3))
	wantCode(t, err, 422, "hash_mismatch")

	// A chunk list must be uploaded, and add up to the hash in order.
	rev := append([]pa.ChunkRef{}, chunks1...)
	rev[0], rev[1] = rev[1], rev[0]
	wantCode(t, c.PutChunks(ctx, pa.HashBytes([]byte("x")), rev), 422, "hash_mismatch")
	wantCode(t, c.PutChunks(ctx, pa.HashBytes([]byte("y")), []pa.ChunkRef{{Hash: pa.HashBytes([]byte("never")), Size: 5}}), 422, "missing_blob")

	// GC: once nothing references them, chunked files and chunks go.
	if err := c.Delete(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(3 * time.Hour)
	old := h.now.Add(-3 * time.Hour)
	_ = h.svc.blobs.Walk(func(b string, _ time.Time) error { return os.Chtimes(h.svc.blobs.path(b), old, old) })
	h.svc.GC(ctx)
	if cs, _, _ := chunkList(ctx, h.svc.st.db, op.Hash); cs != nil || h.svc.blobs.Has(chunks1[0].Hash) {
		t.Fatalf("unreferenced chunks kept: list %v, chunk blob %v", cs != nil, h.svc.blobs.Has(chunks1[0].Hash))
	}
}

func TestDB(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Limits.MaxDocBytes = 200; c.Limits.MaxDocs = 6 })
	ctx := context.Background()
	c := h.c
	must(c.Create(ctx, pa.CreateArtifactRequest{ID: "app", QuotaKey: "u", Publish: &pa.PublishRequest{
		Files: []pa.FileOp{pa.Text("index.html", "page")}, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "index.html"}}}}))(t)

	t.Run("documents", func(t *testing.T) {
		d := must(c.SetDoc(ctx, "app", "tasks/t1", map[string]any{"title": "one", "n": 3, "done": false, "meta": map[string]any{"p": 2}}))(t)
		if d.Rev != 1 || string(d.Data) != `{"n":3,"done":false,"meta":{"p":2},"title":"one"}` {
			t.Fatalf("set: %+v %s", d, d.Data)
		}
		d = must(c.UpdateDoc(ctx, "app", "tasks/t1", map[string]any{"done": true, "meta": nil}))(t)
		if d.Rev != 2 || string(d.Data) != `{"n":3,"done":true,"title":"one"}` {
			t.Fatalf("update: %+v %s", d, d.Data)
		}
		// update creates; its nulls are dropped.
		d = must(c.UpdateDoc(ctx, "app", "tasks/t2", map[string]any{"title": "two", "n": 1, "x": nil}))(t)
		if d.Rev != 1 || string(d.Data) != `{"n":1,"title":"two"}` {
			t.Fatalf("update creates: %s", d.Data)
		}
		must(c.SetDoc(ctx, "app", "tasks/t3", map[string]any{"title": "three", "n": "10"}))(t)
		must(c.SetDoc(ctx, "app", "tasks/t1/notes/a", map[string]any{"text": "deeper"}))(t)
		if got := must(c.GetDoc(ctx, "app", "tasks/t1"))(t); got.Rev != 2 {
			t.Fatalf("get: %+v", got)
		}
		_, err := c.GetDoc(ctx, "app", "tasks/nope")
		wantCode(t, err, 404, "no_document")
		_, err = c.SetDoc(ctx, "app", "tasks", map[string]any{})
		wantCode(t, err, 400, "invalid_request")
		_, err = c.SetDoc(ctx, "app", "tasks/../x", map[string]any{})
		wantCode(t, err, 400, "invalid_request")
		_, err = c.SetDoc(ctx, "app", "tasks/arr", []int{1})
		wantCode(t, err, 400, "invalid_request")
		_, err = c.SetDoc(ctx, "app", "tasks/big", map[string]any{"s": strings.Repeat("x", 300)})
		wantCode(t, err, 413, "too_large")
		_, err = c.GetDoc(ctx, "nope", "a/b")
		wantCode(t, err, 404, "not_found")
	})

	t.Run("queries", func(t *testing.T) {
		ids := func(l *pa.DocList) string {
			var out []string
			for _, d := range l.Docs {
				out = append(out, d.Path)
			}
			return strings.Join(out, ",")
		}
		q := func(dq pa.DocQuery) string { return ids(must(c.Query(ctx, "app", "tasks", dq))(t)) }
		// A collection is the documents directly in it.
		if got := q(pa.DocQuery{}); got != "tasks/t1,tasks/t2,tasks/t3" {
			t.Fatalf("all: %s", got)
		}
		if got := q(pa.DocQuery{Where: []string{"done==true"}}); got != "tasks/t1" {
			t.Fatalf("where ==: %s", got)
		}
		if got := q(pa.DocQuery{Where: []string{"title!=one"}}); got != "tasks/t2,tasks/t3" {
			t.Fatalf("where != (bare string): %s", got)
		}
		// Ranges only match values of the filter's type: "10" isn't > 2.
		if got := q(pa.DocQuery{Where: []string{"n>=2"}}); got != "tasks/t1" {
			t.Fatalf("where >=: %s", got)
		}
		// Across types, jsonb orders strings before numbers.
		if got := q(pa.DocQuery{Order: "n"}); got != "tasks/t3,tasks/t2,tasks/t1" {
			t.Fatalf("order n: %s", got)
		}
		if got := q(pa.DocQuery{Order: "-n"}); got != "tasks/t1,tasks/t2,tasks/t3" {
			t.Fatalf("order -n: %s", got)
		}
		if got := q(pa.DocQuery{Order: "done"}); got != "tasks/t1,tasks/t2,tasks/t3" {
			t.Fatalf("order done (missing last): %s", got)
		}
		p1 := must(c.Query(ctx, "app", "tasks", pa.DocQuery{Limit: 2}))(t)
		p2 := must(c.Query(ctx, "app", "tasks", pa.DocQuery{Limit: 2, After: p1.Next}))(t)
		if ids(p1) != "tasks/t1,tasks/t2" || p1.Next == "" || ids(p2) != "tasks/t3" || p2.Next != "" {
			t.Fatalf("pages: %s %q, %s %q", ids(p1), p1.Next, ids(p2), p2.Next)
		}
		if got := ids(must(c.Query(ctx, "app", "tasks/t1/notes", pa.DocQuery{}))(t)); got != "tasks/t1/notes/a" {
			t.Fatalf("subcollection: %s", got)
		}
		_, err := c.Query(ctx, "app", "tasks", pa.DocQuery{Where: []string{"n~3"}})
		wantCode(t, err, 400, "invalid_request")
	})

	t.Run("revisions and batches", func(t *testing.T) {
		_, err := c.Batch(ctx, "app", []pa.DBWrite{{Op: pa.DBSet, Path: "tasks/t1", Data: json.RawMessage(`{}`), Rev: 1}})
		wantCode(t, err, 412, "rev_mismatch")
		_, err = c.Batch(ctx, "app", []pa.DBWrite{{Op: pa.DBSet, Path: "tasks/t1", Data: json.RawMessage(`{}`), Rev: -1}})
		wantCode(t, err, 412, "rev_mismatch")
		// All or none: the second write fails, so the first doesn't land.
		_, err = c.Batch(ctx, "app", []pa.DBWrite{
			{Op: pa.DBSet, Path: "tasks/t4", Data: json.RawMessage(`{"a":1}`)},
			{Op: pa.DBUpdate, Path: "tasks/t2", Data: json.RawMessage(`{"n":2}`), Rev: 7},
		})
		wantCode(t, err, 412, "rev_mismatch")
		_, err = c.GetDoc(ctx, "app", "tasks/t4")
		wantCode(t, err, 404, "no_document")
		res := must(c.Batch(ctx, "app", []pa.DBWrite{
			{Op: pa.DBUpdate, Path: "tasks/t2", Data: json.RawMessage(`{"n":2}`), Rev: 1},
			{Op: pa.DBDelete, Path: "tasks/t3"},
			{Op: pa.DBDelete, Path: "tasks/never"},
		}))(t)
		if len(res.Docs) != 3 || res.Docs[0].Rev != 2 || !res.Docs[1].Deleted || !res.Docs[2].Deleted {
			t.Fatalf("batch: %+v", res.Docs)
		}
		// t1, t2, t1/notes/a: three of the six documents allowed.
		_, err = c.Batch(ctx, "app", []pa.DBWrite{
			{Op: pa.DBSet, Path: "x/1", Data: json.RawMessage(`{}`)}, {Op: pa.DBSet, Path: "x/2", Data: json.RawMessage(`{}`)},
			{Op: pa.DBSet, Path: "x/3", Data: json.RawMessage(`{}`)}, {Op: pa.DBSet, Path: "x/4", Data: json.RawMessage(`{}`)},
		})
		wantCode(t, err, 413, "too_many_docs")
	})

	// Views: the owner writes and sees _private/, everyone else reads the rest.
	view := func(method, p, body string, owner bool) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, h.srv.URL+p, strings.NewReader(body))
		req.Header.Set("X-Fired-Token", testToken)
		req.Header.Set("X-Artifact", "app")
		if owner {
			req.Header.Set("X-Artifact-Owner", "true")
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp := must(http.DefaultClient.Do(req))(t)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	t.Run("from the page", func(t *testing.T) {
		must(c.SetDoc(ctx, "app", "_private/state", map[string]any{"cursor": 7}))(t)
		if code, body, _ := view("GET", "/.fired/db/tasks/t1", "", false); code != 200 || !strings.Contains(body, `"title":"one"`) {
			t.Fatalf("viewer reads: %d %s", code, body)
		}
		for _, p := range []string{"/.fired/db/_private/state", "/.fired/db/_private"} {
			code, body, _ := view("GET", p, "", false)
			if p == "/.fired/db/_private" && (code != 200 || !strings.Contains(body, `"docs":[]`)) || p != "/.fired/db/_private" && code != 404 {
				t.Fatalf("viewer reads %s: %d %s", p, code, body)
			}
		}
		if code, body, _ := view("GET", "/.fired/db", "", false); code != 200 || strings.Contains(body, "_private") || !strings.Contains(body, "tasks/t1/notes/a") {
			t.Fatalf("viewer lists all: %d %s", code, body)
		}
		if code, body, _ := view("PUT", "/.fired/db/tasks/t9", `{"a":1}`, false); code != 403 {
			t.Fatalf("viewer writes: %d %s", code, body)
		}
		if code, _, _ := view("DELETE", "/.fired/db/tasks", "", false); code != 403 {
			t.Fatalf("viewer deletes: %d", code)
		}
		if code, body, _ := view("GET", "/.fired/db/_private/state", "", true); code != 200 || !strings.Contains(body, `"cursor":7`) {
			t.Fatalf("owner reads _private/: %d %s", code, body)
		}
		if code, body, _ := view("PATCH", "/.fired/db/tasks/t1", `{"n":4}`, true); code != 200 || !strings.Contains(body, `"rev":3`) {
			t.Fatalf("owner writes: %d %s", code, body)
		}
		if code, _, _ := view("PATCH", "/.fired/db/tasks/t1", `{"n":5}`, true); code != 200 {
			t.Fatalf("owner writes again: %d", code)
		}
		req, _ := http.NewRequest("PATCH", h.srv.URL+"/.fired/db/tasks/t1", strings.NewReader(`{"n":6}`))
		req.Header.Set("X-Fired-Token", testToken)
		req.Header.Set("X-Artifact", "app")
		req.Header.Set("X-Artifact-Owner", "true")
		req.Header.Set("Content-Type", "text/plain")
		resp := must(http.DefaultClient.Do(req))(t)
		resp.Body.Close()
		if resp.StatusCode != 415 {
			t.Fatalf("a write that isn't JSON: %d", resp.StatusCode)
		}
		if code, body, hdr := view("GET", "/.fired/db.js", "", false); code != 200 || !strings.Contains(body, "const canWrite = false;") || !strings.HasPrefix(hdr.Get("Content-Type"), "text/javascript") {
			t.Fatalf("db.js for a viewer: %d %s", code, hdr)
		}
		if _, body, _ := view("GET", "/.fired/db.js", "", true); !strings.Contains(body, "const canWrite = true;") {
			t.Fatal("db.js for the owner")
		}
		// The job: everyone sees its status, only the owner runs it.
		if code, body, _ := view("GET", "/.fired/job", "", false); code != 404 || !strings.Contains(body, "no_job") {
			t.Fatalf("job status without a job: %d %s", code, body)
		}
		must(c.Publish(ctx, "app", pa.PublishRequest{BaseVersion: 1, Spec: &pa.Spec{View: &pa.View{Type: pa.ViewPage, Entry: "index.html"}, Job: &pa.Job{Run: "true"}}}))(t)
		if code, body, _ := view("GET", "/.fired/job", "", false); code != 200 || !strings.Contains(body, `"state":"manual"`) {
			t.Fatalf("job status for a viewer: %d %s", code, body)
		}
		if code, body, _ := view("POST", "/.fired/job/runs", `{"payload":{"q":1}}`, false); code != 403 {
			t.Fatalf("a viewer runs the job: %d %s", code, body)
		}
		if code, body, _ := view("POST", "/.fired/job/runs", `{"payload":{"q":1}}`, true); code != 202 || !strings.Contains(body, `"trigger":"page"`) {
			t.Fatalf("the owner runs the job: %d %s", code, body)
		}
		if code, _, _ := view("POST", "/.fired/job/runs", ``, true); code != 415 {
			t.Fatalf("a run that isn't JSON: %d", code)
		}
		if code, _, _ := view("GET", "/.fired/other", "", true); code != 404 {
			t.Fatalf("/.fired/other: %d", code)
		}
	})

	t.Run("watch", func(t *testing.T) {
		req, _ := http.NewRequest("GET", h.srv.URL+"/.fired/db/tasks?watch&where=done==true", nil)
		req.Header.Set("X-Fired-Token", testToken)
		req.Header.Set("X-Artifact", "app")
		resp := must(http.DefaultClient.Do(req))(t)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		next := func() string {
			t.Helper()
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if d, ok := strings.CutPrefix(line, "data: "); ok {
					return strings.TrimSpace(d)
				}
			}
		}
		if first := next(); !strings.Contains(first, "tasks/t1") || strings.Contains(first, "tasks/t2") {
			t.Fatalf("first snapshot: %s", first)
		}
		must(c.SetDoc(ctx, "app", "other/x", map[string]any{"a": 1}))(t) // not in tasks
		must(c.UpdateDoc(ctx, "app", "tasks/t2", map[string]any{"done": true}))(t)
		if got := next(); !strings.Contains(got, "tasks/t1") || !strings.Contains(got, "tasks/t2") {
			t.Fatalf("after a change: %s", got)
		}

		dreq, _ := http.NewRequest("GET", h.srv.URL+"/v1/artifacts/app/db/tasks/gone?watch", nil)
		dreq.Header.Set("X-Fired-Token", testToken)
		dresp := must(http.DefaultClient.Do(dreq))(t)
		defer dresp.Body.Close()
		br = bufio.NewReader(dresp.Body)
		if got := next(); got != "null" {
			t.Fatalf("a missing document: %s", got)
		}
		must(c.SetDoc(ctx, "app", "tasks/gone", map[string]any{"a": 1}))(t)
		if got := next(); !strings.Contains(got, `"rev":1`) {
			t.Fatalf("created: %s", got)
		}
	})

	t.Run("a viewer can't watch _private/", func(t *testing.T) {
		if code, _, _ := view("GET", "/.fired/db/_private/state?watch", "", false); code != 404 {
			t.Fatalf("viewer watches _private/: %d", code)
		}
	})

	t.Run("delete a collection, clear, events and quota", func(t *testing.T) {
		if err := c.DeleteDoc(ctx, "app", "tasks"); err != nil {
			t.Fatal(err)
		}
		if l := must(c.Query(ctx, "app", "", pa.DocQuery{}))(t); len(l.Docs) != 2 {
			t.Fatalf("after deleting tasks: %+v", l.Docs)
		}
		st := must(c.Quota(ctx, "u"))(t)
		if st.DBBytes == 0 || st.StorageUsed < st.DBBytes {
			t.Fatalf("quota: %+v", st)
		}
		must(c.SetQuota(ctx, "u", pa.Quota{StorageBytes: st.StorageUsed + 10}))(t)
		_, err := c.SetDoc(ctx, "app", "other/big", map[string]any{"s": strings.Repeat("x", 50)})
		wantCode(t, err, 413, "quota_exceeded")
		// Shrinking always works.
		must(c.SetDoc(ctx, "app", "other/x", map[string]any{}))(t)
		if err := c.ClearDB(ctx, "app"); err != nil {
			t.Fatal(err)
		}
		if l := must(c.Query(ctx, "app", "", pa.DocQuery{}))(t); len(l.Docs) != 0 {
			t.Fatalf("after clearing: %+v", l.Docs)
		}
		if err := c.Delete(ctx, "app"); err != nil {
			t.Fatal(err)
		}
		var n int
		_ = h.svc.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs`).Scan(&n)
		if n != 0 {
			t.Fatalf("docs left after delete: %d", n)
		}
	})
}
