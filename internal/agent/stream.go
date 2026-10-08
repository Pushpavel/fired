package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/Pushpavel/fired/pkg/api"
)

// handleExecStream runs one command attached to a WebSocket. See the
// protocol description in pkg/api.
func (a *Agent) handleExecStream(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(16 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sendEvent := func(ev api.StreamEvent) error {
		b, _ := json.Marshal(ev)
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	fail := func(err error) {
		_ = sendEvent(api.StreamEvent{Type: "error", Message: err.Error()})
		c.Close(websocket.StatusNormalClosure, "")
	}

	typ, msg, err := c.Read(ctx)
	if err != nil {
		return
	}
	if typ != websocket.MessageText {
		fail(errors.New("first message must be a JSON ExecRequest"))
		return
	}
	var req api.ExecRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		fail(err)
		return
	}
	p, err := a.prepare(&req)
	if err != nil {
		fail(err)
		return
	}
	cmd := p.cmd

	var wmu sync.Mutex
	sendData := func(ch byte, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		frame := make([]byte, 1+len(b))
		frame[0] = ch
		copy(frame[1:], b)
		wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageBinary, frame)
	}
	pump := func(ch byte, rd io.Reader, wg *sync.WaitGroup) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				if sendData(ch, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}

	var stdin io.WriteCloser
	var ptmx *os.File
	var wg sync.WaitGroup
	if req.PTY {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
		// pty.Start sets Setsid+Setctty; Setpgid conflicts with Setsid.
		cmd.SysProcAttr.Setpgid = false
		cols, rows := req.Cols, req.Rows
		if cols <= 0 {
			cols = 120
		}
		if rows <= 0 {
			rows = 30
		}
		ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if err != nil {
			fail(err)
			return
		}
		defer ptmx.Close()
		stdin = ptmx
		wg.Add(1)
		go pump(api.StreamStdout, ptmx, &wg)
	} else {
		in, err := cmd.StdinPipe()
		if err != nil {
			fail(err)
			return
		}
		outR, err := cmd.StdoutPipe()
		if err != nil {
			fail(err)
			return
		}
		errR, err := cmd.StderrPipe()
		if err != nil {
			fail(err)
			return
		}
		if err := cmd.Start(); err != nil {
			fail(err)
			return
		}
		stdin = in
		wg.Add(2)
		go pump(api.StreamStdout, outR, &wg)
		go pump(api.StreamStderr, errR, &wg)
	}
	pid := cmd.Process.Pid
	pr := &proc{pid: pid, cmdline: p.cmdline, started: time.Now(), cmd: cmd, running: true, done: make(chan struct{})}
	a.procs.add(pr)
	_ = sendEvent(api.StreamEvent{Type: "started", PID: pid})

	// Client → process.
	go func() {
		for {
			typ, msg, err := c.Read(ctx)
			if err != nil {
				// Connection gone: terminate the process group.
				killGroup(pid, syscall.SIGKILL)
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := stdin.Write(msg); err != nil && !req.PTY {
					continue
				}
				continue
			}
			var ctl api.StreamControl
			if json.Unmarshal(msg, &ctl) != nil {
				continue
			}
			switch ctl.Type {
			case "resize":
				if ptmx != nil && ctl.Cols > 0 && ctl.Rows > 0 {
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(ctl.Cols), Rows: uint16(ctl.Rows)})
				}
			case "signal":
				if sig, err := parseSignal(ctl.Signal); err == nil {
					killGroup(pid, sig)
				}
			case "eof":
				if ptmx != nil {
					_, _ = ptmx.Write([]byte{4}) // ^D
				} else {
					_ = stdin.Close()
				}
			}
		}
	}()

	var waitErr error
	if req.PTY {
		// With a PTY the reader sees EIO once the session ends; wait for the
		// process first, then drain.
		waitErr = cmd.Wait()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	} else {
		wg.Wait()
		waitErr = cmd.Wait()
	}
	code := exitCode(waitErr)
	pr.finish(code)
	a.procs.exited(pr)
	_ = sendEvent(api.StreamEvent{Type: "exit", ExitCode: code})
	c.Close(websocket.StatusNormalClosure, "")
}
