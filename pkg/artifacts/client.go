package artifacts

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Pushpavel/fired/pkg/api"
	"github.com/Pushpavel/fired/pkg/client"
)

// Client is a Go client for the fired-artifacts API.
//
//	c := artifacts.NewClient("http://artifacts:8444", token)
//	res, _ := c.Create(ctx, artifacts.CreateArtifactRequest{QuotaKey: "user-42",
//		Publish: &artifacts.PublishRequest{Files: []artifacts.FileOp{artifacts.Text("index.html", "<h1>hi</h1>")}}})
type Client struct {
	base  string
	token string
	HTTP  *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, HTTP: &http.Client{}}
}

// Text is a FileOp with inline text content.
func Text(path, content string) FileOp { return FileOp{Path: path, Content: &content} }

// HashBytes returns the blob hash of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Client) req(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		r.Header.Set("X-Fired-Token", c.token)
	}
	return r, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := c.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeErr(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Errors are *client.HTTPError, shared with the fired client.
func decodeErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	e := &client.HTTPError{Status: resp.StatusCode}
	if json.Unmarshal(b, &e.Body) != nil || e.Body.Err.Code == "" {
		e.Body.Err = api.ErrorBody{Code: "http_error", Message: strings.TrimSpace(string(b))}
	}
	return e
}

func ap(id string) string { return "/v1/artifacts/" + url.PathEscape(id) }

// ---- blobs

func (c *Client) MissingBlobs(ctx context.Context, hashes []string) ([]string, error) {
	var out MissingBlobsResult
	return out.Missing, c.do(ctx, "POST", "/v1/blobs/missing", MissingBlobsRequest{Hashes: hashes}, &out)
}

func (c *Client) PutBlob(ctx context.Context, hash string, content io.Reader) error {
	r, err := c.req(ctx, "PUT", "/v1/blobs/"+url.PathEscape(hash), content)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeErr(resp)
	}
	return nil
}

// Upload stores data as a blob if the service doesn't have it yet and
// returns a FileOp referring to it.
func (c *Client) Upload(ctx context.Context, path string, data []byte, mode uint32) (FileOp, error) {
	h := HashBytes(data)
	missing, err := c.MissingBlobs(ctx, []string{h})
	if err != nil {
		return FileOp{}, err
	}
	if len(missing) > 0 {
		if err := c.PutBlob(ctx, h, bytes.NewReader(data)); err != nil {
			return FileOp{}, err
		}
	}
	return FileOp{Path: path, Hash: h, Mode: mode}, nil
}

// ---- artifacts

func (c *Client) Create(ctx context.Context, req CreateArtifactRequest) (*CreateArtifactResult, error) {
	var out CreateArtifactResult
	return &out, c.do(ctx, "POST", "/v1/artifacts", req, &out)
}

func (c *Client) Get(ctx context.Context, id string) (*Artifact, error) {
	var out Artifact
	return &out, c.do(ctx, "GET", ap(id), nil, &out)
}

func (c *Client) List(ctx context.Context, quotaKey string, labels map[string]string) ([]Artifact, error) {
	q := url.Values{}
	if quotaKey != "" {
		q.Set("quota_key", quotaKey)
	}
	for k, v := range labels {
		q.Add("label", k+"="+v)
	}
	var out ArtifactList
	return out.Artifacts, c.do(ctx, "GET", "/v1/artifacts?"+q.Encode(), nil, &out)
}

func (c *Client) Update(ctx context.Context, id string, req UpdateArtifactRequest) (*Artifact, error) {
	var out Artifact
	return &out, c.do(ctx, "PATCH", ap(id), req, &out)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", ap(id), nil, nil)
}

func (c *Client) Publish(ctx context.Context, id string, req PublishRequest) (*PublishResult, error) {
	var out PublishResult
	return &out, c.do(ctx, "POST", ap(id)+"/versions", req, &out)
}

func (c *Client) Versions(ctx context.Context, id string) ([]Version, error) {
	var out VersionList
	return out.Versions, c.do(ctx, "GET", ap(id)+"/versions", nil, &out)
}

// Version returns a version with its file list; n = 0 means current.
func (c *Client) Version(ctx context.Context, id string, n int) (*Version, error) {
	v := "current"
	if n > 0 {
		v = strconv.Itoa(n)
	}
	var out Version
	return &out, c.do(ctx, "GET", ap(id)+"/versions/"+v, nil, &out)
}

func (c *Client) Restore(ctx context.Context, id string, req RestoreRequest) (*PublishResult, error) {
	var out PublishResult
	return &out, c.do(ctx, "POST", ap(id)+"/restore", req, &out)
}

func (c *Client) Fork(ctx context.Context, id string, req ForkRequest) (*Artifact, error) {
	var out Artifact
	return &out, c.do(ctx, "POST", ap(id)+"/fork", req, &out)
}

// OpenFile streams one file; version 0 means current.
func (c *Client) OpenFile(ctx context.Context, id, path string, version int) (io.ReadCloser, error) {
	p := ap(id) + "/files/" + escapePath(path)
	if version > 0 {
		p += "?version=" + strconv.Itoa(version)
	}
	r, err := c.req(ctx, "GET", p, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeErr(resp)
	}
	return resp.Body, nil
}

func (c *Client) ReadFile(ctx context.Context, id, path string, version int) ([]byte, error) {
	rc, err := c.OpenFile(ctx, id, path, version)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// ---- jobs and runs

func (c *Client) Jobs(ctx context.Context, id string) ([]JobStatus, error) {
	var out JobList
	return out.Jobs, c.do(ctx, "GET", ap(id)+"/jobs", nil, &out)
}

func (c *Client) Grant(ctx context.Context, id, job string, g Grant) (*JobStatus, error) {
	var out JobStatus
	return &out, c.do(ctx, "PUT", ap(id)+"/jobs/"+url.PathEscape(job)+"/grant", g, &out)
}

func (c *Client) Trigger(ctx context.Context, id, job string, payload json.RawMessage) (*Run, error) {
	var out Run
	return &out, c.do(ctx, "POST", ap(id)+"/jobs/"+url.PathEscape(job)+"/runs", TriggerRequest{Payload: payload}, &out)
}

func (c *Client) Runs(ctx context.Context, id, job, status string) ([]Run, error) {
	q := url.Values{}
	if job != "" {
		q.Set("job", job)
	}
	if status != "" {
		q.Set("status", status)
	}
	var out RunList
	return out.Runs, c.do(ctx, "GET", ap(id)+"/runs?"+q.Encode(), nil, &out)
}

func (c *Client) Run(ctx context.Context, id, run string) (*Run, error) {
	var out Run
	return &out, c.do(ctx, "GET", ap(id)+"/runs/"+url.PathEscape(run), nil, &out)
}

func (c *Client) RunLogs(ctx context.Context, id, run string) (*RunLogs, error) {
	var out RunLogs
	return &out, c.do(ctx, "GET", ap(id)+"/runs/"+url.PathEscape(run)+"/logs", nil, &out)
}

// ---- quotas

func (c *Client) Quota(ctx context.Context, key string) (*QuotaStatus, error) {
	var out QuotaStatus
	return &out, c.do(ctx, "GET", "/v1/quotas/"+url.PathEscape(key), nil, &out)
}

func (c *Client) SetQuota(ctx context.Context, key string, q Quota) (*QuotaStatus, error) {
	var out QuotaStatus
	return &out, c.do(ctx, "PUT", "/v1/quotas/"+url.PathEscape(key), q, &out)
}

// ---- events

// Events streams events until ctx is done. artifact filters to one artifact
// ("" = all).
func (c *Client) Events(ctx context.Context, artifact string, fn func(Event)) error {
	p := "/v1/events"
	if artifact != "" {
		p += "?artifact=" + url.QueryEscape(artifact)
	}
	r, err := c.req(ctx, "GET", p, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeErr(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var e Event
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				return fmt.Errorf("decode event: %v", err)
			}
			fn(e)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}

// Tiers lists the tier definitions (storage and schedule limits). Which tier
// a quota key is in is set on fired.
func (c *Client) Tiers(ctx context.Context) ([]Tier, error) {
	var l TierList
	return l.Tiers, c.do(ctx, "GET", "/v1/tiers", nil, &l)
}

// PutTier creates or replaces a tier definition.
func (c *Client) PutTier(ctx context.Context, t Tier) (*Tier, error) {
	var out Tier
	return &out, c.do(ctx, "PUT", "/v1/tiers/"+url.PathEscape(t.Name), t, &out)
}

// DeleteTier removes a tier definition.
func (c *Client) DeleteTier(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1/tiers/"+url.PathEscape(name), nil, nil)
}
