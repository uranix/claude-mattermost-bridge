package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithKeyFile(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	os.WriteFile(key, []byte("abc123_-XYZ0123456789\n"), 0o600)

	got, err := withKeyFile("ws://127.0.0.1:3284", key)
	if err != nil || got != "ws://127.0.0.1:3284?key=abc123_-XYZ0123456789" {
		t.Fatalf("got %q, %v", got, err)
	}
	// An existing key parameter is replaced, other parameters are kept.
	got, _ = withKeyFile("ws://h:1/p?key=old&x=1", key)
	if got != "ws://h:1/p?key=abc123_-XYZ0123456789&x=1" {
		t.Fatalf("got %q", got)
	}
	if _, err := withKeyFile("ws://h", filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing key file must be an error")
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte(" \n"), 0o600)
	if _, err := withKeyFile("ws://h", empty); err == nil {
		t.Error("an empty key file must be an error")
	}
}
