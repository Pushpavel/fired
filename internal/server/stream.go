package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Pushpavel/fired/internal/redact"
	"github.com/Pushpavel/fired/internal/sandbox"
	"github.com/Pushpavel/fired/pkg/api"
)

// handleExecStream bridges a client WebSocket to the guest agent's stream
// endpoint, injecting sandbox env/secrets into the first message and
// redacting secrets from output frames.
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.m.Get(id); err != nil {
		writeError(w, err)
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	client.SetReadLimit(16 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fail := func(msg string) {
		b, _ := json.Marshal(api.StreamEvent{Type: "error", Message: msg})
		_ = client.Write(ctx, websocket.MessageText, b)
		client.Close(websocket.StatusNormalClosure, "")
	}

	typ, first, err := client.Read(ctx)
	if err != nil {
		return
	}
	if typ != websocket.MessageText {
		fail("first message must be a JSON ExecRequest")
		return
	}
	var req api.ExecRequest
	if err := json.Unmarshal(first, &req); err != nil {
		fail("invalid ExecRequest: " + err.Error())
		return
	}
	// The process lives as long as the stream: keep the sandbox busy.
	l, err := s.m.Acquire(ctx, id, true)
	if err != nil {
		fail(sandbox.AsError(err).Msg)
		return
	}
	defer l.Release()
	if err := s.m.PrepareExec(l, &req); err != nil {
		fail(err.Error())
		return
	}
	req.Background = false
	agentConn, _, err := websocket.Dial(ctx, "ws://agent/exec/stream", &websocket.DialOptions{HTTPClient: l.Agent.HTTP})
	if err != nil {
		fail("connect to guest agent: " + err.Error())
		return
	}
	agentConn.SetReadLimit(16 << 20)
	defer agentConn.CloseNow()
	b, _ := json.Marshal(req)
	if err := agentConn.Write(ctx, websocket.MessageText, b); err != nil {
		fail(err.Error())
		return
	}

	var wmu sync.Mutex
	writeClient := func(t websocket.MessageType, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, c := context.WithTimeout(ctx, 30*time.Second)
		defer c()
		return client.Write(wctx, t, p)
	}
	mkStream := func(ch byte) *redact.Stream {
		return redact.NewStream(l.Redact, func(p []byte) error {
			frame := make([]byte, 1+len(p))
			frame[0] = ch
			copy(frame[1:], p)
			return writeClient(websocket.MessageBinary, frame)
		})
	}
	outs := map[byte]*redact.Stream{api.StreamStdout: mkStream(api.StreamStdout), api.StreamStderr: mkStream(api.StreamStderr)}

	// client → agent
	go func() {
		defer cancel()
		for {
			t, p, err := client.Read(ctx)
			if err != nil {
				return
			}
			s.m.Touch(id)
			if err := agentConn.Write(ctx, t, p); err != nil {
				return
			}
		}
	}()
	// agent → client
	for {
		t, p, err := agentConn.Read(ctx)
		if err != nil {
			break
		}
		s.m.Touch(id)
		if t == websocket.MessageBinary && len(p) > 0 {
			if st, ok := outs[p[0]]; ok {
				if _, err := st.Write(p[1:]); err != nil {
					break
				}
				continue
			}
		}
		if t == websocket.MessageText {
			for _, st := range outs {
				_ = st.Flush()
			}
		}
		if err := writeClient(t, p); err != nil {
			break
		}
	}
	for _, st := range outs {
		_ = st.Flush()
	}
	client.Close(websocket.StatusNormalClosure, "")
}
