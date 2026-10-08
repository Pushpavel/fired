// Package events is a small in-process pub/sub for lifecycle events.
package events

import (
	"sync"

	"github.com/Pushpavel/fired/pkg/api"
)

type Bus struct {
	mu   sync.Mutex
	subs map[chan api.Event]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[chan api.Event]struct{}{}} }

// Subscribe returns a channel of events and a cancel function. Slow
// subscribers drop events rather than blocking publishers.
func (b *Bus) Subscribe() (<-chan api.Event, func()) {
	ch := make(chan api.Event, 256)
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

func (b *Bus) Publish(e api.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
