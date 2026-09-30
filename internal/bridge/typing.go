package bridge

import (
	"context"
	"log/slog"
	"time"
)

// typingPeriod spaces the typing events. Clients drop the indicator 5 s after
// each event. The Android app (mattermost-mobile, post_draft/typing) also drops
// it when the expiry of an older event fires, even if a newer event arrived
// since, and hides it unless another event arrives within 500 ms. With events
// 2.625 s apart, each expiry is followed 250 ms later by the event two steps on.
// The extra event after each bot post is off that grid, so on Android its
// expiry may still blink the indicator once.
const typingPeriod = 2625 * time.Millisecond

// beginTyping keeps the typing indicator alive until the last turn finishes.
func (c *conv) beginTyping() {
	ctx, cancel := context.WithCancel(context.Background())
	kick := make(chan struct{}, 1)
	c.mu.Lock()
	if c.stopTyping != nil {
		c.stopTyping()
	}
	c.stopTyping = cancel
	c.typingKick = kick
	c.mu.Unlock()
	go c.typingLoop(ctx, kick)
}

func (c *conv) typingLoop(ctx context.Context, kick <-chan struct{}) {
	t := time.NewTicker(typingPeriod)
	defer t.Stop()
	src := "start"
	for {
		c.mu.Lock()
		root := c.rootID
		c.mu.Unlock()
		rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
		c.typingCall(rctx, root, src)
		rcancel()
		select {
		case <-ctx.Done():
			return
		case <-kick:
			src = "retype" // off the grid; the ticker keeps its phase
		case <-t.C:
			src = "ticker"
		}
	}
}

// retype restores the typing indicator right after the bot posted: clients
// clear it when a post arrives, and the next planned event may be seconds away.
func (c *conv) retype() {
	c.mu.Lock()
	kick := c.typingKick
	busy := c.active > 0 && c.stopTyping != nil
	c.mu.Unlock()
	if !busy || kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// typingCall sends one typing event and logs it at debug level, to tell what the
// clients were offered when the indicator misbehaves.
func (c *conv) typingCall(ctx context.Context, root, src string) {
	start := time.Now()
	err := c.b.mm.Typing(ctx, c.b.me.ID, c.channelID, root)
	slog.Debug("typing", "src", src, "root", root, "took", time.Since(start).Round(time.Millisecond), "err", err)
}
