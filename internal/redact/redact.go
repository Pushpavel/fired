// Package redact scrubs known secret values from command output.
//
// Redaction is best effort: it keeps tokens out of transcripts and logs, it
// is not a security boundary (a command can always transform a secret).
package redact

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"sort"
	"sync"
	"time"
)

// MinSecretLen: values shorter than this are not redacted (too many false
// positives, e.g. a P4PORT like "1666").
const MinSecretLen = 6

type secret struct {
	name string
	val  []byte
	repl []byte
}

// Set is a thread-safe collection of secret values for one sandbox.
type Set struct {
	mu      sync.RWMutex
	secrets []secret
	seen    map[string]bool
}

func NewSet() *Set { return &Set{seen: map[string]bool{}} }

// Add registers value (and common encodings of it) under name.
func (s *Set) Add(name, value string) {
	if len(value) < MinSecretLen {
		return
	}
	repl := []byte("[REDACTED:" + name + "]")
	variants := []string{
		value,
		base64.StdEncoding.EncodeToString([]byte(value)),
		base64.RawStdEncoding.EncodeToString([]byte(value)),
		base64.URLEncoding.EncodeToString([]byte(value)),
		base64.RawURLEncoding.EncodeToString([]byte(value)),
		url.QueryEscape(value),
		url.PathEscape(value),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range variants {
		if len(v) < MinSecretLen || s.seen[v] {
			continue
		}
		s.seen[v] = true
		s.secrets = append(s.secrets, secret{name: name, val: []byte(v), repl: repl})
	}
	// Longest first so a value that contains another is replaced whole.
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i].val) > len(s.secrets[j].val) })
}

// Find reports the name of a known secret (or an encoding of it) that b
// contains.
func (s *Set) Find(b []byte) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if bytes.Contains(b, sec.val) {
			return sec.name, true
		}
	}
	return "", false
}

// MaxLen is the length of the longest registered value or encoding.
func (s *Set) MaxLen() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.secrets) == 0 {
		return 0
	}
	return len(s.secrets[0].val)
}

// Names is how many distinct secrets (by name) the set holds.
func (s *Set) Names() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := map[string]bool{}
	for _, sec := range s.secrets {
		names[sec.name] = true
	}
	return len(names)
}

func (s *Set) Empty() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.secrets) == 0
}

// Bytes returns b with all known secrets replaced.
func (s *Set) Bytes(b []byte) []byte {
	if s == nil {
		return b
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if bytes.Contains(b, sec.val) {
			b = bytes.ReplaceAll(b, sec.val, sec.repl)
		}
	}
	return b
}

func (s *Set) String(v string) string {
	if s.Empty() {
		return v
	}
	return string(s.Bytes([]byte(v)))
}

// maxPartial returns the length of the longest suffix of b that is a proper
// prefix of some secret.
func (s *Set) maxPartial(b []byte) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	best := 0
	for _, sec := range s.secrets {
		max := len(sec.val) - 1
		if max > len(b) {
			max = len(b)
		}
		for k := max; k > best; k-- {
			if bytes.Equal(b[len(b)-k:], sec.val[:k]) {
				best = k
				break
			}
		}
	}
	return best
}

// Stream redacts a stream of chunks. A chunk tail that could be the start of
// a secret is held back until the next chunk (or until FlushAfter elapses
// without new data, to keep interactive output responsive).
type Stream struct {
	set  *Set
	out  func([]byte) error
	mu   sync.Mutex
	held []byte
	tmr  *time.Timer
}

const FlushAfter = 150 * time.Millisecond

func NewStream(set *Set, out func([]byte) error) *Stream {
	return &Stream{set: set, out: out}
}

func (r *Stream) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if r.set.Empty() {
		if len(r.held) > 0 {
			p = append(r.held, p...)
			r.held = nil
		}
		return n, r.out(p)
	}
	buf := append(append([]byte{}, r.held...), p...)
	buf = r.set.Bytes(buf)
	k := r.set.maxPartial(buf)
	emit := buf[:len(buf)-k]
	r.held = append([]byte{}, buf[len(buf)-k:]...)
	if r.tmr != nil {
		r.tmr.Stop()
		r.tmr = nil
	}
	if len(r.held) > 0 {
		r.tmr = time.AfterFunc(FlushAfter, func() { _ = r.Flush() })
	}
	if len(emit) > 0 {
		if err := r.out(emit); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (r *Stream) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tmr != nil {
		r.tmr.Stop()
		r.tmr = nil
	}
	if len(r.held) == 0 {
		return nil
	}
	h := r.held
	r.held = nil
	return r.out(h)
}

// Registry maps sandbox IDs to secret sets. It lives only in memory.
type Registry struct {
	mu   sync.Mutex
	sets map[string]*Set
}

func NewRegistry() *Registry { return &Registry{sets: map[string]*Set{}} }

func (r *Registry) For(id string) *Set {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sets[id]
	if !ok {
		s = NewSet()
		r.sets[id] = s
	}
	return s
}

func (r *Registry) Drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sets, id)
}
