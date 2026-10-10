package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listen calls fn with every payload sent on a Postgres channel until ctx
// ends, reconnecting after errors. ready (may be nil) is closed once the
// first LISTEN is in place. Notifications sent while disconnected are lost;
// listeners re-read state instead of relying on every event.
func (s *Store) Listen(ctx context.Context, channel string, ready chan<- struct{}, fn func(payload string)) {
	for ctx.Err() == nil {
		s.listenOnce(ctx, channel, func() {
			if ready != nil {
				close(ready)
				ready = nil
			}
		}, fn)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (s *Store) listenOnce(ctx context.Context, channel string, listening func(), fn func(string)) {
	conn, err := pgx.Connect(ctx, s.url)
	if err != nil {
		return
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return
	}
	listening()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		fn(n.Payload)
	}
}
