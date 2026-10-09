package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
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
	if err := json.NewDecoder(r.Body).Decode(&link); err != nil {
		t.Fatalf("preview-link: %d %v", r.StatusCode, err)
	}
	u, err := url.Parse(link.URL)
	if err != nil || !strings.HasPrefix(u.Hostname(), "p"+strconv.Itoa(port)+"-"+sb.ID+"-") || !strings.HasSuffix(u.Hostname(), ".127-0-0-1.nip.io") || u.Port() != strings.Split(e.srv.Listener.Addr().String(), ":")[1] {
		t.Fatalf("preview link %q", link.URL)
	}
	viaHost := func(host, path string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	// Served at "/" on its own origin, without the API token, and the app
	// never sees the console session.
	resp, body := viaHost(u.Host, "/a/b?x=1", map[string]string{"Cookie": cookie + "; theme=dark"})
	if resp.StatusCode != 200 || body != "/a/b cookie=theme=dark" {
		t.Fatalf("preview: %d %q", resp.StatusCode, body)
	}
	// Another port's signature (or a forged one) falls through to the API,
	// which wants a token.
	bad := strings.Replace(u.Host, "p"+strconv.Itoa(port)+"-", "p"+strconv.Itoa(port+1)+"-", 1)
	if resp, _ := viaHost(bad, "/v1/host", nil); resp.StatusCode != 401 {
		t.Fatalf("preview with another port's signature: %d", resp.StatusCode)
	}
}

func TestPreviewDomain(t *testing.T) {
	e := newEnv(t, t.TempDir(), func(c *config.Config) { c.UI = true; c.PreviewDomain = "preview.test" })
	defer e.close("")
	req, _ := http.NewRequest("POST", e.srv.URL+"/ui/preview-link", strings.NewReader(`{"sandbox":"nope","port":3000}`))
	req.Header.Set("X-Fired-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown sandbox: %d", resp.StatusCode)
	}
	e.template("base", false)
	sb := must(e.c.CreateSandbox(context.Background(), api.CreateSandboxRequest{Template: "base"}))(t)
	req, _ = http.NewRequest("POST", e.srv.URL+"/ui/preview-link", strings.NewReader(`{"sandbox":"`+sb.ID+`","port":3000}`))
	req.Header.Set("X-Fired-Token", token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var link struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&link)
	if u, err := url.Parse(link.URL); err != nil || !strings.HasSuffix(u.Hostname(), ".preview.test") {
		t.Fatalf("preview link %q", link.URL)
	}
}

func TestHeaderProxy(t *testing.T) {
	e := newEnv(t, t.TempDir(), nil)
	defer e.close("")
	ctx := context.Background()
	e.template("base", false)
	sb := must(e.c.CreateSandbox(ctx, api.CreateSandboxRequest{Template: "base"}))(t)
	port := freePort(t)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	app := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Host+" "+r.URL.RequestURI()+" leaked="+r.Header.Get("X-Fired-Sandbox")+r.Header.Get("X-Fired-Port")+r.Header.Get("X-Fired-Token"))
	})}
	go app.Serve(ln)
	defer app.Close()

	send := func(hdr map[string]string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", e.srv.URL+"/some/page?x=1", nil)
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
	route := map[string]string{"X-Fired-Sandbox": sb.ID, "X-Fired-Port": strconv.Itoa(port), "X-Forwarded-Host": "p3000-x.preview.example.com"}
	if code, _ := send(route); code != 401 {
		t.Fatalf("without token: %d", code)
	}
	route["X-Fired-Token"] = token
	if code, _ := send(route); code != 403 {
		t.Fatalf("port not exposed: %d", code)
	}
	if err := e.c.ExposePort(ctx, sb.ID, port); err != nil {
		t.Fatal(err)
	}
	if code, body := send(route); code != 200 || body != "p3000-x.preview.example.com /some/page?x=1 leaked=" {
		t.Fatalf("header proxy: %d %q", code, body)
	}
	route["X-Fired-Port"] = "nope"
	if code, _ := send(route); code != 400 {
		t.Fatalf("bad port: %d", code)
	}
}
