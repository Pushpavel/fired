package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestVsockHandshake checks the Firecracker host-initiated vsock protocol.
func TestVsockHandshake(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, fVsock))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		line, _ := br.ReadString('\n')
		if line != "CONNECT 1024\n" {
			c.Write([]byte("ERR\n"))
			return
		}
		c.Write([]byte("OK 1073741824\npayload"))
	}()
	vm := &fcVM{dir: dir}
	c, err := vm.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 7)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "payload" {
		t.Fatalf("payload %q %v", b, err)
	}
}

// TestSnapshotCalls checks the API calls made to pause and snapshot a VM.
func TestSnapshotCalls(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, fAPISock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		if r.URL.Path == "/snapshot/create" {
			var sc snapshotCreate
			json.Unmarshal(b, &sc)
			os.WriteFile(filepath.Join(dir, sc.SnapshotPath), []byte("state"), 0o644)
			os.WriteFile(filepath.Join(dir, sc.MemFilePath), []byte("mem"), 0o644)
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	done := make(chan struct{})
	close(done) // process already "exited" once killed
	vm := &fcVM{dir: dir, api: newAPIClient(sock), done: done}
	if err := vm.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], `PATCH /vm {"state":"Paused"}`) ||
		!strings.HasPrefix(calls[1], `PUT /snapshot/create {"snapshot_type":"Full","snapshot_path":"vmstate.tmp","mem_file_path":"memory.tmp"}`) {
		t.Fatalf("calls: %q", calls)
	}
	for _, f := range []string{fVMState, fMemory} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not renamed into place: %v", f, err)
		}
	}
}
