package bridge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/config"
)

// :no_entry_sign: interrupts the running turn and starts one that makes the
// agent review the conversation, with the messages it had not read.
func TestHaltReaction(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()
	turns := func(n int) []string {
		t.Helper()
		var out []string
		deadline := time.After(5 * time.Second)
		for len(out) < n {
			select {
			case c := <-calls:
				if strings.HasPrefix(c, "turn/") {
					out = append(out, strings.Fields(c)[0]+" "+c[strings.Index(c, " ")+1:])
				}
			case <-deadline:
				t.Fatalf("got %d turn calls, want %d: %v", len(out), n, out)
			}
		}
		return out
	}

	fm.say("D", "dmhalt", "p1", "", "hold on, long job")
	turns(1)
	time.Sleep(200 * time.Millisecond)
	fm.say("D", "dmhalt", "p2", "", "wait, unread correction: use the other file")
	if got := turns(1)[0]; !strings.HasPrefix(got, "turn/steer") {
		t.Fatalf("second message: %s", got)
	}
	time.Sleep(200 * time.Millisecond)

	fm.react("mallory", "p2", haltEmoji) // not allowed in this DM
	fm.react("alice", "p2", haltEmoji)
	fm.waitPost(t, "Stopped by @Alice")
	got := turns(2)
	if !strings.HasPrefix(got[0], "turn/interrupt") {
		t.Fatalf("first call must interrupt: %v", got)
	}
	if !strings.HasPrefix(got[1], "turn/start") || !strings.Contains(got[1], "stopped you") ||
		!strings.Contains(got[1], "use the other file") || strings.Contains(got[1], "long job") {
		t.Fatalf("the new turn must carry exactly the unread message: %s", got[1])
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		fm.mu.Lock()
		eyes := slices.Contains(fm.reactions, "p2:eyes")
		fm.mu.Unlock()
		if eyes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no :eyes: on the reacted post once the agent reads the review turn")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fm.waitPost(t, "hello from claude")
	time.Sleep(200 * time.Millisecond)
	fm.mu.Lock()
	for _, p := range fm.posts {
		if strings.Contains(p["message"].(string), "Interrupted") {
			t.Errorf("a stop must not also post %q", p["message"])
		}
	}
	fm.mu.Unlock()

	// Nothing running: nothing to stop.
	fm.react("alice", "p1", haltEmoji)
	fm.waitPost(t, "Nothing is running to stop")
	if extra := turns(0); len(drain(calls)) > 0 || len(extra) > 0 {
		t.Fatal("no turn calls expected when nothing runs")
	}
}

// !cancel ending two turns reports it once.
func TestCancelReportsOnce(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()

	fm.say("D", "dmc", "p1", "", "hold on")
	time.Sleep(300 * time.Millisecond)
	fm.say("D", "dmc", "p2", "", "unread follow-up")
	time.Sleep(300 * time.Millisecond)
	fm.say("D", "dmc", "p3", "", "!cancel")
	fm.waitPost(t, "Interrupted")
	time.Sleep(500 * time.Millisecond)
	fm.mu.Lock()
	defer fm.mu.Unlock()
	n := 0
	for _, p := range fm.posts {
		if strings.Contains(p["message"].(string), "Interrupted") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d Interrupted posts, want 1: %v", n, fm.posts)
	}
}
