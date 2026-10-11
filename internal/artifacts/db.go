package artifacts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Pushpavel/fired/internal/redact"
	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// The db: every artifact's documents (see pkg/artifacts/db.go). Who may do
// what is fixed, not configured: the API token, the owner (a header-routed
// request with X-Artifact-Owner) and a running job (its run token) read and
// write everything; any other viewer only reads, and not _private/.

// dbAccess is what a caller may do.
type dbAccess struct {
	// full: write, and see _private/.
	full bool
	// run is set for a job's requests: its writes are checked for the
	// run's credentials and counted.
	run *liveRun
}

// liveRun is what the service keeps about a run while it runs.
type liveRun struct {
	id      string
	secrets *redact.Set
	writes  int // documents written or deleted (under s.mu)
}

var dbSegRE = regexp.MustCompile(`^[A-Za-z0-9_.~@+:-]{1,128}$`)

const dbMaxDepth = 16

// dbPath checks a db path and returns it without surrounding slashes and
// its number of segments ("" and 0 for the root).
func dbPath(p string) (string, int, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", 0, nil
	}
	segs := strings.Split(p, "/")
	if len(segs) > dbMaxDepth {
		return "", 0, badRequest("db path %q is deeper than %d segments", p, dbMaxDepth)
	}
	for _, s := range segs {
		if s == "." || s == ".." || !dbSegRE.MatchString(s) {
			return "", 0, badRequest("db path %q: each segment is 1-128 of A-Z a-z 0-9 _ . ~ @ + : - (not . or ..)", p)
		}
	}
	return p, len(segs), nil
}

// docPath checks the path of a document (an even number of segments).
func docPath(p string) (string, error) {
	c, n, err := dbPath(p)
	if err != nil {
		return "", err
	}
	if n == 0 || n%2 != 0 {
		return "", badRequest("%q is a collection, not a document: a document's path has an even number of segments (collection/doc)", p)
	}
	return c, nil
}

func dbParent(p string) string { return p[:strings.LastIndexByte(p, '/')] }

func dbPrivate(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return first == pa.PrivateCollection
}

func noDoc(p string) error { return errf(http.StatusNotFound, "no_document", "no document %s", p) }

// ---- reading

const docCols = `path, data::text, rev, created_at, updated_at`

func scanDoc(sc interface{ Scan(...any) error }) (*pa.Doc, error) {
	var d pa.Doc
	var data string
	var c, u int64
	if err := sc.Scan(&d.Path, &data, &d.Rev, &c, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	d.Data = compact(data)
	d.CreatedAt, d.UpdatedAt = fromMS(c), fromMS(u)
	return &d, nil
}

// compact drops the spaces Postgres prints jsonb with.
func compact(s string) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, []byte(s)) != nil {
		return json.RawMessage(s)
	}
	return b.Bytes()
}

func getDoc(ctx context.Context, q querier, id, p string) (*pa.Doc, error) {
	return scanDoc(q.QueryRowContext(ctx, `SELECT `+docCols+` FROM docs WHERE artifact=? AND path=?`, id, p))
}

// GetDoc returns one document.
func (s *Service) GetDoc(ctx context.Context, id, p string, acc dbAccess) (*pa.Doc, error) {
	p, err := docPath(p)
	if err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	if dbPrivate(p) && !acc.full {
		return nil, noDoc(p)
	}
	d, err := getDoc(ctx, s.st.db, id, p)
	if err == errNotFound {
		return nil, noDoc(p)
	}
	return d, err
}

type dbFilter struct {
	field []string
	op    string
	value string // JSON
}

var dbFieldRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// parseWhere parses "field op value"; a value that isn't JSON is a string.
func parseWhere(w string) (dbFilter, error) {
	i := strings.IndexAny(w, "=!<>")
	if i <= 0 {
		return dbFilter{}, badRequest("where %q: want field op value, op one of == != < <= > >=", w)
	}
	name := strings.TrimSpace(w[:i])
	f := dbFilter{field: strings.Split(name, ".")}
	if !dbFieldRE.MatchString(name) {
		return f, badRequest("where %q: a field is a key or a dotted path (a.b) of A-Z a-z 0-9 _ -", w)
	}
	rest := w[i:]
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if strings.HasPrefix(rest, op) {
			f.op = op
			break
		}
	}
	if f.op == "" {
		return f, badRequest("where %q: op must be one of == != < <= > >=", w)
	}
	v := strings.TrimSpace(rest[len(f.op):])
	if !json.Valid([]byte(v)) {
		b, _ := json.Marshal(v)
		v = string(b)
	}
	f.value = v
	return f, nil
}

const (
	dbDefaultLimit = 100
	dbMaxLimit     = 1000
)

// queryParams reads ?where=&order=&limit=&after= for a collection.
func queryParams(r *http.Request) (pa.DocQuery, error) {
	v := r.URL.Query()
	dq := pa.DocQuery{Where: v["where"], Order: v.Get("order"), After: v.Get("after")}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			return dq, badRequest("limit must be a positive number")
		}
		dq.Limit = n
	}
	return dq, nil
}

// Query lists the documents of collection coll ("" lists every document).
func (s *Service) Query(ctx context.Context, id, coll string, dq pa.DocQuery, acc dbAccess) (*pa.DocList, error) {
	coll, n, err := dbPath(coll)
	if err != nil {
		return nil, err
	}
	if n%2 == 0 && n > 0 {
		return nil, badRequest("%q is a document, not a collection", coll)
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	out := &pa.DocList{Docs: []pa.Doc{}}
	if coll != "" && dbPrivate(coll) && !acc.full {
		return out, nil
	}
	where, args := `artifact=?`, []any{id}
	if coll != "" {
		where += ` AND parent=?`
		args = append(args, coll)
	} else if !acc.full {
		where += ` AND split_part(path, '/', 1) <> ?`
		args = append(args, pa.PrivateCollection)
	}
	for _, w := range dq.Where {
		f, err := parseWhere(w)
		if err != nil {
			return nil, err
		}
		switch f.op {
		case "==":
			where += ` AND data #> ?::text[] = ?::jsonb`
			args = append(args, f.field, f.value)
		case "!=":
			where += ` AND data #> ?::text[] <> ?::jsonb`
			args = append(args, f.field, f.value)
		default:
			// jsonb orders across types (strings below numbers); compare
			// only values of the filter's type.
			where += ` AND jsonb_typeof(data #> ?::text[]) = jsonb_typeof(?::jsonb) AND data #> ?::text[] ` + f.op + ` ?::jsonb`
			args = append(args, f.field, f.value, f.field, f.value)
		}
	}
	order := ` ORDER BY path`
	if o := dq.Order; o != "" {
		dir := "ASC"
		if strings.HasPrefix(o, "-") {
			o, dir = o[1:], "DESC"
		}
		if !dbFieldRE.MatchString(o) {
			return nil, badRequest("order %q: a field (a key or a dotted path), or -field for descending", dq.Order)
		}
		order = ` ORDER BY data #> ?::text[] ` + dir + ` NULLS LAST, path`
		args = append(args, strings.Split(o, "."))
	}
	limit := dq.Limit
	if limit <= 0 {
		limit = dbDefaultLimit
	}
	if limit > dbMaxLimit {
		limit = dbMaxLimit
	}
	offset := 0
	if dq.After != "" {
		if offset, err = strconv.Atoi(dq.After); err != nil || offset < 0 {
			return nil, badRequest("after must be the next value of a previous page")
		}
	}
	rows, err := s.st.db.QueryContext(ctx, `SELECT `+docCols+` FROM docs WHERE `+where+order+` LIMIT ? OFFSET ?`,
		append(args, limit+1, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out.Docs = append(out.Docs, *d)
	}
	if len(out.Docs) > limit {
		out.Docs = out.Docs[:limit]
		out.Next = strconv.Itoa(offset + limit)
	}
	return out, rows.Err()
}

// ---- writing

// object checks that b is a JSON object.
func object(b json.RawMessage, what string) error {
	var v any
	if err := decodeJSON(b, &v); err != nil {
		return badRequest("%s must be a JSON object: %v", what, err)
	}
	if _, ok := v.(map[string]any); !ok {
		return badRequest("%s must be a JSON object", what)
	}
	return nil
}

// Write applies writes in order, all or none, and returns the documents as
// written (deletes with Deleted set).
func (s *Service) Write(ctx context.Context, id string, writes []pa.DBWrite, acc dbAccess) ([]pa.Doc, error) {
	if !acc.full {
		return nil, errf(http.StatusForbidden, "read_only", "only the artifact's owner and its job write its db")
	}
	if len(writes) == 0 {
		return nil, badRequest("writes is empty")
	}
	if len(writes) > s.cfg.Limits.MaxBatchWrites {
		return nil, badRequest("a batch has at most %d writes (got %d)", s.cfg.Limits.MaxBatchWrites, len(writes))
	}
	for i := range writes {
		w := &writes[i]
		p, err := docPath(w.Path)
		if err != nil {
			return nil, err
		}
		w.Path = p
		switch w.Op {
		case pa.DBSet, pa.DBUpdate:
			if err := object(w.Data, "the data of "+p); err != nil {
				return nil, err
			}
		case pa.DBDelete:
			if len(w.Data) > 0 {
				return nil, badRequest("deleting %s: a delete has no data", p)
			}
		default:
			return nil, badRequest("writes[%d] (%s): op must be set, update or delete", i, p)
		}
		if acc.run != nil && acc.run.secrets != nil {
			if name, ok := acc.run.secrets.Find(w.Data); ok {
				return nil, errf(http.StatusUnprocessableEntity, "secret", "%s contains the value of credential %s: a job's db is shown to the artifact's viewers", p, name)
			}
		}
	}
	now := s.now()
	var out []pa.Doc
	err := s.st.tx(ctx, func(q querier) error {
		a, err := getArtifact(ctx, q, id)
		if err != nil {
			return s.notFound(err, id)
		}
		var grew int64
		created := false
		out = make([]pa.Doc, 0, len(writes))
		for _, w := range writes {
			cur, err := getDoc(ctx, q, id, w.Path)
			if err != nil && err != errNotFound {
				return err
			}
			switch {
			case w.Rev == -1 && cur != nil:
				return errf(http.StatusPreconditionFailed, "rev_mismatch", "document %s already exists (rev %d)", w.Path, cur.Rev)
			case w.Rev > 0 && cur == nil:
				return errf(http.StatusPreconditionFailed, "rev_mismatch", "document %s doesn't exist (expected rev %d)", w.Path, w.Rev)
			case w.Rev > 0 && cur.Rev != w.Rev:
				return errf(http.StatusPreconditionFailed, "rev_mismatch", "document %s is at rev %d, not %d: read it again", w.Path, cur.Rev, w.Rev)
			}
			var oldSize int64
			if cur != nil {
				oldSize = int64(len(cur.Data))
			}
			if w.Op == pa.DBDelete {
				if cur != nil {
					if _, err := q.ExecContext(ctx, `DELETE FROM docs WHERE artifact=? AND path=?`, id, w.Path); err != nil {
						return err
					}
				}
				out = append(out, pa.Doc{Path: w.Path, Deleted: true})
				continue
			}
			data := w.Data
			if w.Op == pa.DBUpdate && cur != nil {
				var d, p any
				if err := decodeJSON(cur.Data, &d); err != nil {
					return err
				}
				if err := decodeJSON(w.Data, &p); err != nil {
					return err
				}
				if data, err = json.Marshal(mergeValue(d, p)); err != nil {
					return err
				}
			} else if w.Op == pa.DBUpdate {
				// A merge patch onto nothing drops its nulls.
				var p any
				if err := decodeJSON(w.Data, &p); err != nil {
					return err
				}
				if data, err = json.Marshal(mergeValue(nil, p)); err != nil {
					return err
				}
			}
			if len(data) > s.cfg.Limits.MaxDocBytes {
				return errf(http.StatusRequestEntityTooLarge, "too_large", "%s would be %d bytes; a document is at most %d", w.Path, len(data), s.cfg.Limits.MaxDocBytes)
			}
			d := pa.Doc{Path: w.Path, Rev: 1, CreatedAt: now, UpdatedAt: now}
			if cur != nil {
				d.Rev, d.CreatedAt = cur.Rev+1, cur.CreatedAt
			} else {
				created = true
			}
			var stored string
			if err := q.QueryRowContext(ctx, `
INSERT INTO docs(artifact, path, parent, data, size, rev, created_at, updated_at) VALUES(?,?,?,?::jsonb,?,?,?,?)
ON CONFLICT(artifact, path) DO UPDATE SET data=excluded.data, size=excluded.size, rev=excluded.rev, updated_at=excluded.updated_at
RETURNING data::text`, id, w.Path, dbParent(w.Path), string(data), len(data), d.Rev, ms(d.CreatedAt), ms(now)).Scan(&stored); err != nil {
				return err
			}
			d.Data = compact(stored)
			grew += int64(len(data)) - oldSize
			out = append(out, d)
		}
		if created {
			var n int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs WHERE artifact=?`, id).Scan(&n); err != nil {
				return err
			}
			if n > s.cfg.Limits.MaxDocs {
				return errf(http.StatusRequestEntityTooLarge, "too_many_docs", "an artifact's db has at most %d documents", s.cfg.Limits.MaxDocs)
			}
		}
		if grew > 0 {
			if err := s.checkStorage(ctx, q, a.QuotaKey); err != nil {
				return err
			}
		}
		return touchArtifact(ctx, q, id, now)
	})
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(out))
	for i, d := range out {
		paths[i] = d.Path
	}
	s.dbChanged(id, paths, acc)
	return out, nil
}

// checkStorage refuses a write that left key over its storage quota.
func (s *Service) checkStorage(ctx context.Context, q querier, key string) error {
	quota, err := s.effectiveQuota(ctx, q, key)
	if err != nil || quota.StorageBytes <= 0 {
		return err
	}
	used, err := storageUsed(ctx, q, key)
	if err != nil {
		return err
	}
	if used > quota.StorageBytes {
		return errf(http.StatusRequestEntityTooLarge, "quota_exceeded", "storage quota exceeded: %d > %d", used, quota.StorageBytes)
	}
	return nil
}

// DeleteDocs removes the documents under p: one document, or a collection and
// everything under it ("" is the whole db).
func (s *Service) DeleteDocs(ctx context.Context, id, p string, rev int64, acc dbAccess) error {
	if !acc.full {
		return errf(http.StatusForbidden, "read_only", "only the artifact's owner and its job write its db")
	}
	p, n, err := dbPath(p)
	if err != nil {
		return err
	}
	if n > 0 && n%2 == 0 {
		_, err := s.Write(ctx, id, []pa.DBWrite{{Op: pa.DBDelete, Path: p, Rev: rev}}, acc)
		return err
	}
	if rev != 0 {
		return badRequest("If-Match is for a document, not a collection")
	}
	var paths []string
	err = s.st.tx(ctx, func(q querier) error {
		if _, err := getArtifact(ctx, q, id); err != nil {
			return s.notFound(err, id)
		}
		query, args := `DELETE FROM docs WHERE artifact=?`, []any{id}
		if p != "" {
			query += ` AND starts_with(path, ?)`
			args = append(args, p+"/")
		}
		rows, err := q.QueryContext(ctx, query+` RETURNING path`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var dp string
			if err := rows.Scan(&dp); err != nil {
				return err
			}
			paths = append(paths, dp)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if len(paths) > 0 {
		s.dbChanged(id, paths, acc)
	}
	return nil
}

func (s *Service) dbChanged(id string, paths []string, acc dbAccess) {
	if acc.run != nil {
		s.mu.Lock()
		acc.run.writes += len(paths)
		s.mu.Unlock()
	}
	s.emit(pa.Event{Type: "db.changed", Artifact: id, Paths: paths})
}

// dbBytes is the size of the documents of key's artifacts.
func dbBytes(ctx context.Context, q querier, key string) (int64, error) {
	var n sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT SUM(d.size)::BIGINT FROM docs d JOIN artifacts a ON a.id=d.artifact WHERE a.quota_key=?`, key).Scan(&n)
	return n.Int64, err
}

// copyDocs copies src's documents to dst (a fork): all of them with
// private, else all but _private/.
func copyDocs(ctx context.Context, q querier, src, dst string, private bool, now time.Time) error {
	skip := pa.PrivateCollection
	if private {
		skip = "" // no first segment is empty
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO docs(artifact, path, parent, data, size, rev, created_at, updated_at)
SELECT ?, path, parent, data, size, 1, ?, ? FROM docs WHERE artifact=? AND split_part(path, '/', 1) <> ?`,
		dst, ms(now), ms(now), src, skip)
	return err
}

// ---- job tokens

// A run's token is its ID and an HMAC of it: it's valid while the run is
// running, for its artifact's db only.
func (s *Service) runToken(runID string) string {
	return "run." + runID + "." + s.mac("fired-artifacts-run", runID)[:40]
}

// jobAccess returns the db access of a request carrying a run token for
// artifact id, or nil.
func (s *Service) jobAccess(ctx context.Context, r *http.Request, id string) *liveRun {
	tok := r.Header.Get("X-Fired-Token")
	if tok == "" {
		tok, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	rest, ok := strings.CutPrefix(tok, "run.")
	if !ok {
		return nil
	}
	i := strings.LastIndexByte(rest, '.')
	if i <= 0 || !hmac.Equal([]byte(tok), []byte(s.runToken(rest[:i]))) {
		return nil
	}
	runID := rest[:i]
	s.mu.Lock()
	lr := s.live[runID]
	s.mu.Unlock()
	if lr == nil {
		return nil
	}
	run, err := getRun(ctx, s.st.db, runID)
	if err != nil || run.Status != pa.RunRunning || run.Artifact != id {
		return nil
	}
	return lr
}

// dbRoute matches /v1/artifacts/{id}/db and the paths under it.
var dbRoute = regexp.MustCompile(`^/v1/artifacts/([^/]+)/db(/.*)?$`)

// ---- HTTP

// serveDB serves the db at path p (relative to the db's root) for the API
// and for views: GET reads a document or a collection (?watch streams it),
// PUT sets, PATCH merges, DELETE deletes; POST on the root is a batch.
func (s *Service) serveDB(w http.ResponseWriter, r *http.Request, id, p string, acc dbAccess) {
	p = strings.Trim(p, "/")
	var rev int64
	if m := strings.Trim(r.Header.Get("If-Match"), `"`); m != "" {
		n, err := strconv.ParseInt(m, 10, 64)
		if err != nil || n == 0 || n < -1 {
			writeErr(w, badRequest("If-Match is a document's rev (or -1: it must not exist)"))
			return
		}
		rev = n
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		_, n, err := dbPath(p)
		if err != nil {
			writeErr(w, err)
			return
		}
		if r.URL.Query().Has("watch") {
			s.watchDB(w, r, id, p, n, acc)
			return
		}
		v, err := s.readDB(r.Context(), r, id, p, n, acc)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, v)
	case http.MethodPut, http.MethodPatch:
		b, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.Limits.MaxDocBytes)+1))
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(b) > s.cfg.Limits.MaxDocBytes {
			writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", "a document is at most %d bytes", s.cfg.Limits.MaxDocBytes))
			return
		}
		op := pa.DBSet
		if r.Method == http.MethodPatch {
			op = pa.DBUpdate
		}
		docs, err := s.Write(r.Context(), id, []pa.DBWrite{{Op: op, Path: p, Data: b, Rev: rev}}, acc)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, docs[0])
	case http.MethodDelete:
		if err := s.DeleteDocs(r.Context(), id, p, rev, acc); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		if p != "" {
			writeErr(w, errf(http.StatusMethodNotAllowed, "method_not_allowed", "POST a batch to the db's root"))
			return
		}
		var req pa.DBBatch
		if err := decode(r, &req, 16<<20); err != nil {
			writeErr(w, err)
			return
		}
		docs, err := s.Write(r.Context(), id, req.Writes, acc)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, pa.DBBatchResult{Docs: docs})
	default:
		writeErr(w, errf(http.StatusMethodNotAllowed, "method_not_allowed", "%s isn't a db method", r.Method))
	}
}

// readDB reads a document (a *pa.Doc, nil in a watch when it's missing) or
// a collection (a *pa.DocList).
func (s *Service) readDB(ctx context.Context, r *http.Request, id, p string, n int, acc dbAccess) (any, error) {
	if n > 0 && n%2 == 0 {
		return s.GetDoc(ctx, id, p, acc)
	}
	dq, err := queryParams(r)
	if err != nil {
		return nil, err
	}
	return s.Query(ctx, id, p, dq, acc)
}

// watchDB streams a document or a collection as server-sent events: a
// "snapshot" event with the whole result now and after every change to it
// (a document is null while it doesn't exist).
func (s *Service) watchDB(w http.ResponseWriter, r *http.Request, id, p string, n int, acc dbAccess) {
	ctx := r.Context()
	isDoc := n > 0 && n%2 == 0
	// To a viewer, _private/ doesn't exist: not even when it changes.
	if !acc.full && p != "" && dbPrivate(p) {
		writeErr(w, noDoc(p))
		return
	}
	ch, cancel := s.bus.subscribe()
	defer cancel()
	snapshot := func() ([]byte, error) {
		v, err := s.readDB(ctx, r, id, p, n, acc)
		if err != nil {
			if isDoc && asError(err).Code == "no_document" {
				return []byte("null"), nil
			}
			return nil, err
		}
		return json.Marshal(v)
	}
	first, err := snapshot()
	if err != nil {
		writeErr(w, err)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	send := func(b []byte) bool {
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(first) {
		return
	}
	touches := func(e pa.Event) bool {
		if e.Artifact != id {
			return false
		}
		if e.Type == "artifact.deleted" {
			return true
		}
		if e.Type != "db.changed" {
			return false
		}
		for _, cp := range e.Paths {
			if !acc.full && dbPrivate(cp) {
				continue
			}
			if isDoc && cp == p || !isDoc && (p == "" || dbParent(cp) == p) {
				return true
			}
		}
		return false
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !touches(e) {
				continue
			}
			// Let a burst of writes settle into one snapshot.
			settle := time.After(50 * time.Millisecond)
		drain:
			for {
				select {
				case <-ch:
				case <-settle:
					break drain
				case <-ctx.Done():
					return
				}
			}
			b, err := snapshot()
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(asError(err).Msg))
				_ = rc.Flush()
				return
			}
			if !send(b) {
				return
			}
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
