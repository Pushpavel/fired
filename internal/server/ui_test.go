package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Pushpavel/fired/internal/config"
	"github.com/Pushpavel/fired/pkg/api"
)

func TestUI(t *testing.T) {
	e := newEnv(t, t.TempDir(), func(c *config.Config) { c.UI = true })
	defer e.close("")
	ctx := context.Background()

	do := func(method, path, body string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	if r := do("GET", "/ui/", "", nil); r.StatusCode != 200 {
		t.Fatalf("ui page: %d", r.StatusCode)
	}
	if r := do("POST", "/ui/login", `{"token":"nope"}`, nil); r.StatusCode != 401 {
		t.Fatalf("bad login: %d", r.StatusCode)
	}
	r := do("POST", "/ui/login", `{"token":"`+token+`"}`, nil)
	if r.StatusCode != 204 || len(r.Cookies()) != 1 {
		t.Fatalf("login: %d %v", r.StatusCode, r.Cookies())
	}
	ck := r.Cookies()[0]
	if strings.Contains(ck.Value, token) || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", ck)
	}
	cookie := ck.Name + "=" + ck.Value

	for _, c := range []struct {
		hdr  map[string]string
		want int
	}{
		{map[string]string{"Cookie": cookie}, 200},
		{map[string]string{"Cookie": cookie, "Origin": e.srv.URL, "Sec-Fetch-Site": "same-origin"}, 200},
		{map[string]string{"Cookie": cookie, "Origin": "http://evil.example"}, 401},
		{map[string]string{"Cookie": cookie, "Sec-Fetch-Site": "same-site"}, 401},
		{map[string]string{"Cookie": "fired_session=forged"}, 401},
	} {
		if r := do("GET", "/v1/host", "", c.hdr); r.StatusCode != c.want {
			t.Errorf("%v: got %d, want %d", c.hdr, r.StatusCode, c.want)
		}
	}

	// Preview links: signed per port, sandboxed, never see the session cookie.
	e.template("base", false)
	sb := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base"}))(t)
	port := freePort(t)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	app := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.Path+" cookie="+r.Header.Get("Cookie"))
	})}
	go app.Serve(ln)
	defer app.Close()
	if err := e.c.ExposePort(ctx, sb.ID, port); err != nil {
		t.Fatal(err)
	}
	if r := do("POST", "/ui/preview-link", `{"sandbox":"`+sb.ID+`","port":`+strconv.Itoa(port)+`}`, nil); r.StatusCode != 401 {
		t.Fatalf("preview-link without session: %d", r.StatusCode)
	}
	r = do("POST", "/ui/preview-link", `{"sandbox":"`+sb.ID+`","port":`+strconv.Itoa(port)+`}`, map[string]string{"Cookie": cookie})
	var link struct{ URL string }
	if err := json.NewDecoder(r.Body).Decode(&link); err != nil || !strings.HasPrefix(link.URL, "/v1/preview/") {
		t.Fatalf("preview-link: %d %v %q", r.StatusCode, err, link.URL)
	}
	r = do("GET", link.URL+"a/b?x=1", "", map[string]string{"Cookie": cookie + "; theme=dark"})
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(body) != "/a/b cookie=theme=dark" {
		t.Fatalf("preview: %d %q", r.StatusCode, body)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("preview CSP: %q", r.Header.Get("Content-Security-Policy"))
	}
	bad := strings.Replace(link.URL, "/"+strconv.Itoa(port)+"/", "/"+strconv.Itoa(port+1)+"/", 1)
	if r := do("GET", bad, "", nil); r.StatusCode != 401 {
		t.Fatalf("preview with another port's signature: %d", r.StatusCode)
	}
}
