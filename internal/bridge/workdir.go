package bridge

import (
	"os"
	"path/filepath"
	"strings"
)

// chatDirPrompt tells the agent its working directory is its own.
const chatDirPrompt = `Your working directory belongs to this conversation alone; other conversations with this bot work in their own directories. Keep files you create, scratch files included, in it rather than in /tmp, which is shared.`

// chatDir is the working directory of a new conversation: Cwd/chats/<name>,
// named after the Mattermost thread (or DM channel). "" when conversations
// share Cwd.
func (b *Bridge) chatDir(key string) string {
	if !b.cfg.ChatDirs || b.cfg.Cwd == "" {
		return ""
	}
	var name string
	switch parts := strings.Split(key, ":"); {
	case len(parts) == 3 && parts[0] == "ch":
		name = "thread-" + parts[2]
	case len(parts) == 2 && parts[0] == "dm":
		name = "dm-" + parts[1]
	default:
		name = unsafeName.ReplaceAllString(key, "-")
	}
	return filepath.Join(b.cfg.Cwd, "chats", name)
}

// workDir is where this conversation's agent runs: its own directory, or the
// shared Cwd for conversations started before chat directories existed.
func (c *conv) workDir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cwd != "" {
		return c.cwd
	}
	return c.b.cfg.Cwd
}

// ensureWorkDir creates the conversation's own directory (mode 700).
func (c *conv) ensureWorkDir() error {
	c.mu.Lock()
	dir := c.cwd
	c.mu.Unlock()
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o700)
}
