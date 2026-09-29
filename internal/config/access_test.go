package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadWith(t *testing.T, env map[string]string) (*Config, error) {
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("x"), 0o600)
	base := map[string]string{
		"MM_URL": "https://mm.example", "MM_TOKEN_FILE": tok,
		"MM_ALLOWED_USERS": "", "MM_ALLOWED_CHANNELS": "", "MM_ALLOW_CHANNELS": "", "MM_CHANNEL_ANYONE": "",
	}
	for k, v := range env {
		base[k] = v
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
	return Load()
}

func TestChannelAllowlistParsing(t *testing.T) {
	c, err := loadWith(t, map[string]string{"MM_ALLOWED_CHANNELS": " Abc123 , general,, Ops-Team ", "MM_ALLOWED_USERS": "@Root"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"abc123", "general", "ops-team"} {
		if !c.AllowedChannels[want] {
			t.Errorf("channel %q missing from %v", want, c.AllowedChannels)
		}
	}
	if len(c.AllowedChannels) != 3 || !c.AllowedUsers["root"] {
		t.Errorf("unexpected: channels=%v users=%v", c.AllowedChannels, c.AllowedUsers)
	}
}

func TestChannelsOnlyIsValid(t *testing.T) {
	if _, err := loadWith(t, map[string]string{"MM_ALLOWED_CHANNELS": "general"}); err != nil {
		t.Fatalf("a bot for channels only needs no DM users: %v", err)
	}
}

func TestNothingAllowedIsRefused(t *testing.T) {
	if _, err := loadWith(t, nil); err == nil || !strings.Contains(err.Error(), "ignore everyone") {
		t.Fatalf("got %v", err)
	}
}

func TestOldChannelFlagsFailLoudly(t *testing.T) {
	for _, env := range []map[string]string{{"MM_ALLOW_CHANNELS": "1"}, {"MM_CHANNEL_ANYONE": "1"}} {
		env["MM_ALLOWED_USERS"] = "root"
		_, err := loadWith(t, env)
		if err == nil || !strings.Contains(err.Error(), "MM_ALLOWED_CHANNELS") {
			t.Fatalf("%v: the migration message must name the new setting, got %v", env, err)
		}
	}
	// ...but not once the new setting is present.
	if _, err := loadWith(t, map[string]string{"MM_ALLOW_CHANNELS": "1", "MM_ALLOWED_CHANNELS": "general", "MM_ALLOWED_USERS": "root"}); err != nil {
		t.Fatalf("the old flag next to the new list is harmless: %v", err)
	}
}
