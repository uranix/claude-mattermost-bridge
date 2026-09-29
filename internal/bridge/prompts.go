package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	body      string // the post without its instructions; outcomes are appended to it
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

// summarizeSuggestions turns the CLI's suggested permission updates into short
// phrases. Rules saved to settings files are called out because they persist.
func summarizeSuggestions(raw json.RawMessage) []string {
	var sugg []struct {
		Type        string   `json:"type"`
		Mode        string   `json:"mode"`
		Destination string   `json:"destination"`
		Directories []string `json:"directories"`
		Rules       []struct {
			ToolName    string `json:"toolName"`
			RuleContent string `json:"ruleContent"`
		} `json:"rules"`
	}
	if json.Unmarshal(raw, &sugg) != nil {
		return nil
	}
	var out []string
	for _, s := range sugg {
		switch s.Type {
		case "setMode":
			if s.Mode == "acceptEdits" {
				out = append(out, "accept file edits without asking")
			} else {
				out = append(out, "switch to "+s.Mode+" mode")
			}
		case "addRules":
			for _, r := range s.Rules {
				rule := r.ToolName
				if r.RuleContent != "" {
					rule += "(" + clip(r.RuleContent, 60, 1) + ")"
				}
				txt := "always allow `" + strings.ReplaceAll(rule, "`", "'") + "`"
				if s.Destination != "" && s.Destination != "session" {
					txt += " (saved to " + s.Destination + ")"
				}
				out = append(out, txt)
			}
		case "addDirectories":
			out = append(out, fmt.Sprintf("allow access to %d more director(y/ies)", len(s.Directories)))
		}
	}
	return out
}

func (a approvalRequest) messages() (body, legend string, withAlways bool) {
	body = "**Permission requested:** `" + a.ToolName + "`"
	if d := describeTool(a.ToolName, a.Input, a.Description); d != "" {
		body += "\n" + d
	}
	sugg := summarizeSuggestions(a.Suggestions)
	legend = fmt.Sprintf("React :%s: to allow, :%s: to deny", emojiAllow, emojiDeny)
	if len(sugg) > 0 {
		legend += fmt.Sprintf(", :%s: to allow and %s", emojiAlways, strings.Join(sugg, " and "))
		withAlways = true
	}
	legend += ". Or reply `!allow`, `!allow always`, `!deny [reason]`."
	if a.ExpiresAt > 0 {
		if left := time.Until(time.UnixMilli(a.ExpiresAt)); left > 0 {
			legend += fmt.Sprintf(" Denied automatically in %d min.", int(left.Minutes())+1)
		}
	}
	return body, legend, withAlways
}

// ---- lifecycle -------------------------------------------------------------

// askPermission queues the prompt post in the conversation's ordered outbox.
func (c *conv) askPermission(a approvalRequest) {
	root := c.replyRoot()
	c.out <- func() { c.deliverPrompt(root, a) }
}

func (c *conv) deliverPrompt(root string, a approvalRequest) {
	body, legend, withAlways := a.messages()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	post, err := c.b.mm.CreatePost(ctx, c.channelID, root, body+"\n\n"+legend, nil)
	if err != nil {
		slog.Error("permission prompt post failed", "err", err)
		// Nobody can answer, so refuse rather than leave the turn hanging.
		c.b.answerPermission(a.ThreadID, a.RequestID, "deny", false, "The permission prompt could not be shown to the user.")
		return
	}
	p := &prompt{conv: c, requestID: a.RequestID, threadID: a.ThreadID, postID: post.ID, body: body}
	c.b.mu.Lock()
	c.b.byPost[post.ID] = p
	c.b.mu.Unlock()
	c.mu.Lock()
	c.prompts = append(c.prompts, p)
	c.mu.Unlock()

	emojis := []string{emojiAllow, emojiDeny}
	if withAlways {
		emojis = append(emojis, emojiAlways)
	}
	for _, e := range emojis {
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

func (b *Bridge) patchPrompt(p *prompt, outcome string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.mm.PatchPost(ctx, p.postID, p.body+"\n\n"+outcome); err != nil {
		slog.Warn("could not update the permission prompt", "err", err)
	}
}

// resolvePrompt sends the user's decision to the server and records it on the post.
func (b *Bridge) resolvePrompt(p *prompt, behavior string, apply bool, message, by string) {
	if !b.takePrompt(p) {
		return // already answered, expired or cancelled
	}
	if err := b.answerPermission(p.threadID, p.requestID, behavior, apply, message); err != nil {
		b.patchPrompt(p, "_Could not send the answer: "+err.Error()+"_")
		return
	}
	var outcome string
	switch {
	case behavior == "deny" && message != "":
		outcome = fmt.Sprintf("**Denied** by @%s: %s", by, message)
	case behavior == "deny":
		outcome = "**Denied** by @" + by
	case apply:
		outcome = "**Allowed** by @" + by + " (and applied the suggested rule)"
	default:
		outcome = "**Allowed** by @" + by
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
		b.patchPrompt(p, "_No answer in time: denied automatically._")
	default:
		b.patchPrompt(p, "_Cancelled._")
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
	name, ok := b.username(ctx, r.UserID)
	if !ok || !b.cfg.AllowedUsers[strings.ToLower(name)] {
		slog.Debug("ignored reaction from non-allowed user", "user", name)
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
