package bridge

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
)

type modelInfo struct {
	Value         string `json:"value"`
	ResolvedModel string `json:"resolved_model"`
	DisplayName   string `json:"display_name"`
	Description   string `json:"description"`
}

// fullModelID matches explicit IDs such as claude-opus-5-5 or claude-sonnet-5-5[1m],
// which are accepted even when the server's list does not mention them.
var fullModelID = regexp.MustCompile(`^claude-[a-z0-9][a-z0-9.\-]*(\[1m\])?$`)

// resolveModel maps what the user typed to a model value. Exact matches win;
// otherwise a case-insensitive partial name must identify exactly one model.
// With several candidates it returns them all and ok=false.
func resolveModel(query string, models []modelInfo) (value string, candidates []modelInfo, ok bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "default" {
		return "default", nil, true
	}
	for _, m := range models {
		if strings.ToLower(m.Value) == q || (m.ResolvedModel != "" && strings.ToLower(m.ResolvedModel) == q) {
			return m.Value, nil, true
		}
	}
	for _, m := range models {
		if strings.Contains(strings.ToLower(m.Value), q) ||
			strings.Contains(strings.ToLower(m.DisplayName), q) ||
			strings.Contains(strings.ToLower(m.ResolvedModel), q) {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 1 {
		return candidates[0].Value, nil, true
	}
	if len(candidates) == 0 && fullModelID.MatchString(q) {
		return q, nil, true
	}
	return "", candidates, false
}

// listModels asks the server for the CLI's model list. Servers that only
// return names still work, with fewer details.
func (c *conv) listModels(ctx context.Context) ([]modelInfo, error) {
	var res struct {
		Models    []string    `json:"models"`
		ModelInfo []modelInfo `json:"model_info"`
	}
	if err := c.b.app.Call(ctx, "model/list", nil, &res); err != nil {
		return nil, err
	}
	if len(res.ModelInfo) > 0 {
		return res.ModelInfo, nil
	}
	out := make([]modelInfo, 0, len(res.Models))
	for _, n := range res.Models {
		out = append(out, modelInfo{Value: n})
	}
	return out, nil
}

func formatModels(models []modelInfo, current string) string {
	var sb strings.Builder
	for _, m := range models {
		mark := ""
		if m.Value == current {
			mark = " (selected)"
		}
		fmt.Fprintf(&sb, "- `%s`", m.Value)
		if m.DisplayName != "" {
			sb.WriteString(" - " + m.DisplayName)
		}
		if m.Description != "" {
			sb.WriteString(": " + m.Description)
		}
		sb.WriteString(mark + "\n")
	}
	return sb.String()
}

func (c *conv) modelCommand(args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c.mu.Lock()
	cur := c.model
	c.mu.Unlock()
	shown := cur
	if shown == "" {
		shown = "default"
	}

	models, err := c.listModels(ctx)
	if err != nil {
		if len(args) == 0 {
			c.reply("**Error:** could not read the model list: " + err.Error())
			return
		}
		// Selecting still works for explicit IDs without a list.
		models = nil
	}

	if len(args) == 0 {
		c.reply("Model: `" + shown + "`\n\n**Available**\n" + formatModels(models, cur) + "\nUse `!model <name>` (a partial name is fine) or `!model default`.")
		return
	}

	value, candidates, ok := resolveModel(strings.Join(args, " "), models)
	if !ok {
		if len(candidates) > 1 {
			c.reply("Ambiguous, which one?\n" + formatModels(candidates, cur))
		} else {
			c.reply("Unknown model. Send `!model` to list the available ones.")
		}
		return
	}

	c.mu.Lock()
	tid := c.threadID
	c.mu.Unlock()
	note := ""
	if tid != "" {
		err := c.b.app.Call(ctx, "thread/set_model", map[string]any{"thread_id": tid, "model": value}, nil)
		switch {
		case err == nil:
		case appclient.IsCode(err, appclient.ErrThreadNotFound):
			c.dropThread(tid) // the new model applies when the thread is recreated
		case appclient.IsCode(err, appclient.ErrMethodNotFound):
			note = " The server cannot switch a running thread; it applies after `!new`."
		default:
			c.reply("**Error:** " + err.Error())
			return
		}
	}

	stored := value
	if value == "default" {
		stored = ""
	}
	c.mu.Lock()
	c.model = stored
	c.mu.Unlock()
	c.persistSettings()
	c.reply("Model: `" + value + "`." + note)
}
