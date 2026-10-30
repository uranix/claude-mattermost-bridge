package bridge

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/config"
)

const evChat = "abcdefghijklmnopqrstuvwxyz" // a DM channel ID, 26 characters

func TestEventGate(t *testing.T) {
	var g eventGate
	now := time.Now()
	if dup, err := g.admit("k", "ci", "e1", now); dup || err != nil {
		t.Fatal(dup, err)
	}
	if dup, _ := g.admit("k", "ci", "e1", now); !dup {
		t.Fatal("same source and id must be a duplicate")
	}
	if dup, _ := g.admit("k", "other", "e1", now); dup {
		t.Fatal("another source with the same id is a different event")
	}
	for i := 0; i < eventRateLimit-2; i++ {
		if _, err := g.admit("k", "ci", "", now); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if _, err := g.admit("k", "ci", "", now); err == nil {
		t.Fatal("rate limit not applied")
	}
	if _, err := g.admit("k2", "ci", "", now); err != nil {
		t.Fatal("the limit is per conversation")
	}
	if _, err := g.admit("k", "ci", "", now.Add(eventRateSpan)); err != nil {
		t.Fatal("the limit must expire")
	}
}

func TestExternalEvents(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)
	fm, msrv := newFakeMM(t)
	base := t.TempDir()
	sock := filepath.Join(t.TempDir(), "ev.sock")
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
		Cwd: base, ChatDirs: true, EventSocket: sock,
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()
	ctx := context.Background()
	send := func(r EventRequest) (EventResponse, int) {
		t.Helper()
		resp, code, err := SendEvent(ctx, sock, r)
		if err != nil {
			t.Fatal(err)
		}
		return resp, code
	}
	turns := func(n int) []string {
		t.Helper()
		var out []string
		deadline := time.After(5 * time.Second)
		for len(out) < n {
			select {
			case c := <-calls:
				if strings.HasPrefix(c, "turn/") {
					out = append(out, c)
				}
				if strings.HasPrefix(c, "thread/start") && !strings.Contains(c, "event --to "+evChat) {
					t.Errorf("the agent is not told how to send events: %s", c)
				}
			case <-deadline:
				t.Fatalf("got %d turn calls, want %d: %v", len(out), n, out)
			}
		}
		return out
	}

	// An unknown chat is refused.
	if r, code := send(EventRequest{To: evChat, Source: "ci", Text: "x"}); code != http.StatusNotFound {
		t.Fatalf("unknown chat: %d %v", code, r)
	}

	fm.say("D", evChat, "p1", "", "hi")
	fm.waitPost(t, "hello from claude")
	turns(1)
	time.Sleep(200 * time.Millisecond)

	// Delivered: posted visibly, sent to the agent labelled as an event.
	if r, code := send(EventRequest{To: evChat, Source: "ci", ID: "build-42", Text: "build 42 ok"}); code != 200 || r.Status != "accepted" || r.Chat != evChat {
		t.Fatalf("event: %d %+v", code, r)
	}
	p := fm.waitPost(t, "build 42 ok")
	if p["message"] != "_Event from `ci`:_\nbuild 42 ok" || p["channel_id"] != evChat {
		t.Fatalf("event post: %v", p)
	}
	got := turns(1)[0]
	if !strings.HasPrefix(got, "turn/start") || !strings.Contains(got, "External event from ci") || !strings.Contains(got, "build 42 ok") {
		t.Fatalf("turn: %s", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		fm.mu.Lock()
		eyes := slices.Contains(fm.reactions, p["id"].(string)+":eyes")
		fm.mu.Unlock()
		if eyes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no :eyes: on the event post")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	// A retry with the same id is not delivered again.
	if r, _ := send(EventRequest{To: evChat, Source: "ci", ID: "build-42", Text: "build 42 ok"}); r.Status != "duplicate" {
		t.Fatalf("retry: %+v", r)
	}

	// Event text never runs a command, and the chat is found from the directory.
	dir := filepath.Join(base, "chats", "dm-"+evChat, "sub")
	os.MkdirAll(dir, 0o700)
	if r, code := send(EventRequest{Cwd: dir, Source: "ci", Text: "!new"}); code != 200 || r.Chat != evChat {
		t.Fatalf("by directory: %d %+v", code, r)
	}
	if got := turns(1)[0]; !strings.Contains(got, `!new`) {
		t.Fatalf("an event starting with ! must reach the agent as text: %s", got)
	}
	if r, code := send(EventRequest{Cwd: base, Source: "ci", Text: "x"}); code != http.StatusBadRequest {
		t.Fatalf("outside chat directories: %d %+v", code, r)
	}
	time.Sleep(200 * time.Millisecond)

	// While a turn runs, an event is steered in like a user message.
	fm.say("D", evChat, "p9", "", "hold on")
	turns(1)
	time.Sleep(200 * time.Millisecond)
	send(EventRequest{To: evChat, Source: "watcher", Text: "file ready"})
	if got := turns(1)[0]; !strings.HasPrefix(got, "turn/steer") || !strings.Contains(got, "file ready") {
		t.Fatalf("busy: %s", got)
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, p := range fm.posts {
		if strings.Contains(p["message"].(string), "fresh context") {
			t.Fatal("an event ran !new")
		}
	}
}
