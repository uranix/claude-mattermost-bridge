package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/config"
)

// filePrompt teaches the agent the convention below. It is appended to the
// system prompt of every thread when file sending is enabled.
const filePrompt = `You are chatting with the user through Mattermost; your replies are shown as Mattermost markdown.
To send a file or image to the user, put a standalone line in your reply, outside code blocks:
  ![short caption](/absolute/path/to/image.png)   for images
  [report.csv](/absolute/path/to/report.csv)       for any other file (the label becomes the file name)
Files must already exist and lie under the working directory or /tmp. Use such a line only for files you want delivered, not for files you merely mention. Remote URLs are not downloaded.`

// maxFilesPerReply bounds how many files one text item may attach.
const maxFilesPerReply = 8

type fileRef struct {
	Label  string
	Target string
	Image  bool
}

// A standalone markdown link or image on its own line. Paths with spaces use <...>.
var refLine = regexp.MustCompile(`^(!?)\[([^\]]*)\]\((?:<([^>]+)>|([^)\s]+))\)$`)

var hasScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)

// extractRefs removes standalone local-file links/images from text (outside
// code fences) and returns them. Web links and links inside sentences stay.
func extractRefs(text string) (rest string, refs []fileRef) {
	var kept []string
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			kept = append(kept, line)
			continue
		}
		if !fenced {
			if m := refLine.FindStringSubmatch(trimmed); m != nil {
				target := m[3]
				if target == "" {
					target = m[4]
				}
				if local, ok := localTarget(target); ok {
					refs = append(refs, fileRef{Label: m[2], Target: local, Image: m[1] == "!"})
					continue
				}
			}
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), refs
}

// localTarget reports whether a link target names a local file, and its path.
func localTarget(t string) (string, bool) {
	t = strings.TrimPrefix(t, "sandbox:")
	if t == "" || strings.HasPrefix(t, "#") || hasScheme.MatchString(t) || strings.HasPrefix(t, "//") {
		return "", false
	}
	return t, true
}

// sendRoots are the directories a reply may attach files from.
func (b *Bridge) sendRoots() []string {
	roots := []string{"/tmp", "/var/tmp", os.TempDir()}
	if b.cfg.Cwd != "" {
		roots = append(roots, b.cfg.Cwd)
	}
	if abs, err := filepath.Abs(b.cfg.AttachDir); err == nil {
		roots = append(roots, abs)
	}
	roots = append(roots, b.cfg.SendRoots...)
	var out []string
	for _, r := range roots {
		if real, err := filepath.EvalSymlinks(r); err == nil {
			out = append(out, real)
		}
	}
	return out
}

// resolveSendPath maps a link target to a real regular file inside an allowed
// root. Symlinks are resolved first, so a link inside an allowed directory
// cannot lead outside it. The agent's output is untrusted (prompt injection
// could ask it to send ~/.ssh/id_rsa), hence the allowlist.
func (b *Bridge) resolveSendPath(target string) (string, error) {
	p := target
	if !filepath.IsAbs(p) {
		if b.cfg.Cwd == "" {
			return "", fmt.Errorf("relative path and no CLAUDE_CWD configured")
		}
		p = filepath.Join(b.cfg.Cwd, p)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("file not found")
	}
	allowed := false
	for _, root := range b.sendRoots() {
		if rel, err := filepath.Rel(root, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("outside the directories files may be sent from")
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	if st.Size() > config.MaxFileBytes {
		return "", fmt.Errorf("too large (%d MB, limit %d MB)", st.Size()>>20, config.MaxFileBytes>>20)
	}
	return real, nil
}

// fileName picks the name shown in Mattermost: the link label for plain files
// (when it is a bare name), otherwise the file's own name.
func (r fileRef) fileName(path string) string {
	if !r.Image && r.Label != "" && !strings.ContainsAny(r.Label, `/\`) {
		return r.Label
	}
	return filepath.Base(path)
}

// deliverAgentText posts one text item of the agent, uploading the files it
// references. Runs in the conversation's outbox goroutine.
func (c *conv) deliverAgentText(root, text string) {
	if !c.b.cfg.SendFiles {
		c.b.post(c.channelID, root, text)
		return
	}
	rest, refs := extractRefs(text)
	if len(refs) == 0 {
		c.b.post(c.channelID, root, text)
		return
	}

	var ids, notes, captions []string
	seen := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for i, ref := range refs {
		name := ref.Label
		if name == "" {
			name = ref.Target
		}
		if i >= maxFilesPerReply {
			notes = append(notes, fmt.Sprintf("_Could not attach `%s`: at most %d files per message._", name, maxFilesPerReply))
			continue
		}
		path, err := c.b.resolveSendPath(ref.Target)
		if err == nil && seen[path] {
			continue // the same file twice
		}
		if err == nil {
			seen[path] = true
			var id string
			if id, err = c.b.mm.UploadFile(ctx, c.channelID, ref.fileName(path), path); err == nil {
				ids = append(ids, id)
				if ref.Image && ref.Label != "" {
					captions = append(captions, ref.Label)
				}
				continue
			}
		}
		notes = append(notes, fmt.Sprintf("_Could not attach `%s`: %v._", name, err))
	}

	msg := rest
	if msg == "" { // an image-only reply keeps its captions
		msg = strings.Join(captions, "\n")
	}
	if len(notes) > 0 {
		msg = strings.TrimSpace(msg + "\n\n" + strings.Join(notes, "\n"))
	}
	c.b.postFiles(c.channelID, root, msg, ids)
}
