package httpapi

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	hintEvents uint32 = 1 << iota
	hintDirectory
	hintSession
	hintReads
	hintTimer
	hintSync
	hintPresence
)

type gatewayConnection struct {
	cancel        context.CancelFunc
	wake          chan struct{}
	user          string // protected by gateway.mu
	watchPresence bool   // protected by gateway.mu
	mu            sync.Mutex
	flags         uint32
	reads         map[string]bool
}

func (c *gatewayConnection) note(flags uint32, conversation string) {
	c.mu.Lock()
	c.flags |= flags
	if conversation != "" {
		if c.reads == nil {
			c.reads = map[string]bool{}
		}
		if len(c.reads) < 100 {
			c.reads[conversation] = true
		} else {
			c.flags |= hintSync
		}
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *gatewayConnection) take() (uint32, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	flags := c.flags
	c.flags = 0
	ids := make([]string, 0, len(c.reads))
	for id := range c.reads {
		ids = append(ids, id)
	}
	c.reads = nil
	return flags, ids
}
func (g *gateway) hint(payload string) {
	flags := hintEvents
	user, conversation := "", ""
	switch {
	case payload == "directory":
		flags |= hintDirectory
	case payload == "sessions":
		flags |= hintSession
	case payload == "reset":
		flags |= hintDirectory | hintSession | hintSync
	case strings.HasPrefix(payload, "read:"):
		parts := strings.Split(payload, ":")
		if len(parts) != 3 {
			return
		}
		user, conversation = parts[1], parts[2]
		flags = hintReads
	}
	g.mu.Lock()
	targets := make([]*gatewayConnection, 0, len(g.connections))
	for _, connection := range g.connections {
		if user == "" || connection.user == user {
			targets = append(targets, connection)
		}
	}
	g.mu.Unlock()
	for _, connection := range targets {
		connection.note(flags, conversation)
	}
}

// One dedicated LISTEN connection wakes independent account workers. It never
// executes their queries and does not consume a pooled request connection.
// Hints contain no message body or credential, and transaction rollback sends
// nothing. A lost listener triggers a directory/read-state reconciliation.
func (g *gateway) listen(ctx context.Context) {
	defer g.listenerWG.Done()
	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, g.store.Pool.Config().ConnConfig.Copy())
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN chatxo_gateway")
			if err == nil {
				g.listenerOnline.Store(true)
				g.hint("reset")
				for ctx.Err() == nil {
					notification, waitErr := conn.WaitForNotification(ctx)
					if waitErr != nil {
						break
					}
					g.hint(notification.Payload)
				}
			}
			g.listenerOnline.Store(false)
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
