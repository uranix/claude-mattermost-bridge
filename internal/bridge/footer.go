package bridge

import (
	"fmt"
	"regexp"
	"strings"
)

// usageInfo is what the footer of each agent message shows.
type usageInfo struct {
	model   string // as the CLI reports it, e.g. claude-opus-5-5
	effort  string
	window  int   // context window, tokens
	context int   // tokens in the context now
	in, out int64 // tokens read and written by the conversation so far
}

// addUsage applies a thread/usage notification.
func (c *conv) addUsage(model string, context, in, out int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if model != "" {
		c.usage.model = model
	}
	if context > 0 {
		c.usage.context = context
	}
	c.usage.in += int64(in)
	c.usage.out += int64(out)
}

// setSettings applies a thread/settings notification.
func (c *conv) setSettings(model, effort string, window int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if model != "" {
		c.usage.model = model
	}
	c.usage.effort = effort
	if window > 0 {
		c.usage.window = window
	}
}

func (c *conv) footer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage.footer()
}

// footer renders the usage line, e.g.
// "_opus 5.5 | medium | ctx 45.2k/1M | tokens 1.2M in, 38.0k out_".
// Parts not known yet are left out; "" when nothing is.
func (u usageInfo) footer() string {
	var parts []string
	if u.model != "" {
		parts = append(parts, shortModel(u.model))
	}
	if u.effort != "" {
		parts = append(parts, u.effort)
	}
	if u.context > 0 {
		ctx := "ctx " + shortTokens(int64(u.context))
		if u.window > 0 {
			ctx += "/" + shortTokens(int64(u.window))
		}
		parts = append(parts, ctx)
	}
	if u.in > 0 || u.out > 0 {
		parts = append(parts, fmt.Sprintf("tokens %s in, %s out", shortTokens(u.in), shortTokens(u.out)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "_" + strings.Join(parts, " | ") + "_"
}

var modelName = regexp.MustCompile(`^claude-([a-z]+)-(\d+)-(\d+)(?:-\d{8})?(.*)$`)

// shortModel turns claude-opus-5-5 into "opus 5.5" and claude-haiku-4-5-20251001
// into "haiku 4.5"; other names lose only the "claude-" prefix.
func shortModel(m string) string {
	if s := modelName.FindStringSubmatch(m); s != nil {
		return s[1] + " " + s[2] + "." + s[3] + s[4]
	}
	return strings.TrimPrefix(m, "claude-")
}

// shortTokens renders a token count as 950, 45.2k, 1.2M or 1M.
func shortTokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	case n%1000000 == 0:
		return fmt.Sprintf("%dM", n/1000000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
}
