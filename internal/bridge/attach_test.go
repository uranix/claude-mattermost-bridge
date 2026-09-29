package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uranix/claude-mattermost-bridge/internal/config"
)

func TestExtractRefs(t *testing.T) {
	text := "Here are the results.\n" +
		"\n" +
		"![loss curve](/tmp/out/loss.png)\n" +
		"[report.csv](</home/u/my dir/report.csv>)\n" +
		"[docs](https://example.com/x)\n" + // web link: stays
		"See [notes](/tmp/notes.txt) inline.\n" + // not standalone: stays
		"[rel](sub/data.bin)\n" +
		"[sb](sandbox:/tmp/sandboxed.log)\n" +
		"```\n" +
		"![in code](/etc/passwd)\n" + // inside a fence: stays
		"```\n" +
		"[anchor](#top)\n"
	rest, refs := extractRefs(text)

	want := []fileRef{
		{"loss curve", "/tmp/out/loss.png", true},
		{"report.csv", "/home/u/my dir/report.csv", false},
		{"rel", "sub/data.bin", false},
		{"sb", "/tmp/sandboxed.log", false},
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %+v", refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("ref %d = %+v, want %+v", i, refs[i], want[i])
		}
	}
	for _, keep := range []string{"Here are the results.", "https://example.com/x", "See [notes](/tmp/notes.txt) inline.", "![in code](/etc/passwd)", "[anchor](#top)"} {
		if !strings.Contains(rest, keep) {
			t.Errorf("rest lost %q:\n%s", keep, rest)
		}
	}
	for _, gone := range []string{"loss.png", "report.csv", "data.bin", "sandboxed.log"} {
		if strings.Contains(rest, gone) {
			t.Errorf("rest still has %q:\n%s", gone, rest)
		}
	}
}

func TestExtractRefsNone(t *testing.T) {
	rest, refs := extractRefs("plain text\nwith [a link](https://x.y) in it")
	if len(refs) != 0 || rest != "plain text\nwith [a link](https://x.y) in it" {
		t.Fatalf("unexpected: %q %+v", rest, refs)
	}
}

func TestFileName(t *testing.T) {
	cases := []struct {
		ref  fileRef
		want string
	}{
		{fileRef{Label: "report.csv"}, "report.csv"},
		{fileRef{Label: "the report"}, "the report"},
		{fileRef{Label: "../evil"}, "data.csv"}, // labels with separators are ignored
		{fileRef{Label: "pretty caption", Image: true}, "data.csv"},
		{fileRef{}, "data.csv"},
	}
	for _, tc := range cases {
		if got := tc.ref.fileName("/x/data.csv"); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestResolveSendPath(t *testing.T) {
	cwd := t.TempDir()
	attach := t.TempDir()
	extra := t.TempDir()
	// A directory that is under no allowed root: /tmp and os.TempDir() are roots,
	// so create it next to the test sources instead.
	outside, err := os.MkdirTemp(".", "outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	outside, _ = filepath.Abs(outside)

	write := func(dir, name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	okFile := write(cwd, "sub/ok.txt", "hi")
	attFile := write(attach, "a.bin", "x")
	extraFile := write(extra, "e.txt", "x")
	secret := write(outside, "secret.txt", "s3cret")
	big := write(cwd, "big.bin", strings.Repeat("x", 2<<20))
	if err := os.Symlink(secret, filepath.Join(cwd, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(okFile, filepath.Join(cwd, "link-inside")); err != nil {
		t.Fatal(err)
	}

	// Roots: cwd, attachments, extra (plus /tmp and TempDir, which are always allowed).
	b := &Bridge{cfg: &config.Config{Cwd: cwd, AttachDir: attach, SendRoots: []string{extra}}}

	check := func(target, wantErr string) {
		t.Helper()
		got, err := b.resolveSendPath(target)
		if wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", target, err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: got (%q, %v), want error containing %q", target, got, err, wantErr)
		}
	}

	check(okFile, "")
	check("sub/ok.txt", "") // relative to cwd
	check(attFile, "")
	check(extraFile, "")
	check(filepath.Join(cwd, "link-inside"), "") // a symlink that stays inside is fine
	check(big, "")

	check(secret, "outside")                                          // directly
	check(filepath.Join(cwd, "link-to-file"), "outside")              // via a symlink to a file
	check(filepath.Join(cwd, "link-to-dir", "secret.txt"), "outside") // via a symlinked directory
	rel, _ := filepath.Rel(cwd, secret)
	check(rel, "outside") // relative escape: ../../..../outside-x/secret.txt
	check("../../../../../../etc/passwd", "outside")
	check("/etc/passwd", "outside")

	check(filepath.Join(cwd, "missing.txt"), "not found")
	check(cwd, "not a regular file")

	old := config.MaxFileBytes
	config.MaxFileBytes = 1 << 20
	check(big, "too large")
	config.MaxFileBytes = old

	if _, err := (&Bridge{cfg: &config.Config{}}).resolveSendPath("relative.txt"); err == nil {
		t.Error("a relative path without a cwd must be refused")
	}
}
