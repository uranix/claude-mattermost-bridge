package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
)

// janitor closes the server threads of idle conversations, so threads (and
// their per-connection slots) do not pile up. The conversation itself is kept:
// its session id stays in the state file and the next message re-attaches.
func (b *Bridge) janitor(ctx context.Context) {
	idle := b.cfg.IdleClose
	if idle <= 0 {
		return
	}
	tick := idle / 4
	if tick > time.Minute {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b.mu.Lock()
		all := make([]*conv, 0, len(b.convs))
		for _, c := range b.convs {
			all = append(all, c)
		}
		b.mu.Unlock()
		for _, c := range all {
			c.idleClose(idle)
		}
	}
}

// idleClose closes this conversation's thread if nothing has happened for
// idle. It skips the conversation while a message is being handled.
func (c *conv) idleClose(idle time.Duration) {
	if !c.opMu.TryLock() {
		return
	}
	defer c.opMu.Unlock()

	c.mu.Lock()
	tid := c.threadID
	due := tid != "" && c.active == 0 && time.Since(c.lastActive) >= idle
	c.mu.Unlock()
	if !due {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var res struct {
		CliSessionID string `json:"cli_session_id"`
	}
	err := c.b.app.Call(ctx, "thread/close", map[string]any{"thread_id": tid}, &res)
	switch {
	case err == nil:
		// An empty id means no turn ever completed, so there is nothing to resume.
		if res.CliSessionID == "" {
			c.mu.Lock()
			c.cliSessionID = ""
			c.mu.Unlock()
		}
	case appclient.IsCode(err, appclient.ErrThreadNotFound):
		// The server already forgot it (e.g. reconnect); just drop our side.
	default:
		// Includes servers without thread/close: keep the thread and retry later.
		slog.Warn("idle close failed", "conv", c.key, "err", err)
		c.mu.Lock()
		c.lastActive = time.Now()
		c.mu.Unlock()
		return
	}
	c.dropThread(tid)
	slog.Debug("closed idle thread", "conv", c.key, "thread", tid)
}
