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
	convs    map[string]*conv // by conversation key
	byThread map[string]*conv // by app-server thread id

	users sync.Map // user id -> username cache
}

func New(cfg *config.Config, m *mm.Client, app *appclient.Client, me mm.User) *Bridge {
	return &Bridge{
		cfg: cfg, mm: m, app: app, me: me,
		mention:  regexp.MustCompile(`(?i)(^|[^\w.@-])@` + regexp.QuoteMeta(me.Username) + `\b`),
		convs:    map[string]*conv{},
		byThread: map[string]*conv{},
	}
}

// Run blocks until ctx is done.
func (b *Bridge) Run(ctx context.Context) {
	go b.app.Run(ctx)
	go b.dispatchNotifications(ctx)
	b.mm.Listen(ctx, func(ev mm.Posted) { b.onPosted(ctx, ev) })
}

func (b *Bridge) username(ctx context.Context, id string) (string, bool) {
	if v, ok := b.users.Load(id); ok {
		return v.(string), true
	}
	u, err := b.mm.User(ctx, id)
	if err != nil {
		slog.Warn("user lookup failed", "id", id, "err", err)
		return "", false
	}
	b.users.Store(id, u.Username)
	return u.Username, true
}

func (b *Bridge) onPosted(ctx context.Context, ev mm.Posted) {
	p := ev.Post
	if p.UserID == b.me.ID || p.Type != "" { // own posts and system messages
		return
	}
	name, ok := b.username(ctx, p.UserID)
	if !ok || !b.cfg.AllowedUsers[strings.ToLower(name)] {
		slog.Debug("ignored post from non-allowed user", "user", name)
		return
	}

	direct := ev.ChannelType == "D"
	mentioned := b.mention.MatchString(p.Message)
	for _, id := range ev.Mentions {
		if id == b.me.ID {
			mentioned = true
		}
	}

	key := "dm:" + p.ChannelID
	rootID := "" // DMs are flat: replies are not threaded
	if !direct {
		if !b.cfg.AllowChannels {
			return
		}
		root := p.RootID
		if root == "" {
			root = p.ID
		}
		key = "ch:" + p.ChannelID + ":" + root
		rootID = root
		b.mu.Lock()
		_, known := b.convs[key]
		b.mu.Unlock()
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
		c = &conv{b: b, key: key, channelID: channelID, mode: b.cfg.PermissionMode, inbox: make(chan inbound, 64)}
		b.convs[key] = c
		go c.worker()
	}
	return c
}

// post sends text to a conversation, splitting it to fit Mattermost limits.
func (b *Bridge) post(channelID, rootID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, part := range mm.Split(text, mm.MaxPostRunes) {
		if err := b.mm.CreatePost(ctx, channelID, rootID, part); err != nil {
			slog.Error("post failed", "channel", channelID, "err", err)
			return
		}
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
		ThreadID  string          `json:"thread_id"`
		TurnID    string          `json:"turn_id"`
		Status    string          `json:"status"`
		Error     string          `json:"error"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		Item      struct {
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
			c.reply(it.Text)
		case "tool_call":
			if b.cfg.ShowTools {
				c.reply("`" + it.Name + "` " + summarizeInput(it.Input))
			}
		}
	case "turn/completed":
		c.turnDone()
		if p.Status == "interrupted" {
			c.reply("_Interrupted._")
		}
	case "turn/error":
		c.turnDone()
		c.reply("**Error:** " + p.Error)
	case "turn/permission_denied":
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
