package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
)

// Mattermost swallows messages starting with "/" as its own slash commands,
// so the bridge uses a "!" prefix instead.
const helpText = `**Commands**
- ` + "`!help`" + ` - this message
- ` + "`!status`" + ` - thread, permission mode, running turns
- ` + "`!new`" + ` - start a fresh context (the old one keeps running until it finishes)
- ` + "`!cancel`" + ` - interrupt the running work and drop queued messages
- ` + "`!model [name|default]`" + ` - list models, or switch this conversation's model (a partial name is fine)
- ` + "`!allow [always]`" + ` / ` + "`!deny [reason]`" + ` - answer a permission prompt (the oldest waiting one)
- ` + "`!mode [default|plan|acceptEdits|dontAsk|bypassPermissions]`" + ` - show or change the permission mode

Messages sent while the agent is working are queued as the next turn.`

// command handles a "!cmd" message. It returns false for unknown commands,
// which are then passed to the agent as ordinary text.
func (c *conv) command(in inbound) bool {
	fields := strings.Fields(in.text)
	name := strings.ToLower(strings.TrimPrefix(fields[0], "!"))
	args := fields[1:]

	switch name {
	case "help":
		c.reply(helpText)
	case "status":
		c.mu.Lock()
		tid, mode, active, model, waiting := c.threadID, c.mode, c.active, c.model, len(c.prompts)
		c.mu.Unlock()
		if model == "" {
			model = "default"
		}
		if tid == "" {
			tid = "(none yet)"
		}
		cwd := c.b.cfg.Cwd
		if cwd == "" {
			cwd = "(server default)"
		}
		c.reply(fmt.Sprintf("Thread: `%s`\nMode: `%s`\nModel: `%s`\nRunning turns: %d\nWaiting for approval: %d\nWorking directory: `%s`", tid, mode, model, active, waiting, cwd))
	case "new", "clear":
		c.mu.Lock()
		tid := c.threadID
		c.active = 0
		c.cliSessionID = ""
		c.attached = false
		c.mu.Unlock()
		c.b.state.delete(c.key)
		if tid != "" {
			go c.closeThread(tid)
			c.dropThread(tid)
		}
		c.turnDone()
		c.reply("Started a fresh context.")
	case "cancel":
		c.cancel()
	case "mode":
		c.setMode(args)
	case "model":
		c.modelCommand(args)
	case "allow":
		c.answerOldest("allow", len(args) > 0 && strings.EqualFold(args[0], "always"), "", in.user)
	case "deny":
		c.answerOldest("deny", false, strings.Join(args, " "), in.user)
	default:
		return false
	}
	return true
}

// closeThread frees the thread's slot on the server. Servers without
// thread/close only get an interrupt, so the abandoned turn stops anyway.
func (c *conv) closeThread(tid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := c.b.app.Call(ctx, "thread/close", map[string]any{"thread_id": tid}, nil)
	switch {
	case err == nil, appclient.IsCode(err, appclient.ErrThreadNotFound):
	case appclient.IsCode(err, appclient.ErrMethodNotFound):
		_ = c.interrupt(tid)
	default:
		slog.Warn("thread/close failed", "thread", tid, "err", err)
	}
}

func (c *conv) interrupt(tid string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.b.app.Call(ctx, "turn/interrupt", map[string]any{"thread_id": tid}, nil)
}

// cancel interrupts the running turn, then each queued (steered) turn in
// turn, since the server would otherwise start the next one.
func (c *conv) cancel() {
	c.mu.Lock()
	tid, n := c.threadID, c.active
	c.mu.Unlock()
	if tid == "" || n == 0 {
		c.reply("Nothing is running.")
		return
	}
	for i := 0; i < n; i++ {
		if err := c.interrupt(tid); err != nil {
			if !appclient.IsCode(err, appclient.ErrNoActiveTurn) {
				c.reply("**Error:** " + err.Error())
			}
			return
		}
		// Wait until the server reports this turn finished before the next interrupt.
		want := n - i - 1
		for w := 0; w < 25; w++ {
			c.mu.Lock()
			left := c.active
			c.mu.Unlock()
			if left <= want {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (c *conv) setMode(args []string) {
	c.mu.Lock()
	cur := c.mode
	c.mu.Unlock()
	if len(args) == 0 {
		c.reply("Permission mode: `" + cur + "`")
		return
	}
	mode := args[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.ensureThread(ctx); err != nil {
		c.reply("**Error:** " + err.Error())
		return
	}
	c.mu.Lock()
	tid := c.threadID
	c.mu.Unlock()
	var res struct {
		Mode string `json:"permission_mode"`
	}
	err := c.b.app.Call(ctx, "approval/respond", map[string]any{"thread_id": tid, "approved": true, "permission_mode": mode}, &res)
	if err != nil {
		c.reply("**Error:** " + err.Error())
		return
	}
	if res.Mode == "" {
		res.Mode = mode
	}
	c.mu.Lock()
	c.mode = res.Mode
	c.mu.Unlock()
	c.persistSettings()
	c.reply("Permission mode: `" + res.Mode + "`")
}
