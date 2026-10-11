package artifacts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"

	"github.com/Pushpavel/fired/pkg/api"
	"github.com/Pushpavel/fired/pkg/client"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// pullConcurrency is how many files are downloaded from fired at once.
const pullConcurrency = 8

// pullSandbox resolves a from_sandbox op: it asks fired to scan the source
// for the sandbox's secret values (fired hashes the same bytes it scans),
// downloads only the blobs the store is missing, and checks each download
// against the scanned hash. So what is published is exactly what was scanned:
// a file that changed during the publish fails it instead of slipping in.
//
// The result clears dst ("" = the whole tree) and then adds every file, so
// the folder is mirrored.
func (s *Service) pullSandbox(ctx context.Context, i int, dst string, src pa.SandboxSource) ([]resolvedOp, error) {
	if src.ID == "" || src.Path == "" {
		return nil, badRequest("files[%d]: from_sandbox needs id and path", i)
	}
	fc := s.fired
	if src.FiredURL != "" && src.FiredURL != s.cfg.Fired.URL {
		u, err := url.Parse(src.FiredURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, badRequest("files[%d]: from_sandbox.fired_url must be an http(s) URL", i)
		}
		fc = client.New(src.FiredURL, s.cfg.Fired.Token)
	}
	where := fmt.Sprintf("files[%d]: sandbox %s", i, src.ID)
	scan, err := fc.ScanFiles(ctx, src.ID, src.Path, src.Exclude...)
	if err != nil {
		return nil, firedErr(where, err)
	}
	if n := len(scan.Found); n > 0 {
		f := scan.Found[0]
		more := ""
		if n > 1 {
			more = fmt.Sprintf(" (and %d more files)", n-1)
		}
		return nil, errf(http.StatusUnprocessableEntity, "secret_found",
			"%s: %s contains the secret %s%s; remove it or exclude the file", where, path.Join(scan.Path, f.Path), f.Secret, more)
	}
	if len(scan.Files) > s.cfg.Limits.MaxFilesPerVersion {
		return nil, errf(http.StatusRequestEntityTooLarge, "too_many_files", "%s: %s has %d files; a version can have at most %d",
			where, scan.Path, len(scan.Files), s.cfg.Limits.MaxFilesPerVersion)
	}
	var total int64
	hashes := make([]string, 0, len(scan.Files))
	ops := []resolvedOp{{path: dst, clear: true}}
	for _, f := range scan.Files {
		if !validHash(f.SHA256) {
			return nil, errf(http.StatusBadGateway, "fired_error", "%s: bad hash for %s", where, f.Path)
		}
		p := f.Path
		if dst != "" {
			p = dst + "/" + f.Path
		}
		cp, err := cleanPath(p)
		if err != nil {
			return nil, badRequest("%s: %v", where, err)
		}
		mode := uint32(0o644)
		if m, err := strconv.ParseUint(f.Mode, 8, 32); err == nil && m&0o111 != 0 {
			mode = 0o755
		}
		total += f.Size
		hashes = append(hashes, f.SHA256)
		ops = append(ops, resolvedOp{path: cp, file: pa.File{Path: cp, Hash: f.SHA256, Size: f.Size, Mode: mode}})
	}
	if total > s.cfg.Limits.MaxVersionBytes {
		return nil, errf(http.StatusRequestEntityTooLarge, "too_large", "%s: %s is %d bytes; a version can be at most %d",
			where, scan.Path, total, s.cfg.Limits.MaxVersionBytes)
	}
	missing, err := s.MissingBlobs(hashes)
	if err != nil {
		return nil, err
	}
	byHash := map[string]api.ManifestEntry{}
	for _, f := range scan.Files {
		byHash[f.SHA256] = f
	}
	if err := s.download(ctx, fc, src.ID, scan.Base, missing, byHash, where); err != nil {
		return nil, err
	}
	return ops, nil
}

// download fetches the missing blobs from the sandbox, a few at a time.
func (s *Service) download(ctx context.Context, fc *client.Client, sb, base string, missing []string,
	byHash map[string]api.ManifestEntry, where string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan string)
	var once sync.Once
	var first error
	fail := func(err error) {
		once.Do(func() { first = err; cancel() })
	}
	var wg sync.WaitGroup
	for range min(pullConcurrency, len(missing)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range work {
				f := byHash[h]
				changed := errf(http.StatusConflict, "changed",
					"%s: %s changed while publishing; publish again", where, path.Join(base, f.Path))
				rc, err := fc.OpenFile(ctx, sb, path.Join(base, f.Path))
				if client.IsNotFound(err) {
					fail(changed)
					continue
				}
				if err != nil {
					fail(firedErr(where, err))
					continue
				}
				var got string
				got, _, err = s.putContent(ctx, rc, s.cfg.Limits.MaxVersionBytes, h)
				rc.Close()
				if err == nil && got != h {
					err = ErrHashMismatch
				}
				switch {
				case errors.Is(err, ErrHashMismatch):
					fail(changed)
				case err != nil:
					fail(err)
				}
			}
		}()
	}
	for _, h := range missing {
		select {
		case work <- h:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	return first
}

// firedErr turns an error from a fired call into an API error.
func firedErr(where string, err error) error {
	var he *client.HTTPError
	if errors.As(err, &he) {
		if he.Status == http.StatusNotFound {
			return errf(http.StatusNotFound, "not_found", "%s: %s", where, he.Body.Err.Message)
		}
		return errf(http.StatusBadGateway, "fired_error", "%s: fired returned %d %s: %s", where, he.Status, he.Body.Err.Code, he.Body.Err.Message)
	}
	return errf(http.StatusBadGateway, "fired_error", "%s: %v", where, err)
}
