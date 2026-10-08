package agent

import (
	"fmt"
	"sync"
)

// headTail keeps the first and last max/2 bytes of a stream.
type headTail struct {
	mu    sync.Mutex
	max   int
	head  []byte
	tail  []byte // ring of the last max/2 bytes once head is full
	total int64
}

func newHeadTail(max int) *headTail { return &headTail{max: max} }

func (h *headTail) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(p)
	h.total += int64(n)
	half := h.max / 2
	if len(h.head) < half {
		k := half - len(h.head)
		if k > len(p) {
			k = len(p)
		}
		h.head = append(h.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		h.tail = append(h.tail, p...)
		if len(h.tail) > half {
			h.tail = append(h.tail[:0], h.tail[len(h.tail)-half:]...)
		}
	}
	return n, nil
}

// String returns the captured output and whether bytes were dropped.
func (h *headTail) String() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := int64(len(h.head) + len(h.tail))
	if kept == h.total {
		return string(h.head) + string(h.tail), false
	}
	return fmt.Sprintf("%s\n... [%d bytes truncated] ...\n%s", h.head, h.total-kept, h.tail), true
}

// ring retains the last cap bytes of a stream with absolute offsets.
type ring struct {
	mu    sync.Mutex
	cap   int
	buf   []byte
	total int64 // bytes ever written
}

func newRing(cap int) *ring { return &ring{cap: cap} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.total += int64(len(p))
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		// Compact occasionally rather than on every write.
		r.buf = append(make([]byte, 0, r.cap+r.cap/4), r.buf[len(r.buf)-r.cap:]...)
	}
	return len(p), nil
}

// Since returns bytes from absolute offset off, the new end offset, and
// whether some requested bytes were already dropped.
func (r *ring) Since(off int64) ([]byte, int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := r.total - int64(len(r.buf))
	dropped := false
	if off < start {
		off, dropped = start, true
	}
	if off > r.total {
		off = r.total
	}
	out := append([]byte{}, r.buf[off-start:]...)
	return out, r.total, dropped
}
