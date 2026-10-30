package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

// haltEmoji on any post of a conversation stops the agent: the running turn is
// interrupted, like !cancel, and the agent is told to review the latest
// messages before it goes on. Messages it had not read yet are sent again,
// since the interrupt drops them.
const haltEmoji = "no_entry_sign"

// onHaltReaction finds the conversation of the reacted post and halts it.
func (b *Bridge) onHaltReaction(ctx context.Context, r mm.Reaction) {
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	post, err := b.mm.GetPost(pctx, r.PostID)
	cancel()
	if err != nil {
		slog.Warn("halt: post lookup failed", "post", r.PostID, "err", err)
		return
	}
	root := post.RootID
	if root == "" {
		root = post.ID
	}
	var c *conv
	for _, key := range []string{"dm:" + post.ChannelID, "ch:" + post.ChannelID + ":" + root} {
		if b.known(key) {
			c = b.getConv(key, post.ChannelID)
			break
		}
	}
	if c == nil {
		return // not a post of any conversation
	}
	u, ok := b.user(ctx, r.UserID)
	if !ok || !b.mayUse(u, strings.HasPrefix(c.key, "ch:")) {
		slog.Debug("ignored halt from a user without access", "user", u.Username)
		return
	}
	c.halt(u.Username, r.PostID)
}

// known reports whether a conversation exists, live or saved.
func (b *Bridge) known(key string) bool {
	b.mu.Lock()
	_, ok := b.convs[key]
	b.mu.Unlock()
	if ok {
		return true
	}
	_, ok = b.state.get(key)
	return ok
}

// halt interrupts the running turn and starts one that makes the agent
// review the conversation, carrying the messages it had not read.
func (c *conv) halt(by, postID string) {
	c.opMu.Lock() // not while a message is being handed over
	defer c.opMu.Unlock()
	c.mu.Lock()
	tid, n := c.threadID, c.active
	c.mu.Unlock()
	if tid == "" || n == 0 {
		c.reply("_Nothing is running to stop._")
		return
	}
	c.reply(fmt.Sprintf("_Stopped by @%s. The agent will review the latest messages before going on._", by))
	if msg := c.interruptAll(tid, n); msg != "" {
		c.reply(msg)
		return
	}

	// What the agent never read is gone from its queue; take it back.
	c.mu.Lock()
	var unread []awaited
	for id, a := range c.awaiting {
		unread = append(unread, a)
		delete(c.awaiting, id)
	}
	c.mu.Unlock()
	sort.Slice(unread, func(i, j int) bool { return unread[i].since.Before(unread[j].since) })

	var sb strings.Builder
	fmt.Fprintf(&sb, "@%s stopped you (they reacted with :%s:), so your previous turn was interrupted. Do not simply resume it. First review the latest messages in this conversation", by, haltEmoji)
	if len(unread) == 0 {
		sb.WriteString(": something you were told, or did, made them stop you. Then decide how to go on, and ask if it is unclear.")
	} else {
		sb.WriteString(", including these, which arrived while you worked and you had not read yet:\n")
		for _, a := range unread {
			sb.WriteString("\n---\n" + a.prompt)
		}
		sb.WriteString("\n---\n\nThen decide how to go on, and ask if it is unclear.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.send(ctx, sb.String(), postID); err != nil {
		c.reply("**Error:** " + err.Error())
	}
}
