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

func TestFind(t *testing.T) {
	s := NewSet()
	s.Add("GH_TOKEN", "ghp_secretvalue")
	if name, ok := s.Find([]byte("token: ghp_secretvalue\n")); !ok || name != "GH_TOKEN" {
		t.Fatalf("raw: %q %v", name, ok)
	}
	if _, ok := s.Find([]byte("Z2hwX3NlY3JldHZhbHVl")); !ok {
		t.Fatal("base64 encoding not found")
	}
	if _, ok := s.Find([]byte("nothing here")); ok {
		t.Fatal("false positive")
	}
	if s.MaxLen() < len("ghp_secretvalue") {
		t.Fatalf("max len %d", s.MaxLen())
	}
	var nilSet *Set
	if _, ok := nilSet.Find([]byte("x")); ok || nilSet.MaxLen() != 0 {
		t.Fatal("nil set")
	}
}

func TestMultiLineSecret(t *testing.T) {
	s := NewSet()
	s.Add("KEY", "-----BEGIN KEY-----\nMIIEpAIBAAKCAQEAsecretline1\nshort\n-----END KEY-----\n")
	for _, leak := range []string{
		"line: MIIEpAIBAAKCAQEAsecretline1",                                                    // one line of it
		`{"v":"-----BEGIN KEY-----\nMIIEpAIBAAKCAQEAsecretline1\nshort\n-----END KEY-----\n"}`, // the whole file in JSON
	} {
		if _, ok := s.Find([]byte(leak)); !ok {
			t.Errorf("not found in %q", leak)
		}
	}
	for _, fine := range []string{"-----BEGIN KEY-----", "short and sweet"} {
		if _, ok := s.Find([]byte(fine)); ok {
			t.Errorf("%q isn't secret", fine)
		}
	}
}
