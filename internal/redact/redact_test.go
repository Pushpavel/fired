package redact

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSet(t *testing.T) {
	s := NewSet()
	s.Add("GH_TOKEN", "ghs_abcdef123456")
	s.Add("SHORT", "1666") // too short, ignored
	in := "token ghs_abcdef123456 b64 " + base64.StdEncoding.EncodeToString([]byte("ghs_abcdef123456")) + " port 1666"
	got := s.String(in)
	if strings.Contains(got, "ghs_abcdef123456") || strings.Count(got, "[REDACTED:GH_TOKEN]") != 2 || !strings.Contains(got, "1666") {
		t.Fatalf("got %q", got)
	}
	var nilSet *Set
	if nilSet.String("x") != "x" {
		t.Fatal("nil set should pass through")
	}
}

func TestStreamAcrossChunks(t *testing.T) {
	s := NewSet()
	s.Add("T", "supersecretvalue")
	var out strings.Builder
	st := NewStream(s, func(p []byte) error { out.Write(p); return nil })
	st.Write([]byte("hello super"))
	st.Write([]byte("secretval"))
	st.Write([]byte("ue world"))
	st.Flush()
	if got := out.String(); got != "hello [REDACTED:T] world" {
		t.Fatalf("got %q", got)
	}
}

func TestStreamFlushesHeldBytes(t *testing.T) {
	s := NewSet()
	s.Add("T", "supersecretvalue")
	ch := make(chan string, 4)
	st := NewStream(s, func(p []byte) error { ch <- string(p); return nil })
	st.Write([]byte("prompt$ su"))
	if got := <-ch; got != "prompt$ " {
		t.Fatalf("first emit %q", got)
	}
	select {
	case got := <-ch:
		if got != "su" {
			t.Fatalf("held flush %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("held bytes were never flushed")
	}
}
