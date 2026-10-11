package artifacts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

func TestUI(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.UI = true; c.PreviewDomain = "preview.test" })
	ctx := context.Background()

	do := func(method, path, body string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			if k == "Host" {
				req.Host = v
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	read := func(r *http.Response) string {
		t.Helper()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}

	if r := do("GET", "/ui/", "", nil); r.StatusCode != 200 || !strings.Contains(read(r), "fired artifacts") {
		t.Fatalf("ui page: %d", r.StatusCode)
	}
	if r := do("POST", "/ui/login", `{"token":"nope"}`, nil); r.StatusCode != 401 {
		t.Fatalf("bad login: %d", r.StatusCode)
	}
	r := do("POST", "/ui/login", `{"token":"`+testToken+`"}`, nil)
	if r.StatusCode != 204 || len(r.Cookies()) != 1 {
		t.Fatalf("login: %d %v", r.StatusCode, r.Cookies())
	}
	ck := r.Cookies()[0]
	if ck.Name != sessionCookie || strings.Contains(ck.Value, testToken) || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", ck)
	}
	cookie := ck.Name + "=" + ck.Value

	for _, c := range []struct {
		hdr  map[string]string
		want int
	}{
		{map[string]string{"Cookie": cookie}, 200},
		{map[string]string{"Cookie": cookie, "Origin": h.srv.URL, "Sec-Fetch-Site": "same-origin"}, 200},
		{map[string]string{"Cookie": cookie, "Origin": "http://evil.example"}, 401},
		{map[string]string{"Cookie": cookie, "Sec-Fetch-Site": "same-site"}, 401},
		{map[string]string{"Cookie": sessionCookie + "=forged"}, 401},
		{nil, 401},
	} {
		if r := do("GET", "/v1/artifacts", "", c.hdr); r.StatusCode != c.want {
			t.Errorf("%v: got %d, want %d", c.hdr, r.StatusCode, c.want)
		}
	}
	if r := do("GET", "/ui/info", "", nil); r.StatusCode != 401 {
		t.Fatalf("info without session: %d", r.StatusCode)
	}

	// Preview links: one origin per artifact and version, serving its view.
	res := must(h.c.Create(ctx, pa.CreateArtifactRequest{QuotaKey: "u1", Publish: &pa.PublishRequest{Files: []pa.FileOp{
		pa.Text("index.html", "<h1>one</h1>"), pa.Text("app/x.js", "1"),
	}, Spec: &pa.Spec{Page: "index.html"}}}))(t)
	id := res.Artifact.ID
	must(h.c.Publish(ctx, id, pa.PublishRequest{BaseVersion: 1, Files: []pa.FileOp{pa.Text("index.html", "<h1>two</h1>")}}))(t)

	link := func(v int) string {
		t.Helper()
		r := do("POST", "/ui/preview-link", `{"artifact":"`+id+`","version":`+strconv.Itoa(v)+`}`, map[string]string{"Cookie": cookie})
		var out struct{ URL string }
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil || r.StatusCode != 200 {
			t.Fatalf("preview-link: %d %v", r.StatusCode, err)
		}
		u, _ := url.Parse(out.URL)
		if !strings.HasSuffix(u.Hostname(), ".preview.test") {
			t.Fatalf("preview host %s", u.Host)
		}
		return u.Host
	}
	cur, v1 := link(0), link(1)
	if r := do("GET", "/", "", map[string]string{"Host": cur}); read(r) != "<h1>two</h1>" {
		t.Fatalf("current preview: %d", r.StatusCode)
	}
	if r := do("GET", "/", "", map[string]string{"Host": v1}); read(r) != "<h1>one</h1>" || r.Header.Get("X-Artifact-Version") != "1" {
		t.Fatalf("pinned preview")
	}
	if r := do("GET", "/app/x.js", "", map[string]string{"Host": v1}); read(r) != "1" {
		t.Fatalf("preview subpath: %d", r.StatusCode)
	}
	for _, p := range []string{"/app/", "/artifact.json", "/.fired/view"} {
		if r := do("GET", p, "", map[string]string{"Host": v1}); r.StatusCode != 404 {
			t.Fatalf("%s on preview: %d", p, r.StatusCode)
		}
	}
	// A preview origin is read-only and can't reach the API, even with the token.
	if r := do("POST", "/v1/artifacts", `{}`, map[string]string{"Host": v1, "X-Fired-Token": testToken}); r.StatusCode != 405 {
		t.Fatalf("POST on preview: %d", r.StatusCode)
	}
	if r := do("GET", "/v1/artifacts", "", map[string]string{"Host": v1, "X-Fired-Token": testToken}); r.StatusCode != 404 {
		t.Fatalf("API on preview: %d", r.StatusCode)
	}
	// A forged or re-pointed signature is not a preview.
	forged := strings.Replace(v1, id+"-1-", id+"-2-", 1)
	if r := do("GET", "/app/x.js", "", map[string]string{"Host": forged}); r.StatusCode != 401 {
		t.Fatalf("forged preview: %d", r.StatusCode)
	}

	// Without ui, none of it exists.
	plain := newHarness(t, nil)
	resp, err := http.Get(plain.srv.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("ui disabled: %d", resp.StatusCode)
	}
}
