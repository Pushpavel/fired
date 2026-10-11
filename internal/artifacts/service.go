// Package artifacts implements fired-artifacts: a store of versioned file
// trees ("artifacts") with views and scheduled jobs that run in fired
// sandboxes. It knows nothing about users: callers group artifacts under an
// opaque quota key, and a credentials broker (your backend) supplies
// secrets for each run. See docs/artifacts.md.
package artifacts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/Pushpavel/fired/pkg/client"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

type Service struct {
	cfg    Config
	st     *store
	blobs  *Blobs
	bus    *bus
	log    *slog.Logger
	fired  *client.Client
	broker *broker
	now    func() time.Time

	mu      sync.Mutex
	running map[string]string // artifact → run id
	buckets map[string]*bucket
	touched map[string]time.Time
	// quotaWait: until when a quota key's runs wait, after fired refused
	// one for that key's limits.
	quotaWait map[string]time.Time
	// tiers caches fired's quota key → tier mapping.
	tiers    map[string]tierEntry
	wake     chan struct{}
	runsWG   sync.WaitGroup
	stopOnce sync.Once
	stop     chan struct{}
	bgWG     sync.WaitGroup
}

// New opens the store under cfg.DataDir.
func New(cfg Config, log *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	blobs, err := OpenBlobs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Service{
		cfg: cfg, st: st, blobs: blobs, bus: newBus(), log: log,
		fired:   client.New(cfg.Fired.URL, cfg.Fired.Token),
		broker:  newBroker(cfg.Broker),
		now:     func() time.Time { return time.Now().UTC() },
		running: map[string]string{}, buckets: map[string]*bucket{}, touched: map[string]time.Time{},
		quotaWait: map[string]time.Time{}, tiers: map[string]tierEntry{},
		wake: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	return s, nil
}

// Close stops background work, waits for runs in flight, and closes the store.
func (s *Service) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.bgWG.Wait()
	s.runsWG.Wait()
	return s.st.close()
}

func (s *Service) emit(e pa.Event) {
	if e.At.IsZero() {
		e.At = s.now()
	}
	s.bus.publish(e)
}

// ---- artifacts

// Create makes an artifact together with its first version, in one
// transaction: an invalid publish leaves nothing behind.
func (s *Service) Create(ctx context.Context, req pa.CreateArtifactRequest) (*pa.CreateArtifactResult, error) {
	if req.QuotaKey == "" {
		return nil, badRequest("quota_key is required")
	}
	if req.Publish == nil {
		return nil, badRequest("publish is required: an artifact is created with its first version")
	}
	if req.Publish.BaseVersion != 0 {
		return nil, badRequest("publish.base_version must be 0 (or left out) when creating an artifact")
	}
	id := req.ID
	if id == "" {
		id = newID("art-")
	} else if !idRE.MatchString(id) {
		return nil, badRequest("id must match %s", idRE)
	}
	if len(req.Meta) > 0 && !json.Valid(req.Meta) {
		return nil, badRequest("meta must be valid JSON")
	}
	prep, err := s.preparePublish(ctx, *req.Publish)
	if err != nil {
		return nil, err
	}
	s.tierOf(ctx, req.QuotaKey)
	now := s.now()
	a := &pa.Artifact{ID: id, QuotaKey: req.QuotaKey, Labels: req.Labels, Meta: req.Meta, DeleteAfter: req.DeleteAfter,
		RetainRuns: req.RetainRuns, CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
	var v *pa.Version
	err = s.st.tx(ctx, func(q querier) error {
		if _, err := getArtifact(ctx, q, id); err == nil {
			return errf(http.StatusConflict, "exists", "artifact %s already exists", id)
		}
		if err := putArtifact(ctx, q, a); err != nil {
			return err
		}
		var err error
		v, _, err = s.commitEdit(ctx, q, a, *req.Publish, prep, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.emit(pa.Event{Type: "artifact.created", Artifact: id})
	res, err := s.afterPublish(ctx, id, v, false, *req.Publish)
	if err != nil {
		return nil, err
	}
	return &pa.CreateArtifactResult{Artifact: *a, Publish: *res}, nil
}

func (s *Service) Get(ctx context.Context, id string) (*pa.Artifact, error) {
	a, err := getArtifact(ctx, s.st.db, id)
	if err == errNotFound {
		return nil, errf(http.StatusNotFound, "not_found", "artifact %s not found", id)
	}
	return a, err
}

// List returns artifacts, optionally filtered by quota key and labels.
func (s *Service) List(ctx context.Context, quotaKey string, labels map[string]string) ([]pa.Artifact, error) {
	all, err := listArtifacts(ctx, s.st.db, quotaKey)
	if err != nil {
		return nil, err
	}
	out := []pa.Artifact{}
	for _, a := range all {
		ok := true
		for k, v := range labels {
			if a.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, id string, req pa.UpdateArtifactRequest) (*pa.Artifact, error) {
	if len(req.Meta) > 0 && !json.Valid(req.Meta) {
		return nil, badRequest("meta must be valid JSON")
	}
	var out *pa.Artifact
	err := s.st.tx(ctx, func(q querier) error {
		a, err := getArtifact(ctx, q, id)
		if err != nil {
			return s.notFound(err, id)
		}
		if req.QuotaKey != nil {
			if *req.QuotaKey == "" {
				return badRequest("quota_key can't be empty")
			}
			a.QuotaKey = *req.QuotaKey
		}
		if req.Labels != nil {
			labels := make(map[string]string, len(a.Labels)+len(req.Labels))
			for k, v := range a.Labels {
				labels[k] = v
			}
			for k, v := range req.Labels {
				if v == nil {
					delete(labels, k)
				} else {
					labels[k] = *v
				}
			}
			if len(labels) == 0 {
				labels = nil
			}
			a.Labels = labels
		}
		if len(req.Meta) > 0 {
			meta, err := mergePatch(a.Meta, req.Meta)
			if err != nil {
				return err
			}
			a.Meta = meta
		}
		if req.DeleteAfter != nil {
			a.DeleteAfter = *req.DeleteAfter
		}
		if req.RetainRuns != nil {
			a.RetainRuns = *req.RetainRuns
		}
		a.UpdatedAt = s.now()
		out = a
		return putArtifact(ctx, q, a)
	})
	return out, err
}

// mergePatch applies a JSON merge patch (RFC 7386) to doc: object keys in
// the patch are set, null ones removed, recursively; a patch that isn't an
// object replaces doc. A result of null is returned as nil.
func mergePatch(doc, patch json.RawMessage) (json.RawMessage, error) {
	var d, p any
	if len(doc) > 0 {
		if err := decodeJSON(doc, &d); err != nil {
			return nil, err
		}
	}
	if err := decodeJSON(patch, &p); err != nil {
		return nil, badRequest("meta must be valid JSON")
	}
	out := mergeValue(d, p)
	if out == nil {
		return nil, nil
	}
	return json.Marshal(out)
}

// decodeJSON keeps numbers as written, so merging doesn't round them.
func decodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

func mergeValue(doc, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	d, ok := doc.(map[string]any)
	if !ok {
		d = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(d, k)
		} else {
			d[k] = mergeValue(d[k], v)
		}
	}
	return d
}

func (s *Service) notFound(err error, id string) error {
	if err == errNotFound {
		return errf(http.StatusNotFound, "not_found", "artifact %s not found", id)
	}
	return err
}

// Delete removes an artifact, its versions, jobs and runs, and its runner
// sandbox. Blobs no longer referenced are removed by the next GC.
func (s *Service) Delete(ctx context.Context, id string) error {
	err := s.st.tx(ctx, func(q querier) error {
		if _, err := getArtifact(ctx, q, id); err != nil {
			return s.notFound(err, id)
		}
		return deleteArtifactRows(ctx, q, id)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.buckets, id)
	delete(s.touched, id)
	s.mu.Unlock()
	go s.removeRunner(context.Background(), id)
	s.emit(pa.Event{Type: "artifact.deleted", Artifact: id})
	return nil
}

// ---- versions

func (s *Service) Version(ctx context.Context, id string, n int, withFiles bool) (*pa.Version, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		n = a.CurrentVersion
	}
	v, err := getVersion(ctx, s.st.db, id, n)
	if err == errNotFound {
		return nil, errf(http.StatusNotFound, "not_found", "artifact %s has no version %d", id, n)
	}
	if err != nil {
		return nil, err
	}
	if withFiles {
		tree, err := getTree(ctx, s.st.db, id, n)
		if err != nil {
			return nil, err
		}
		v.Files = sortedFiles(tree)
	}
	return v, nil
}

func (s *Service) Versions(ctx context.Context, id string) ([]pa.Version, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	vs, err := listVersions(ctx, s.st.db, id)
	if vs == nil {
		vs = []pa.Version{}
	}
	return vs, err
}

func sortedFiles(tree map[string]pa.File) []pa.File {
	out := make([]pa.File, 0, len(tree))
	for _, f := range tree {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

type resolvedOp struct {
	path   string
	delete bool
	// clear removes everything under path ("" = the whole tree), so a
	// from_sandbox folder is mirrored.
	clear bool
	file  pa.File
}

// resolveOps validates file ops and stores inline content as blobs.
func (s *Service) resolveOps(ctx context.Context, req pa.PublishRequest) ([]resolvedOp, error) {
	var ops []resolvedOp
	var inline int64
	for i, op := range req.Files {
		set := 0
		for _, b := range []bool{op.Hash != "", op.Content != nil, op.ContentBase64 != "", op.Delete, op.FromSandbox != nil} {
			if b {
				set++
			}
		}
		if set != 1 {
			return nil, badRequest("files[%d] (%s): set exactly one of hash, content, content_base64, delete or from_sandbox", i, op.Path)
		}
		if op.FromSandbox != nil {
			// path is the folder to mirror into; "" or "." is the root.
			dst := ""
			if op.Path != "" && op.Path != "." {
				var err error
				if dst, err = cleanPath(op.Path); err != nil {
					return nil, badRequest("files[%d]: %v", i, err)
				}
			}
			if op.Mode != 0 {
				return nil, badRequest("files[%d] (%s): from_sandbox takes modes from the sandbox; leave mode out", i, op.Path)
			}
			pulled, err := s.pullSandbox(ctx, i, dst, *op.FromSandbox)
			if err != nil {
				return nil, err
			}
			ops = append(ops, pulled...)
			continue
		}
		p, err := cleanPath(op.Path)
		if err != nil {
			return nil, badRequest("files[%d]: %v", i, err)
		}
		mode := op.Mode
		switch mode {
		case 0:
			mode = 0o644
		case 0o644, 0o755:
		default:
			return nil, badRequest("files[%d] (%s): mode must be 0644 (420) or 0755 (493)", i, p)
		}
		if op.Delete {
			ops = append(ops, resolvedOp{path: p, delete: true})
			continue
		}
		var data []byte
		switch {
		case op.Hash != "":
			if !validHash(op.Hash) {
				return nil, badRequest("files[%d] (%s): %v", i, p, errBadHash)
			}
			if !s.blobs.Has(op.Hash) {
				return nil, errf(http.StatusUnprocessableEntity, "missing_blob", "files[%d] (%s): blob %s has not been uploaded", i, p, op.Hash)
			}
			size, err := s.blobSize(op.Hash)
			if err != nil {
				return nil, err
			}
			ops = append(ops, resolvedOp{path: p, file: pa.File{Path: p, Hash: op.Hash, Size: size, Mode: mode}})
			continue
		case op.Content != nil:
			data = []byte(*op.Content)
		default:
			data, err = base64.StdEncoding.DecodeString(op.ContentBase64)
			if err != nil {
				return nil, badRequest("files[%d] (%s): content_base64: %v", i, p, err)
			}
		}
		inline += int64(len(data))
		if inline > s.cfg.Limits.MaxInlineBytes {
			return nil, errf(http.StatusRequestEntityTooLarge, "too_large", "inline content exceeds %d bytes; upload large files as blobs", s.cfg.Limits.MaxInlineBytes)
		}
		h, err := s.blobs.PutBytes(data)
		if err != nil {
			return nil, err
		}
		ops = append(ops, resolvedOp{path: p, file: pa.File{Path: p, Hash: h, Size: int64(len(data)), Mode: mode}})
	}
	return ops, nil
}

// blobSize reads a blob's length (the store only keeps compressed bytes).
func (s *Service) blobSize(h string) (int64, error) {
	r, err := s.blobs.Open(h)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	var n int64
	buf := make([]byte, 64<<10)
	for {
		k, err := r.Read(buf)
		n += int64(k)
		if err != nil {
			break
		}
		if n > s.cfg.Limits.MaxVersionBytes {
			return 0, errf(http.StatusRequestEntityTooLarge, "too_large", "blob %s is larger than max_version_bytes", h)
		}
	}
	return n, nil
}

// specFor reads and validates the spec of a tree.
func (s *Service) specFor(tree map[string]pa.File) (pa.Spec, error) {
	f, ok := tree[pa.SpecFile]
	if !ok {
		return DefaultSpec(tree), nil
	}
	data, err := s.blobs.ReadAll(f.Hash, 1<<20)
	if err != nil {
		return pa.Spec{}, badRequest("%s: %v", pa.SpecFile, err)
	}
	spec, err := ParseSpec(data)
	if err != nil {
		return spec, badRequest("%v", err)
	}
	if err := ValidateSpec(&spec, tree); err != nil {
		return spec, badRequest("%v", err)
	}
	return spec, nil
}

// ensureSpecFile makes sure tree has an artifact.json, writing spec (or,
// when nil, the default for the tree) if it has none, and sets the title
// and description given.
func (s *Service) ensureSpecFile(tree map[string]pa.File, spec *pa.Spec, title, description *string) error {
	f, ok := tree[pa.SpecFile]
	if ok && title == nil && description == nil {
		return nil
	}
	var sp pa.Spec
	switch {
	case ok:
		data, err := s.blobs.ReadAll(f.Hash, 1<<20)
		if err != nil {
			return badRequest("%s: %v", pa.SpecFile, err)
		}
		if sp, err = ParseSpec(data); err != nil {
			return badRequest("%v", err)
		}
	case spec != nil:
		sp = *spec
	default:
		sp = DefaultSpec(tree)
	}
	if title != nil {
		sp.Title = *title
	}
	if description != nil {
		sp.Description = *description
	}
	data := EncodeSpec(sp)
	h, err := s.blobs.PutBytes(data)
	if err != nil {
		return err
	}
	tree[pa.SpecFile] = pa.File{Path: pa.SpecFile, Hash: h, Size: int64(len(data)), Mode: 0o644}
	return nil
}

// setCurrent makes v the artifact's current version, and its title and
// description the artifact's.
func setCurrent(a *pa.Artifact, v *pa.Version) {
	a.CurrentVersion, a.Title, a.Description = v.Version, v.Spec.Title, v.Spec.Description
}

// checkTree enforces per-version limits and the quota key's storage limit.
func (s *Service) checkTree(ctx context.Context, q querier, key string, tree map[string]pa.File) (int64, error) {
	if len(tree) > s.cfg.Limits.MaxFilesPerVersion {
		return 0, errf(http.StatusRequestEntityTooLarge, "too_many_files", "a version can have at most %d files (got %d)", s.cfg.Limits.MaxFilesPerVersion, len(tree))
	}
	var total int64
	hashes := map[string]int64{}
	for _, f := range tree {
		total += f.Size
		hashes[f.Hash] = f.Size
	}
	if total > s.cfg.Limits.MaxVersionBytes {
		return 0, errf(http.StatusRequestEntityTooLarge, "too_large", "a version can be at most %d bytes (got %d)", s.cfg.Limits.MaxVersionBytes, total)
	}
	quota, err := s.effectiveQuota(ctx, q, key)
	if err != nil {
		return 0, err
	}
	if quota.StorageBytes > 0 {
		known, err := knownHashes(ctx, q, key, hashes)
		if err != nil {
			return 0, err
		}
		var added int64
		for h, sz := range hashes {
			if !known[h] {
				added += sz
			}
		}
		if added > 0 {
			used, err := storageUsed(ctx, q, key)
			if err != nil {
				return 0, err
			}
			if used+added > quota.StorageBytes {
				return 0, errf(http.StatusRequestEntityTooLarge, "quota_exceeded", "storage quota exceeded: %d used + %d new > %d", used, added, quota.StorageBytes)
			}
		}
	}
	return total, nil
}

func treesEqual(a, b map[string]pa.File) bool {
	if len(a) != len(b) {
		return false
	}
	for p, f := range a {
		g, ok := b[p]
		if !ok || g.Hash != f.Hash || g.Mode != f.Mode {
			return false
		}
	}
	return true
}

// Publish creates a new version on top of req.BaseVersion.
// publishPrep is a publish request checked and resolved outside the
// transaction (blob lookups, the spec file).
type publishPrep struct {
	ops         []resolvedOp
	specHash    string
	specSize    int64
	replaceDirs []string
}

func (s *Service) preparePublish(ctx context.Context, req pa.PublishRequest) (*publishPrep, error) {
	ops, err := s.resolveOps(ctx, req)
	if err != nil {
		return nil, err
	}
	p := &publishPrep{ops: ops, replaceDirs: make([]string, 0, len(req.ReplaceDirs))}
	for _, op := range ops {
		if op.path == pa.SpecFile && op.delete {
			return nil, badRequest("%s can't be deleted: every version has one (publish a new one to change it)", pa.SpecFile)
		}
	}
	if req.Spec != nil {
		for _, op := range ops {
			if op.path == pa.SpecFile {
				return nil, badRequest("pass the spec either as \"spec\" or as an %s file, not both", pa.SpecFile)
			}
		}
		data := EncodeSpec(*req.Spec)
		if p.specHash, err = s.blobs.PutBytes(data); err != nil {
			return nil, err
		}
		p.specSize = int64(len(data))
	}
	for i, d := range req.ReplaceDirs {
		c, err := cleanPath(d)
		if err != nil {
			return nil, badRequest("replace_dirs[%d]: %v", i, err)
		}
		p.replaceDirs = append(p.replaceDirs, c)
	}
	for _, j := range req.RunNow {
		if !jobNameRE.MatchString(j) {
			return nil, badRequest("run_now: %q is not a job name", j)
		}
	}
	return p, nil
}

func (s *Service) Publish(ctx context.Context, id string, req pa.PublishRequest) (*pa.PublishResult, error) {
	prep, err := s.preparePublish(ctx, req)
	if err != nil {
		return nil, err
	}
	if a, err := s.Get(ctx, id); err == nil {
		s.tierOf(ctx, a.QuotaKey)
	}
	now := s.now()
	var v *pa.Version
	var unchanged bool
	err = s.st.tx(ctx, func(q querier) error {
		a, err := getArtifact(ctx, q, id)
		if err != nil {
			return s.notFound(err, id)
		}
		if err := checkBase(req.BaseVersion, a.CurrentVersion); err != nil {
			return err
		}
		v, unchanged, err = s.commitEdit(ctx, q, a, req, prep, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.afterPublish(ctx, id, v, unchanged, req)
}

// checkBase checks that a publish or restore is on top of the current
// version. Every artifact has a version, so 0 means base_version was left out.
func checkBase(base, current int) error {
	if base == 0 {
		return badRequest("base_version is required: the version your change is on top of (current is %d)", current)
	}
	if base != current {
		return errf(http.StatusConflict, "conflict", "base_version %d is not the current version (current is %d); read it, merge your changes and publish again", base, current)
	}
	return nil
}

// commitEdit applies a prepared publish on top of a's current version and
// makes it current. A version always has at least one file. When the result
// has the current version's files, nothing is written (beyond notify) and
// it returns the current version with unchanged set.
func (s *Service) commitEdit(ctx context.Context, q querier, a *pa.Artifact, req pa.PublishRequest, p *publishPrep, now time.Time) (v *pa.Version, unchanged bool, err error) {
	id := a.ID
	cur := map[string]pa.File{}
	if a.CurrentVersion > 0 {
		if cur, err = getTree(ctx, q, id, a.CurrentVersion); err != nil {
			return nil, false, err
		}
	}
	tree := map[string]pa.File{}
	if !req.Replace {
		for p, f := range cur {
			tree[p] = f
		}
	}
	for _, d := range p.replaceDirs {
		for path := range tree {
			if under(path, d) {
				delete(tree, path)
			}
		}
	}
	for _, op := range p.ops {
		if op.clear {
			for path := range tree {
				if op.path == "" || under(path, op.path) {
					delete(tree, path)
				}
			}
			continue
		}
		if op.delete {
			delete(tree, op.path)
			continue
		}
		tree[op.path] = op.file
	}
	if p.specHash != "" {
		tree[pa.SpecFile] = pa.File{Path: pa.SpecFile, Hash: p.specHash, Size: p.specSize, Mode: 0o644}
	}
	if len(tree) == 0 {
		return nil, false, badRequest("a version needs at least one file")
	}
	if _, ok := tree[pa.SpecFile]; !ok {
		if f, ok := cur[pa.SpecFile]; ok {
			tree[pa.SpecFile] = f // left out (replace, a mirrored folder): kept
		}
	}
	if err := s.ensureSpecFile(tree, nil, req.Title, req.Description); err != nil {
		return nil, false, err
	}
	if a.CurrentVersion > 0 && treesEqual(cur, tree) {
		if v, err = getVersion(ctx, q, id, a.CurrentVersion); err != nil {
			return nil, false, err
		}
		if err := checkRunNow(req.RunNow, v.Spec); err != nil {
			return nil, false, err
		}
		if req.Notify != "" {
			if err := setNotify(ctx, q, id, v.Version, req.Notify); err != nil {
				return nil, false, err
			}
		}
		return v, true, nil
	}
	if err := checkNoFileDirClash(tree); err != nil {
		return nil, false, err
	}
	spec, err := s.specFor(tree)
	if err != nil {
		return nil, false, err
	}
	size, err := s.checkTree(ctx, q, a.QuotaKey, tree)
	if err != nil {
		return nil, false, err
	}
	n, err := maxVersion(ctx, q, id)
	if err != nil {
		return nil, false, err
	}
	if err := checkRunNow(req.RunNow, spec); err != nil {
		return nil, false, err
	}
	v = &pa.Version{Artifact: id, Version: n + 1, Parent: a.CurrentVersion, Origin: pa.OriginEdit, Label: req.Label,
		Labels: req.Labels, FileCount: len(tree), SizeBytes: size, Spec: spec, CreatedAt: now}
	if err := insertVersion(ctx, q, v, tree); err != nil {
		return nil, false, err
	}
	if req.Notify != "" {
		if err := setNotify(ctx, q, id, v.Version, req.Notify); err != nil {
			return nil, false, err
		}
	}
	setCurrent(a, v)
	a.UpdatedAt, a.LastActiveAt = now, now
	if err := putArtifact(ctx, q, a); err != nil {
		return nil, false, err
	}
	return v, false, s.syncJobs(ctx, q, id, spec, now, true)
}

func checkRunNow(jobs []string, spec pa.Spec) error {
	for _, j := range jobs {
		if _, ok := spec.Jobs[j]; !ok {
			return badRequest("run_now: job %q is not defined in this version", j)
		}
	}
	return nil
}

// afterPublish announces a committed version (unless the publish changed
// nothing), starts run_now jobs and reports every job's status.
func (s *Service) afterPublish(ctx context.Context, id string, v *pa.Version, unchanged bool, req pa.PublishRequest) (*pa.PublishResult, error) {
	if !unchanged {
		s.emit(pa.Event{Type: "artifact.version", Artifact: id, Version: v.Version, Origin: v.Origin})
	}
	for _, j := range req.RunNow {
		if _, err := s.Trigger(ctx, id, j, pa.TriggerRequest{}, pa.TriggerManual); err != nil {
			s.log.Warn("run_now", "artifact", id, "job", j, "err", err)
		}
	}
	jobs, err := s.Jobs(ctx, id)
	if err != nil {
		return nil, err
	}
	return &pa.PublishResult{Version: *v, Unchanged: unchanged, Jobs: jobs}, nil
}

// checkNoFileDirClash rejects trees where a path is both a file and a
// directory ("a" and "a/b"), which can't be materialized on disk.
func checkNoFileDirClash(tree map[string]pa.File) error {
	for p := range tree {
		for i := 0; i < len(p); i++ {
			if p[i] != '/' {
				continue
			}
			if _, ok := tree[p[:i]]; ok {
				return badRequest("%q is a file but %q needs it to be a directory", p[:i], p)
			}
		}
	}
	return nil
}

// Restore publishes a new version with the files of an older one.
func (s *Service) Restore(ctx context.Context, id string, req pa.RestoreRequest) (*pa.PublishResult, error) {
	if a, err := s.Get(ctx, id); err == nil {
		s.tierOf(ctx, a.QuotaKey)
	}
	now := s.now()
	var v *pa.Version
	var unchanged bool
	err := s.st.tx(ctx, func(q querier) error {
		a, err := getArtifact(ctx, q, id)
		if err != nil {
			return s.notFound(err, id)
		}
		if err := checkBase(req.BaseVersion, a.CurrentVersion); err != nil {
			return err
		}
		if req.Version == 0 {
			return badRequest("version is required: the version to restore")
		}
		old, err := getVersion(ctx, q, id, req.Version)
		if err != nil {
			if err == errNotFound {
				return errf(http.StatusNotFound, "not_found", "artifact %s has no version %d", id, req.Version)
			}
			return err
		}
		tree, err := getTree(ctx, q, id, req.Version)
		if err != nil {
			return err
		}
		if err := s.ensureSpecFile(tree, &old.Spec, nil, nil); err != nil {
			return err
		}
		cur, err := getTree(ctx, q, id, a.CurrentVersion)
		if err != nil {
			return err
		}
		if treesEqual(cur, tree) {
			v, err = getVersion(ctx, q, id, a.CurrentVersion)
			unchanged = true
			return err
		}
		size, err := s.checkTree(ctx, q, a.QuotaKey, tree)
		if err != nil {
			return err
		}
		n, err := maxVersion(ctx, q, id)
		if err != nil {
			return err
		}
		label := req.Label
		if label == "" {
			label = fmt.Sprintf("restore v%d", req.Version)
		}
		v = &pa.Version{Artifact: id, Version: n + 1, Parent: a.CurrentVersion, Origin: pa.OriginRestore, Label: label,
			Labels: map[string]string{"restored_from": fmt.Sprint(req.Version)}, FileCount: len(tree), SizeBytes: size, Spec: old.Spec, CreatedAt: now}
		if err := insertVersion(ctx, q, v, tree); err != nil {
			return err
		}
		setCurrent(a, v)
		a.UpdatedAt, a.LastActiveAt = now, now
		if err := putArtifact(ctx, q, a); err != nil {
			return err
		}
		return s.syncJobs(ctx, q, id, old.Spec, now, true)
	})
	if err != nil {
		return nil, err
	}
	if !unchanged {
		s.emit(pa.Event{Type: "artifact.version", Artifact: id, Version: v.Version, Origin: v.Origin})
	}
	jobs, err := s.Jobs(ctx, id)
	if err != nil {
		return nil, err
	}
	return &pa.PublishResult{Version: *v, Unchanged: unchanged, Jobs: jobs}, nil
}

// Fork creates a new artifact whose first version has the files of
// id@version. Jobs are copied (they're in artifact.json) but inactive until
// the new artifact is published to or granted; run state is not copied.
func (s *Service) Fork(ctx context.Context, id string, req pa.ForkRequest) (*pa.Artifact, error) {
	if req.QuotaKey == "" {
		return nil, badRequest("quota_key is required")
	}
	s.tierOf(ctx, req.QuotaKey)
	newIDv := req.ID
	if newIDv == "" {
		newIDv = newID("art-")
	} else if !idRE.MatchString(newIDv) {
		return nil, badRequest("id must match %s", idRE)
	}
	now := s.now()
	var out *pa.Artifact
	err := s.st.tx(ctx, func(q querier) error {
		src, err := getArtifact(ctx, q, id)
		if err != nil {
			return s.notFound(err, id)
		}
		n := req.Version
		if n == 0 {
			n = src.CurrentVersion
		}
		if n == 0 {
			return errf(http.StatusConflict, "empty", "artifact %s has no versions to fork", id)
		}
		old, err := getVersion(ctx, q, id, n)
		if err != nil {
			if err == errNotFound {
				return errf(http.StatusNotFound, "not_found", "artifact %s has no version %d", id, n)
			}
			return err
		}
		if _, err := getArtifact(ctx, q, newIDv); err == nil {
			return errf(http.StatusConflict, "exists", "artifact %s already exists", newIDv)
		}
		tree, err := getTree(ctx, q, id, n)
		if err != nil {
			return err
		}
		spec := old.Spec
		var title *string
		if req.Title != "" {
			title, spec.Title = &req.Title, req.Title
		}
		if err := s.ensureSpecFile(tree, &old.Spec, title, nil); err != nil {
			return err
		}
		size, err := s.checkTree(ctx, q, req.QuotaKey, tree)
		if err != nil {
			return err
		}
		v := &pa.Version{Artifact: newIDv, Version: 1, Origin: pa.OriginFork, Label: fmt.Sprintf("fork of %s v%d", id, n),
			FileCount: len(tree), SizeBytes: size, Spec: spec, CreatedAt: now}
		out = &pa.Artifact{ID: newIDv, QuotaKey: req.QuotaKey, Labels: req.Labels, Meta: req.Meta,
			ForkedFrom: &pa.VersionRef{Artifact: id, Version: n}, CreatedAt: now, UpdatedAt: now, LastActiveAt: now}
		setCurrent(out, v)
		if err := putArtifact(ctx, q, out); err != nil {
			return err
		}
		if err := insertVersion(ctx, q, v, tree); err != nil {
			return err
		}
		return s.syncJobs(ctx, q, newIDv, spec, now, false)
	})
	if err != nil {
		return nil, err
	}
	s.emit(pa.Event{Type: "artifact.created", Artifact: out.ID})
	s.emit(pa.Event{Type: "artifact.version", Artifact: out.ID, Version: 1, Origin: pa.OriginFork})
	return out, nil
}

// ---- blobs

func (s *Service) MissingBlobs(hashes []string) ([]string, error) {
	out := []string{}
	for _, h := range hashes {
		if !validHash(h) {
			return nil, badRequest("%q: %v", h, errBadHash)
		}
		if !s.blobs.Has(h) {
			out = append(out, h)
		}
	}
	return out, nil
}

// ---- quotas

// effectiveQuota resolves a key's limits: its own override, else its
// tier's, else the default tier's (≤ 0 = no limit). The tier is fired's
// mapping, read from the cache (see knownTier) so no network call happens
// inside a transaction.
func (s *Service) effectiveQuota(ctx context.Context, q querier, key string) (pa.Quota, error) {
	v, err := getQuota(ctx, q, key)
	if err != nil {
		return v, err
	}
	for _, name := range []string{s.knownTier(key), pa.DefaultTier} {
		t, _, err := getTier(ctx, q, name)
		if err != nil {
			return v, err
		}
		if v.MinInterval == 0 {
			v.MinInterval = t.MinInterval
		}
		if v.StorageBytes == 0 {
			v.StorageBytes = t.StorageBytes
		}
	}
	return v, nil
}

// ---- tiers: definitions here, the key → tier mapping in fired

const tierCacheTTL = time.Minute

type tierEntry struct {
	tier string
	at   time.Time
}

// tierOf returns a key's tier from fired, refreshing the cache when it's
// older than a minute. If fired can't be reached, the last known tier (or
// the default) is used: a fired outage mustn't block publishing. Call it
// outside transactions.
func (s *Service) tierOf(ctx context.Context, key string) string {
	if key == "" {
		return pa.DefaultTier
	}
	s.mu.Lock()
	e, ok := s.tiers[key]
	s.mu.Unlock()
	if ok && time.Since(e.at) < tierCacheTTL {
		return e.tier
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := s.fired.QuotaKey(ctx, key)
	if err != nil {
		if ok {
			return e.tier
		}
		s.log.Warn("tier lookup in fired failed; using the default tier", "quota_key", key, "err", err)
		return pa.DefaultTier
	}
	s.mu.Lock()
	s.tiers[key] = tierEntry{tier: st.Tier, at: time.Now()}
	s.mu.Unlock()
	return st.Tier
}

// knownTier returns the cached tier without waiting (for use inside
// transactions), refreshing it in the background when stale or missing.
func (s *Service) knownTier(key string) string {
	if key == "" {
		return pa.DefaultTier
	}
	s.mu.Lock()
	e, ok := s.tiers[key]
	s.mu.Unlock()
	if !ok || time.Since(e.at) >= tierCacheTTL {
		go s.tierOf(context.Background(), key)
	}
	if !ok {
		return pa.DefaultTier
	}
	return e.tier
}

var tierNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func (s *Service) PutTier(ctx context.Context, t pa.Tier) error {
	if !tierNameRE.MatchString(t.Name) {
		return badRequest("tier name must match %s", tierNameRE)
	}
	return putTier(ctx, s.st.db, t)
}

func (s *Service) Tiers(ctx context.Context) ([]pa.Tier, error) { return listTiers(ctx, s.st.db) }

func (s *Service) Tier(ctx context.Context, name string) (*pa.Tier, error) {
	t, ok, err := getTier(ctx, s.st.db, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errf(http.StatusNotFound, "not_found", "tier %q not found", name)
	}
	return &pa.Tier{Name: name, Limits: t}, nil
}

// DeleteTier removes a tier's definition. Keys fired maps to it get the
// default tier's limits here until it's defined again.
func (s *Service) DeleteTier(ctx context.Context, name string) error {
	ok, err := deleteTier(ctx, s.st.db, name)
	if err != nil {
		return err
	}
	if !ok {
		return errf(http.StatusNotFound, "not_found", "tier %q not found", name)
	}
	return nil
}

func (s *Service) SetQuota(ctx context.Context, key string, v pa.Quota) error {
	if key == "" {
		return badRequest("quota key is required")
	}
	return putQuota(ctx, s.st.db, key, v)
}

func (s *Service) QuotaStatus(ctx context.Context, key string) (*pa.QuotaStatus, error) {
	tier := s.tierOf(ctx, key)
	raw, err := getQuota(ctx, s.st.db, key)
	if err != nil {
		return nil, err
	}
	eff, err := s.effectiveQuota(ctx, s.st.db, key)
	if err != nil {
		return nil, err
	}
	used, err := storageUsed(ctx, s.st.db, key)
	if err != nil {
		return nil, err
	}
	return &pa.QuotaStatus{Key: key, Tier: tier, Quota: raw, Effective: eff, StorageUsed: used}, nil
}
