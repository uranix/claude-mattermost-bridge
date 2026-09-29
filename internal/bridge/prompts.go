package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

// Reactions the bot offers on a permission prompt, and the ones it accepts.
const (
	emojiAllow  = "white_check_mark"
	emojiDeny   = "x"
	emojiAlways = "fast_forward"
)

var (
	allowEmojis  = map[string]bool{emojiAllow: true, "heavy_check_mark": true, "+1": true}
	denyEmojis   = map[string]bool{emojiDeny: true, "-1": true, "no_entry_sign": true}
	alwaysEmojis = map[string]bool{emojiAlways: true}
)

// prompt is a permission request waiting for an answer, shown as a post.
type prompt struct {
	conv      *conv
	requestID string
	threadID  string
	postID    string
	summary   string // one line naming the tool call; what the post collapses to
	tool      string
}

type approvalRequest struct {
	ThreadID    string          `json:"thread_id"`
	RequestID   string          `json:"request_id"`
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Suggestions json.RawMessage `json:"suggestions"`
	ExpiresAt   int64           `json:"expires_at"`
}

// ---- building the message --------------------------------------------------

// fence wraps text in a code fence long enough not to be closed by the text itself.
func fence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "\n" + text + "\n" + f
}

// clip shortens text to at most n runes and lines, marking the cut.
func clip(text string, n, lines int) string {
	text = strings.TrimRight(text, "\n")
	cut := false
	if ls := strings.Split(text, "\n"); len(ls) > lines {
		text, cut = strings.Join(ls[:lines], "\n"), true
	}
	if r := []rune(text); len(r) > n {
		text, cut = string(r[:n]), true
	}
	if cut {
		text += "\n..."
	}
	return text
}

// describeTool renders what a tool call is about to do, in Mattermost markdown.
func describeTool(tool string, raw json.RawMessage, desc string) string {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }

	var sb strings.Builder
	switch tool {
	case "Bash":
		sb.WriteString(fence(clip(str("command"), 1500, 25)))
		if d := str("description"); d != "" {
			sb.WriteString("\n" + d)
		}
	case "Write":
		sb.WriteString("`" + str("file_path") + "`\n" + fence(clip(str("content"), 800, 15)))
	case "Edit":
		sb.WriteString("`" + str("file_path") + "`\nreplace:\n" + fence(clip(str("old_string"), 400, 8)) +
			"\nwith:\n" + fence(clip(str("new_string"), 400, 8)))
	case "MultiEdit":
		n := 0
		if edits, ok := in["edits"].([]any); ok {
			n = len(edits)
		}
		sb.WriteString(fmt.Sprintf("`%s` (%d edits)", str("file_path"), n))
	default:
		if b, err := json.MarshalIndent(in, "", "  "); err == nil && len(in) > 0 {
			sb.WriteString(fence(clip(string(b), 700, 20)))
		}
		if desc != "" {
			sb.WriteString("\n" + desc)
		}
	}
	return sb.String()
}

// summary is the one-line form a prompt collapses to once it is decided.
func (a approvalRequest) summary() string {
	s := "`" + a.ToolName + "`"
	if h := summarizeInput(a.Input); h != "" {
		s += " " + h
	}
	return s
}

func (a approvalRequest) messages() (body, legend string) {
	body = "**Permission requested:** `" + a.ToolName + "`"
	if d := describeTool(a.ToolName, a.Input, a.Description); d != "" {
		body += "\n" + d
	}
	legend = fmt.Sprintf(":%s: allow, :%s: deny, :%s: allow every `%s` call in this conversation. Or `!allow`, `!allow always`, `!deny [reason]`.",
		emojiAllow, emojiDeny, emojiAlways, trustName(a.ToolName))
	if a.ExpiresAt > 0 {
		if left := time.Until(time.UnixMilli(a.ExpiresAt)); left > 0 {
			legend += fmt.Sprintf(" Denied automatically in %d min.", int(left.Minutes())+1)
		}
	}
	return body, legend
}

// ---- trust: tools a conversation has agreed to run without asking -----------

const trustAll = "*"

// editTools are treated as one family: trusting one file-editing tool and being
// asked again for its siblings would defeat the purpose.
var editTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// trustKey maps a tool to what is trusted on its behalf.
func trustKey(tool string) string {
	if editTools[tool] {
		return "Edit"
	}
	return tool
}

// trustName is how a trust key is shown to the user.
func trustName(tool string) string {
	if editTools[tool] {
		return "Edit/Write"
	}
	return tool
}

func (c *conv) trusts(tool string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.trusted[trustAll] || c.trusted[trustKey(tool)]
}

// trust adds a tool (or trustAll) to the conversation's trust list and saves it.
func (c *conv) trust(key string) {
	c.mu.Lock()
	if c.trusted == nil {
		c.trusted = map[string]bool{}
	}
	c.trusted[key] = true
	c.mu.Unlock()
	c.persistSettings()
}

func (c *conv) trustList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k := range c.trusted {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- lifecycle -------------------------------------------------------------

// askPermission queues the prompt post in the conversation's ordered outbox.
func (c *conv) askPermission(a approvalRequest) {
	root := c.replyRoot()
	c.out <- func() { c.deliverPrompt(root, a) }
}

func (c *conv) deliverPrompt(root string, a approvalRequest) {
	body, legend := a.messages()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	post, err := c.b.mm.CreatePost(ctx, c.channelID, root, body+"\n\n"+legend, nil)
	if err != nil {
		slog.Error("permission prompt post failed", "err", err)
		// Nobody can answer, so refuse rather than leave the turn hanging.
		c.b.answerPermission(a.ThreadID, a.RequestID, "deny", false, "The permission prompt could not be shown to the user.")
		return
	}
	p := &prompt{conv: c, requestID: a.RequestID, threadID: a.ThreadID, postID: post.ID, summary: a.summary(), tool: a.ToolName}
	c.b.mu.Lock()
	c.b.byPost[post.ID] = p
	c.b.mu.Unlock()
	c.mu.Lock()
	c.prompts = append(c.prompts, p)
	c.mu.Unlock()

	for _, e := range []string{emojiAllow, emojiDeny, emojiAlways} {
		if err := c.b.mm.AddReaction(ctx, c.b.me.ID, post.ID, e); err != nil {
			slog.Warn("could not add reaction", "emoji", e, "err", err)
		}
	}
}

func (b *Bridge) answerPermission(threadID, requestID, behavior string, apply bool, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	params := map[string]any{"thread_id": threadID, "request_id": requestID, "behavior": behavior}
	if apply {
		params["apply_suggestions"] = true
	}
	if message != "" {
		params["message"] = message
	}
	return b.app.Call(ctx, "permission/respond", params, nil)
}

// takePrompt removes p from both indexes and reports whether it was still pending.
func (b *Bridge) takePrompt(p *prompt) bool {
	b.mu.Lock()
	if b.byPost[p.postID] != p {
		b.mu.Unlock()
		return false
	}
	delete(b.byPost, p.postID)
	b.mu.Unlock()
	c := p.conv
	c.mu.Lock()
	for i, q := range c.prompts {
		if q == p {
			c.prompts = append(c.prompts[:i], c.prompts[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	return true
}

// patchPrompt collapses the prompt to one line: the outcome and what it was about.
func (b *Bridge) patchPrompt(p *prompt, outcome string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.mm.PatchPost(ctx, p.postID, outcome+" "+p.summary); err != nil {
		slog.Warn("could not update the permission prompt", "err", err)
	}
}

// resolvePrompt sends the user's decision to the server and records it on the post.
// With always, the tool is also trusted for the rest of the conversation.
func (b *Bridge) resolvePrompt(p *prompt, behavior string, always bool, message, by string) {
	if !b.takePrompt(p) {
		return // already answered, expired or cancelled
	}
	if err := b.answerPermission(p.threadID, p.requestID, behavior, false, message); err != nil {
		b.patchPrompt(p, "_Could not send the answer ("+err.Error()+"):_")
		return
	}
	var outcome string
	switch {
	case behavior == "deny" && message != "":
		outcome = fmt.Sprintf("**Denied** by @%s (%s):", by, message)
	case behavior == "deny":
		outcome = "**Denied** by @" + by + ":"
	case always:
		p.conv.trust(trustKey(p.tool))
		outcome = fmt.Sprintf("**Allowed** by @%s, and every later `%s` call in this conversation (`!trust off` to revoke):", by, trustName(p.tool))
	default:
		outcome = "**Allowed** by @" + by + ":"
	}
	b.patchPrompt(p, outcome)
}

// expirePrompt records that the server withdrew a request (timeout or interrupt).
func (b *Bridge) expirePrompt(p *prompt, reason string) {
	if !b.takePrompt(p) {
		return
	}
	switch reason {
	case "timeout":
		b.patchPrompt(p, "_No answer in time, denied automatically:_")
	default:
		b.patchPrompt(p, "_Cancelled:_")
	}
}

func (c *conv) findPrompt(requestID string) *prompt {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.prompts {
		if p.requestID == requestID {
			return p
		}
	}
	return nil
}

// dropPrompts forgets every pending prompt, for example after the server connection dropped.
func (c *conv) dropPrompts(outcome string) {
	c.mu.Lock()
	all := append([]*prompt(nil), c.prompts...)
	c.mu.Unlock()
	for _, p := range all {
		if c.b.takePrompt(p) {
			c.b.patchPrompt(p, outcome)
		}
	}
}

// answerOldest handles !allow / !deny: it answers the longest-waiting prompt.
func (c *conv) answerOldest(behavior string, apply bool, message, by string) {
	c.mu.Lock()
	var p *prompt
	if len(c.prompts) > 0 {
		p = c.prompts[0]
	}
	c.mu.Unlock()
	if p == nil {
		c.reply("Nothing is waiting for approval.")
		return
	}
	c.b.resolvePrompt(p, behavior, apply, message, by)
}

// onReaction turns a reaction on a prompt post into a decision.
func (b *Bridge) onReaction(ctx context.Context, r mm.Reaction) {
	if r.UserID == b.me.ID {
		return // the bot's own option reactions
	}
	b.mu.Lock()
	p := b.byPost[r.PostID]
	b.mu.Unlock()
	if p == nil {
		return
	}
	u, ok := b.user(ctx, r.UserID)
	name := u.Username
	if !ok || !b.mayUse(u, strings.HasPrefix(p.conv.key, "ch:")) {
		slog.Debug("ignored reaction from a user without access", "user", name)
		return
	}
	switch {
	case allowEmojis[r.Emoji]:
		b.resolvePrompt(p, "allow", false, "", name)
	case alwaysEmojis[r.Emoji]:
		b.resolvePrompt(p, "allow", true, "", name)
	case denyEmojis[r.Emoji]:
		b.resolvePrompt(p, "deny", false, "", name)
	}
}
