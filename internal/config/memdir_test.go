package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The derived memory directory follows the CLI's naming: the real path with
// every character other than a letter or digit replaced by '-'.
func TestDefaultMemoryDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg")
	real := t.TempDir()
	dir := filepath.Join(real, "x.state", "work")
	os.MkdirAll(dir, 0o700)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(real, "x.state"), link); err != nil {
		t.Fatal(err)
	}
	got := defaultMemoryDir(filepath.Join(link, "work"))
	enc := ""
	for _, r := range dir {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			enc += string(r)
		} else {
			enc += "-"
		}
	}
	if want := "/cfg/projects/" + enc + "/memory"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
