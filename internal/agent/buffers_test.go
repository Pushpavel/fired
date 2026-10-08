package agent

import (
	"strings"
	"testing"
)

func TestHeadTail(t *testing.T) {
	h := newHeadTail(10)
	h.Write([]byte("abc"))
	if s, tr := h.String(); s != "abc" || tr {
		t.Fatalf("%q %v", s, tr)
	}
	h.Write([]byte(strings.Repeat("x", 100) + "END"))
	s, tr := h.String()
	if !tr || !strings.HasPrefix(s, "abcxx") || !strings.HasSuffix(s, "xxEND") || !strings.Contains(s, "truncated") {
		t.Fatalf("%q", s)
	}
}

func TestRing(t *testing.T) {
	r := newRing(8)
	r.Write([]byte("hello "))
	b, off, dropped := r.Since(0)
	if string(b) != "hello " || off != 6 || dropped {
		t.Fatalf("%q %d %v", b, off, dropped)
	}
	r.Write([]byte("world!"))
	b, off, dropped = r.Since(6)
	if string(b) != "world!" || off != 12 || dropped {
		t.Fatalf("%q %d %v", b, off, dropped)
	}
	b, _, dropped = r.Since(0)
	if string(b) != "lo world!"[1:] || !dropped {
		t.Fatalf("%q %v", b, dropped)
	}
}
