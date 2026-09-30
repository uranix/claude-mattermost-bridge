package bridge

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
	"github.com/uranix/claude-mattermost-bridge/internal/config"
	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

// reactionDelay is a var so tests can shorten it. The delay lets clients
// replace their pending copy of a post before the reaction arrives; the
// desktop client can drop a reaction that comes too early.
var reactionDelay = 300 * time.Millisecond

// newMessageID returns a random UUIDv4, which the app server passes to the CLI
// to learn when the message is consumed.
func newMessageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

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
	out       chan func() // ordered outgoing posts, so slow uploads only delay this conversation

	opMu sync.Mutex // held while handling a message or idle-closing, so the two never overlap

	mu           sync.Mutex
	lastActive   time.Time
	threadID     string
	mode         string
	model        string            // "" = server/CLI default
	cliSessionID string            // Claude CLI session behind threadID; survives restarts via the state file
	attached     bool              // threadID was attached to a saved session and has not completed a turn yet
	awaiting     map[string]string // message_id -> post ID, sent to the agent and not yet consumed
	prompts      []*prompt         // permission prompts waiting for an answer, oldest first
	trusted      map[string]bool   // tools this conversation runs without asking (trustKey, or trustAll)
	active       int               // turns sent to the app server and not yet finished
	rootID       string            // reply target of the latest inbound message
	stopTyping   context.CancelFunc
	typingKick   chan struct{} // asks the typing loop for an event now
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

func (c *conv) outWorker() {
	for job := range c.out {
		job()
		c.retype()
	}
}

func (c *conv) replyRoot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rootID
}

// reply queues a plain message. The reply target is fixed at call time.
func (c *conv) reply(text string) {
	root := c.replyRoot()
	c.out <- func() { c.b.post(c.channelID, root, text) }
}

// replyAgent queues a text item of the agent, which may reference files to upload.
func (c *conv) replyAgent(text string) {
	root := c.replyRoot()
	c.out <- func() { c.deliverAgentText(root, text) }
}

func (c *conv) handle(in inbound) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	c.rootID = in.rootID
	c.lastActive = time.Now()
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

	if err := c.send(ctx, prompt, in.post.ID); err != nil {
		slog.Error("send failed", "conv", c.key, "err", err)
		c.reply("**Error:** " + err.Error())
		return
	}
}

// send starts a turn, or steers the running one when the agent is busy. The
// post gets an :eyes: reaction once the agent actually consumes the message
// (see messageConsumed), which for a steered message can be well after this returns.
func (c *conv) send(ctx context.Context, prompt, postID string) error {
	msgID := newMessageID()
	c.mu.Lock()
	if c.awaiting == nil {
		c.awaiting = map[string]string{}
	}
	c.awaiting[msgID] = postID // before the call, so a fast notification cannot be missed
	c.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			c.mu.Lock()
			delete(c.awaiting, msgID)
			c.mu.Unlock()
		}
	}()

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

		params := map[string]any{"thread_id": tid, "content": prompt, "message_id": msgID}
		err := c.b.app.Call(ctx, method, params, nil)
		if err == nil {
			ok = true
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
	tid, mode, sid, model := c.threadID, c.mode, c.cliSessionID, c.model
	c.mu.Unlock()
	if tid != "" {
		return nil
	}
	params := map[string]any{"permission_mode": mode}
	if model != "" {
		params["model"] = model
	}
	if c.b.cfg.SendFiles {
		params["append_system_prompt"] = filePrompt
	}
	if c.b.cfg.PermissionPrompts {
		params["permission_prompts"] = true
	}
	if c.b.cfg.Cwd != "" {
		params["cwd"] = c.b.cfg.Cwd
	}
	var res struct {
		ThreadID     string `json:"thread_id"`
		CliSessionID string `json:"cli_session_id"`
	}
	attached := false
	if sid != "" {
		params["cli_session_id"] = sid
		err := c.b.app.Call(ctx, "thread/attach", params, &res)
		switch {
		case err == nil:
			attached = true
		case appclient.IsCode(err, appclient.ErrMethodNotFound):
			slog.Warn("app server has no thread/attach; context cannot be resumed", "conv", c.key)
		default:
			return fmt.Errorf("thread/attach: %w", err)
		}
		delete(params, "cli_session_id")
	}
	if !attached {
		if err := c.b.app.Call(ctx, "thread/start", params, &res); err != nil {
			return fmt.Errorf("thread/start: %w", err)
		}
		if res.CliSessionID == "" {
			res.CliSessionID = res.ThreadID // servers before thread/attach: session id == thread id
		}
	}
	c.mu.Lock()
	c.threadID = res.ThreadID
	c.cliSessionID = res.CliSessionID
	c.attached = attached
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
	c.awaiting = nil // the agent process is gone, these will never be consumed
	stop := c.stopTyping
	c.stopTyping = nil
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	c.dropPrompts("_The connection to the agent was lost: this request was dropped._")
	if hadWork {
		c.reply("_Connection to the agent server was lost and the running request was aborted. Your next message resumes the conversation._")
	}
}

// messageConsumed reacts with :eyes: to the post whose message the agent just picked up.
func (c *conv) messageConsumed(msgID string) {
	c.mu.Lock()
	postID, ok := c.awaiting[msgID]
	delete(c.awaiting, msgID)
	c.mu.Unlock()
	if !ok {
		return
	}
	go func() {
		time.Sleep(reactionDelay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.b.mm.AddReaction(ctx, c.b.me.ID, postID, "eyes"); err != nil {
			slog.Warn("could not add eyes reaction", "err", err)
		}
	}()
}

func (c *conv) turnDone() {
	c.mu.Lock()
	c.lastActive = time.Now()
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

// sessionRan records that the CLI session now exists on disk (a turn finished),
// which is the earliest point at which it can be resumed after a restart.
func (c *conv) sessionRan() {
	c.mu.Lock()
	c.attached = false
	st := convState{ChannelID: c.channelID, CliSessionID: c.cliSessionID, Mode: c.mode, Model: c.model, Trusted: c.trustedLocked()}
	c.mu.Unlock()
	if st.CliSessionID != "" {
		c.b.state.put(c.key, st)
	}
}

// persistSettings saves mode and model. The session id is only ever written by
// sessionRan, because a session that has not completed a turn cannot be resumed.
func (c *conv) persistSettings() {
	c.mu.Lock()
	mode, model, trusted := c.mode, c.model, c.trustedLocked()
	c.mu.Unlock()
	st, _ := c.b.state.get(c.key)
	st.ChannelID, st.Mode, st.Model, st.Trusted = c.channelID, mode, model, trusted
	c.b.state.put(c.key, st)
}

// resumeFailed handles a turn error on a freshly attached session: the saved
// session is gone or unreadable, so forget it. Reports whether that was the case.
func (c *conv) resumeFailed() bool {
	c.mu.Lock()
	failed, tid := c.attached, c.threadID
	if failed {
		c.attached = false
		c.cliSessionID = ""
	}
	c.mu.Unlock()
	if !failed {
		return false
	}
	c.b.state.delete(c.key)
	c.dropThread(tid)
	return true
}

// trustedLocked returns the trust list sorted; the caller holds c.mu.
func (c *conv) trustedLocked() []string {
	var out []string
	for k := range c.trusted {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
