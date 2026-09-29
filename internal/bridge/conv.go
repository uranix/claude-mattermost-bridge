package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
	"github.com/uranix/claude-mattermost-bridge/internal/config"
	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

type inbound struct {
	post   mm.Post
	text   string
	user   string
	rootID string // where replies go ("" in DMs)
	direct bool
}

// conv is one conversation: a DM, or one Mattermost thread in a channel.
// Its inbox is drained by a single worker so messages are handled in order.
type conv struct {
	b         *Bridge
	key       string
	channelID string
	inbox     chan inbound

	mu         sync.Mutex
	threadID   string
	mode       string
	active     int    // turns sent to the app server and not yet finished
	rootID     string // reply target of the latest inbound message
	stopTyping context.CancelFunc
}

func (c *conv) enqueue(in inbound) {
	select {
	case c.inbox <- in:
	default:
		c.b.post(c.channelID, in.rootID, "Too many queued messages, dropping this one.")
	}
}

func (c *conv) worker() {
	for in := range c.inbox {
		c.handle(in)
	}
}

func (c *conv) reply(text string) {
	c.mu.Lock()
	root := c.rootID
	c.mu.Unlock()
	c.b.post(c.channelID, root, text)
}

func (c *conv) handle(in inbound) {
	c.mu.Lock()
	c.rootID = in.rootID
	c.mu.Unlock()

	if strings.HasPrefix(in.text, "!") {
		if c.command(in) {
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	prompt := in.text
	if files := c.downloadFiles(ctx, in.post); files != "" {
		prompt += "\n\n" + files
	}
	if strings.TrimSpace(prompt) == "" {
		return
	}
	if !in.direct {
		prompt = "@" + in.user + ": " + prompt // several people can share a channel thread
	}

	if err := c.send(ctx, prompt); err != nil {
		slog.Error("send failed", "conv", c.key, "err", err)
		c.reply("**Error:** " + err.Error())
	}
}

// send starts a turn, or steers the running one when the agent is busy.
func (c *conv) send(ctx context.Context, prompt string) error {
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.ensureThread(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		tid := c.threadID
		method := "turn/start"
		if c.active > 0 {
			method = "turn/steer"
		}
		c.active++
		startTyping := c.active == 1
		c.mu.Unlock()
		if startTyping {
			c.beginTyping()
		}

		params := map[string]any{"thread_id": tid, "content": prompt}
		err := c.b.app.Call(ctx, method, params, nil)
		if err == nil {
			return nil
		}
		c.turnDone() // undo the optimistic increment

		switch {
		case appclient.IsCode(err, appclient.ErrThreadNotFound):
			c.dropThread(tid)
		case appclient.IsCode(err, appclient.ErrTurnBusy):
			c.markBusy()
		case appclient.IsCode(err, appclient.ErrNoActiveTurn):
			c.markIdle()
		default:
			return err
		}
	}
	return errors.New("could not deliver the message to the agent")
}

// markBusy/markIdle correct our view when it disagrees with the server.
func (c *conv) markBusy() {
	c.mu.Lock()
	if c.active == 0 {
		c.active = 1
	}
	c.mu.Unlock()
}

func (c *conv) markIdle() {
	c.mu.Lock()
	c.active = 0
	c.mu.Unlock()
}

func (c *conv) ensureThread(ctx context.Context) error {
	c.mu.Lock()
	tid, mode := c.threadID, c.mode
	c.mu.Unlock()
	if tid != "" {
		return nil
	}
	params := map[string]any{"permission_mode": mode}
	if c.b.cfg.Cwd != "" {
		params["cwd"] = c.b.cfg.Cwd
	}
	var res struct {
		ThreadID string `json:"thread_id"`
	}
	if err := c.b.app.Call(ctx, "thread/start", params, &res); err != nil {
		return fmt.Errorf("thread/start: %w", err)
	}
	c.mu.Lock()
	c.threadID = res.ThreadID
	c.mu.Unlock()
	c.b.mu.Lock()
	c.b.byThread[res.ThreadID] = c
	c.b.mu.Unlock()
	return nil
}

// dropThread forgets a thread that the server no longer knows or that !new replaced.
func (c *conv) dropThread(tid string) {
	c.mu.Lock()
	if c.threadID == tid {
		c.threadID = ""
	}
	c.mu.Unlock()
	c.b.mu.Lock()
	delete(c.b.byThread, tid)
	c.b.mu.Unlock()
}

func (c *conv) onServerReset() {
	c.mu.Lock()
	hadWork := c.active > 0
	c.threadID = ""
	c.active = 0
	stop := c.stopTyping
	c.stopTyping = nil
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if hadWork {
		c.reply("_Connection to the agent server was lost; the running request was aborted and the context reset._")
	}
}

func (c *conv) turnDone() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	var stop context.CancelFunc
	if c.active == 0 {
		stop, c.stopTyping = c.stopTyping, nil
	}
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// beginTyping keeps the typing indicator alive until the last turn finishes.
func (c *conv) beginTyping() {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.stopTyping != nil {
		c.stopTyping()
	}
	c.stopTyping = cancel
	c.mu.Unlock()
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			c.mu.Lock()
			root := c.rootID
			c.mu.Unlock()
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			_ = c.b.mm.Typing(rctx, c.b.me.ID, c.channelID, root)
			rcancel()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// ---- attachments -----------------------------------------------------------

var unsafeName = regexp.MustCompile(`[^\w.\-]+`)

// downloadFiles saves a post's attachments locally and describes them for the
// agent, which shares the filesystem with the bridge.
func (c *conv) downloadFiles(ctx context.Context, p mm.Post) string {
	if len(p.FileIDs) == 0 {
		return ""
	}
	dir, err := filepath.Abs(c.b.cfg.AttachDir)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return "(attachments could not be saved: " + err.Error() + ")"
	}
	var lines []string
	for _, id := range p.FileIDs {
		fi, err := c.b.mm.FileInfo(ctx, id)
		if err != nil {
			lines = append(lines, "- "+id+": unavailable ("+err.Error()+")")
			continue
		}
		if fi.Size > config.MaxFileBytes {
			lines = append(lines, fmt.Sprintf("- %s: skipped, %d bytes exceeds the limit", fi.Name, fi.Size))
			continue
		}
		dest := filepath.Join(dir, id+"-"+unsafeName.ReplaceAllString(filepath.Base(fi.Name), "_"))
		if err := c.b.mm.DownloadFile(ctx, id, dest); err != nil {
			lines = append(lines, "- "+fi.Name+": download failed ("+err.Error()+")")
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (original name %q, %s, %d bytes)", dest, fi.Name, fi.MimeType, fi.Size))
	}
	return "Attached files (local paths):\n" + strings.Join(lines, "\n")
}
