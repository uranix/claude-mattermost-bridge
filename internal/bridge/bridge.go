// Package bridge routes Mattermost posts to claude-app-server threads and
// posts the agent's output back.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
	"github.com/uranix/claude-mattermost-bridge/internal/config"
	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

type Bridge struct {
	cfg     *config.Config
	mm      *mm.Client
	app     *appclient.Client
	me      mm.User
	mention *regexp.Regexp

	mu       sync.Mutex
	convs    map[string]*conv   // by conversation key
	byThread map[string]*conv   // by app-server thread id
	byPost   map[string]*prompt // permission prompts waiting for an answer, by post id

	state *store

	users sync.Map // user id -> username cache
}

func New(cfg *config.Config, m *mm.Client, app *appclient.Client, me mm.User) (*Bridge, error) {
	st, err := openStore(cfg.StateFile)
	if err != nil {
		return nil, fmt.Errorf("state file: %w", err)
	}
	return &Bridge{
		cfg: cfg, mm: m, app: app, me: me, state: st,
		mention:  regexp.MustCompile(`(?i)(^|[^\w.@-])@` + regexp.QuoteMeta(me.Username) + `\b`),
		convs:    map[string]*conv{},
		byThread: map[string]*conv{},
		byPost:   map[string]*prompt{},
	}, nil
}

// Run blocks until ctx is done.
func (b *Bridge) Run(ctx context.Context) {
	go b.app.Run(ctx)
	go b.dispatchNotifications(ctx)
	go b.janitor(ctx)
	b.mm.Listen(ctx, func(ev mm.Posted) { b.onPosted(ctx, ev) }, func(r mm.Reaction) { go b.onReaction(ctx, r) })
}

func (b *Bridge) user(ctx context.Context, id string) (mm.User, bool) {
	if v, ok := b.users.Load(id); ok {
		return v.(mm.User), true
	}
	u, err := b.mm.User(ctx, id)
	if err != nil {
		slog.Warn("user lookup failed", "id", id, "err", err)
		return mm.User{}, false
	}
	b.users.Store(id, u)
	return u, true
}

// mayUse says whether a user may use the bot, for everything: chatting, approving
// tools, changing settings. DMs are limited to MM_ALLOWED_USERS. In a channel the
// channel allowlist has already been applied, and every human there is equal
// (other bots never count).
func (b *Bridge) mayUse(u mm.User, inChannel bool) bool {
	if inChannel {
		return !u.IsBot
	}
	return b.cfg.AllowedUsers[strings.ToLower(u.Username)]
}

// channelAllowed reports whether the bot may answer in this channel, by ID or name.
func (b *Bridge) channelAllowed(ev mm.Posted) bool {
	return b.cfg.AllowedChannels[strings.ToLower(ev.Post.ChannelID)] ||
		(ev.ChannelName != "" && b.cfg.AllowedChannels[strings.ToLower(ev.ChannelName)])
}

func (b *Bridge) username(ctx context.Context, id string) (string, bool) {
	u, ok := b.user(ctx, id)
	return u.Username, ok
}

func (b *Bridge) onPosted(ctx context.Context, ev mm.Posted) {
	p := ev.Post
	if p.UserID == b.me.ID || p.Type != "" { // own posts and system messages
		return
	}
	u, ok := b.user(ctx, p.UserID)
	if !ok {
		return
	}
	name := u.Username
	direct := ev.ChannelType == "D"
	if !direct && !b.channelAllowed(ev) {
		slog.Debug("ignored post in a channel that is not allowed", "channel", ev.ChannelName)
		return
	}
	if !b.mayUse(u, !direct) {
		slog.Debug("ignored post from a user without access", "user", name)
		return
	}
	mentioned := b.mention.MatchString(p.Message)
	for _, id := range ev.Mentions {
		if id == b.me.ID {
			mentioned = true
		}
	}

	key := "dm:" + p.ChannelID
	rootID := "" // DMs are flat: replies are not threaded
	if !direct {
		root := p.RootID
		if root == "" {
			root = p.ID
		}
		key = "ch:" + p.ChannelID + ":" + root
		rootID = root
		b.mu.Lock()
		_, known := b.convs[key]
		b.mu.Unlock()
		if _, saved := b.state.get(key); saved {
			known = true // engaged before a restart
		}
		if !mentioned && !known { // follow-ups in an engaged thread need no mention
			return
		}
	}

	text := strings.TrimSpace(b.mention.ReplaceAllString(p.Message, "$1"))
	c := b.getConv(key, p.ChannelID)
	c.enqueue(inbound{post: p, text: text, user: name, rootID: rootID, direct: direct})
}

func (b *Bridge) getConv(key, channelID string) *conv {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.convs[key]
	if !ok {
		mode := b.cfg.PermissionMode
		if b.cfg.ChannelMode != "" && strings.HasPrefix(key, "ch:") {
			mode = b.cfg.ChannelMode
		}
		c = &conv{b: b, key: key, channelID: channelID, mode: mode, model: b.cfg.Model, trusted: map[string]bool{}, inbox: make(chan inbound, 64), out: make(chan func(), 256)}
		if st, ok := b.state.get(key); ok {
			c.cliSessionID = st.CliSessionID
			c.model = st.Model // "" means the user chose the default
			for _, k := range st.Trusted {
				c.trusted[k] = true
			}
			if st.Mode != "" {
				c.mode = st.Mode
			}
		}
		b.convs[key] = c
		go c.worker()
		go c.outWorker()
	}
	return c
}

// post sends text to a conversation, splitting it to fit Mattermost limits.
func (b *Bridge) post(channelID, rootID, text string) {
	b.postFiles(channelID, rootID, text, nil)
}

// postFiles is post with attachments: the files ride on the last text chunk,
// or on posts of their own when there is no text or more than the per-post limit.
func (b *Bridge) postFiles(channelID, rootID, text string, fileIDs []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	parts := mm.Split(text, mm.MaxPostRunes)
	if len(parts) == 0 && len(fileIDs) > 0 {
		parts = []string{""}
	}
	for i, part := range parts {
		var ids []string
		if i == len(parts)-1 {
			n := min(len(fileIDs), mm.MaxFilesPerPost)
			ids, fileIDs = fileIDs[:n], fileIDs[n:]
		}
		if _, err := b.mm.CreatePost(ctx, channelID, rootID, part, ids); err != nil {
			slog.Error("post failed", "channel", channelID, "err", err)
			return
		}
		if i < len(parts)-1 || len(fileIDs) > 0 {
			b.retypeChannel(channelID) // more posts follow in this call
		}
	}
	for len(fileIDs) > 0 { // more files than fit on one post
		n := min(len(fileIDs), mm.MaxFilesPerPost)
		if _, err := b.mm.CreatePost(ctx, channelID, rootID, "", fileIDs[:n]); err != nil {
			slog.Error("post failed", "channel", channelID, "err", err)
			return
		}
		fileIDs = fileIDs[n:]
		if len(fileIDs) > 0 {
			b.retypeChannel(channelID)
		}
	}
}

// retypeChannel refreshes the typing indicator of the busy conversations of a
// channel after an intermediate post of a multi-post reply.
func (b *Bridge) retypeChannel(channelID string) {
	b.mu.Lock()
	var cs []*conv
	for _, c := range b.convs {
		if c.channelID == channelID {
			cs = append(cs, c)
		}
	}
	b.mu.Unlock()
	for _, c := range cs {
		c.sendTyping()
	}
}

// ---- notifications from the app server ------------------------------------

func (b *Bridge) dispatchNotifications(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-b.app.Notifications():
			b.handleNotification(n)
		}
	}
}

func (b *Bridge) handleNotification(n appclient.Notification) {
	if n.Method == appclient.ResetMethod {
		b.mu.Lock()
		all := make([]*conv, 0, len(b.convs))
		for _, c := range b.convs {
			all = append(all, c)
		}
		b.byThread = map[string]*conv{}
		b.mu.Unlock()
		for _, c := range all {
			c.onServerReset()
		}
		return
	}

	var p struct {
		ThreadID   string          `json:"thread_id"`
		RequestID  string          `json:"request_id"`
		Reason     string          `json:"reason"`
		MessageID  string          `json:"message_id"`
		Trigger    string          `json:"trigger"`
		PreTokens  int             `json:"pre_tokens"`
		PostTokens int             `json:"post_tokens"`
		TurnID     string          `json:"turn_id"`
		Status     string          `json:"status"`
		Error      string          `json:"error"`
		ToolName   string          `json:"tool_name"`
		ToolInput  json.RawMessage `json:"tool_input"`
		Item       struct {
			Item struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"item"`
		} `json:"item"`
	}
	if err := json.Unmarshal(n.Params, &p); err != nil || p.ThreadID == "" {
		return
	}
	b.mu.Lock()
	c := b.byThread[p.ThreadID]
	b.mu.Unlock()
	if c == nil {
		return // thread was replaced by !new, or belongs to nobody
	}

	switch n.Method {
	case "item/created":
		it := p.Item.Item
		switch it.Type {
		case "text":
			c.replyAgent(it.Text)
		case "tool_call":
			if b.cfg.ShowTools {
				c.reply("`" + it.Name + "` " + summarizeInput(it.Input))
			}
		}
	case "context/compacting":
		c.reply("_Compacting the context..._")
	case "context/compacted":
		how := "automatic"
		if p.Trigger == "manual" {
			how = "manual"
		}
		c.reply(fmt.Sprintf("_Context compacted (%s): %s -> %s tokens._", how, humanTokens(p.PreTokens), humanTokens(p.PostTokens)))
	case "message/consumed":
		c.messageConsumed(p.MessageID)
	case "turn/completed":
		c.turnDone()
		c.sessionRan()
		if p.Status == "interrupted" {
			c.reply("_Interrupted._")
		}
	case "turn/error":
		c.turnDone()
		if c.resumeFailed() {
			c.reply("Could not resume the previous Claude session, so I started a fresh context. Please resend your message.")
			return
		}
		c.reply("**Error:** " + p.Error)
	case "approval/requested":
		var a approvalRequest
		if json.Unmarshal(n.Params, &a) != nil {
			return
		}
		if c.trusts(a.ToolName) {
			// The user already agreed to this tool in this conversation: no post at all.
			if err := b.answerPermission(a.ThreadID, a.RequestID, "allow", false, ""); err != nil {
				slog.Warn("auto-allow failed", "tool", a.ToolName, "err", err)
			}
			return
		}
		c.askPermission(a)
	case "approval/cancelled":
		if pr := c.findPrompt(p.RequestID); pr != nil {
			b.expirePrompt(pr, p.Reason)
		}
	case "turn/permission_denied":
		if b.cfg.PermissionPrompts {
			return // the user was asked; the outcome is on the prompt
		}
		c.reply(fmt.Sprintf("Permission denied for `%s` %s\nIf this is fine, run `!mode acceptEdits` (or `!mode bypassPermissions` if the server allows it) and repeat the request.",
			p.ToolName, summarizeInput(p.ToolInput)))
	}
}

// summarizeInput renders a tool input as a short single-line hint.
func summarizeInput(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description"} {
		if s, ok := m[k].(string); ok && s != "" {
			s = strings.Join(strings.Fields(s), " ")
			if r := []rune(s); len(r) > 120 {
				s = string(r[:120]) + "..."
			}
			return "`" + strings.ReplaceAll(s, "`", "'") + "`"
		}
	}
	return ""
}

// humanTokens formats a token count as "36.7k".
func humanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
