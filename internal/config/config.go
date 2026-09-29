// Package config loads bridge settings from environment variables, so the
// same bot.env file works for a shell (`set -a; . bot.env`) and systemd.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MattermostURL string // e.g. https://chat.example.com
	Token         string // bot access token (read from MM_TOKEN_FILE or MM_TOKEN)

	AllowedUsers  map[string]bool // Mattermost usernames, lowercase, without '@'
	AllowChannels bool            // respond to @mentions in channels and group DMs

	AppServerURL   string // ws://127.0.0.1:3284?key=...
	Cwd            string
	PermissionMode string
	Model          string

	AttachDir         string
	StateFile         string        // conversation -> CLI session map; "" disables persistence
	IdleClose         time.Duration // close a conversation's server thread after this much inactivity; 0 disables
	PermissionPrompts bool          // ask in chat before tools that need permission run
	SendFiles         bool          // upload files the agent references in its replies
	SendRoots         []string      // extra directories (besides cwd, attachments, /tmp) files may be sent from
	ShowTools         bool
	Debug             bool
}

func Load() (*Config, error) {
	c := &Config{
		MattermostURL:     strings.TrimRight(os.Getenv("MM_URL"), "/"),
		AppServerURL:      getenv("APP_SERVER_URL", "ws://127.0.0.1:3284"),
		Cwd:               os.Getenv("CLAUDE_CWD"),
		PermissionMode:    getenv("CLAUDE_PERMISSION_MODE", "acceptEdits"),
		Model:             os.Getenv("CLAUDE_MODEL"),
		AttachDir:         getenv("BRIDGE_ATTACHMENT_DIR", "attachments"),
		StateFile:         getenv("BRIDGE_STATE_FILE", "state.json"),
		AllowChannels:     truthy(os.Getenv("MM_ALLOW_CHANNELS")),
		ShowTools:         truthy(os.Getenv("BRIDGE_SHOW_TOOLS")),
		SendFiles:         getenv("BRIDGE_SEND_FILES", "1") != "0",
		PermissionPrompts: getenv("BRIDGE_PERMISSION_PROMPTS", "1") != "0",
		Debug:             truthy(os.Getenv("BRIDGE_DEBUG")),
		AllowedUsers:      map[string]bool{},
	}
	if f := os.Getenv("APP_SERVER_KEY_FILE"); f != "" {
		u, err := withKeyFile(c.AppServerURL, f)
		if err != nil {
			return nil, err
		}
		c.AppServerURL = u
	}
	if c.MattermostURL == "" {
		return nil, fmt.Errorf("MM_URL is required")
	}
	if f := os.Getenv("MM_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read MM_TOKEN_FILE: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	} else {
		c.Token = strings.TrimSpace(os.Getenv("MM_TOKEN"))
	}
	if c.Token == "" {
		return nil, fmt.Errorf("MM_TOKEN_FILE (or MM_TOKEN) is required")
	}
	for _, u := range strings.Split(os.Getenv("MM_ALLOWED_USERS"), ",") {
		u = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(u), "@"))
		if u != "" {
			c.AllowedUsers[u] = true
		}
	}
	if len(c.AllowedUsers) == 0 {
		return nil, fmt.Errorf("MM_ALLOWED_USERS is empty: the bot would ignore everyone")
	}
	for _, r := range strings.Split(os.Getenv("BRIDGE_SEND_ROOTS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			c.SendRoots = append(c.SendRoots, r)
		}
	}
	c.IdleClose = time.Hour
	if v := os.Getenv("BRIDGE_IDLE_CLOSE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("BRIDGE_IDLE_CLOSE: want a duration like 90m, or 0")
		}
		c.IdleClose = d
	}
	if n, err := strconv.Atoi(os.Getenv("BRIDGE_MAX_FILE_MB")); err == nil && n > 0 {
		MaxFileBytes = int64(n) << 20
	}
	return c, nil
}

// withKeyFile adds the auth key stored in keyFile to the app server URL as ?key=,
// so the secret can live in a mode-600 file instead of the environment file.
func withKeyFile(rawURL, keyFile string) (string, error) {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return "", fmt.Errorf("read APP_SERVER_KEY_FILE: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("APP_SERVER_KEY_FILE %s is empty", keyFile)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("APP_SERVER_URL: %w", err)
	}
	q := u.Query()
	q.Set("key", key)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// MaxFileBytes caps a single downloaded attachment.
var MaxFileBytes int64 = 50 << 20

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
