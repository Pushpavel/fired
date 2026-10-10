package artifacts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sync"

	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// Error is an API error with an HTTP status and a machine-readable code.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Msg: fmt.Sprintf(format, a...)}
}

func badRequest(format string, a ...any) *Error {
	return errf(http.StatusBadRequest, "invalid_request", format, a...)
}

func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, errNotFound) {
		return errf(http.StatusNotFound, "not_found", "not found")
	}
	return errf(http.StatusInternalServerError, "internal", "%v", err)
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,47}$`)

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// bus is an in-process pub/sub for events. Slow subscribers drop events
// rather than blocking publishers.
type bus struct {
	mu   sync.Mutex
	subs map[chan pa.Event]struct{}
}

func newBus() *bus { return &bus{subs: map[chan pa.Event]struct{}{}} }

func (b *bus) subscribe() (<-chan pa.Event, func()) {
	ch := make(chan pa.Event, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

func (b *bus) publish(e pa.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
