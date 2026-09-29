package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uranix/claude-mattermost-bridge/internal/config"
)

// newFakeStuckApp accepts turns and never ends them. interrupt: "ack" answers
// turn/interrupt normally (but the turn still does not end); "noturn" answers
// with the server's no-active-turn error.
func newFakeStuckApp(t *testing.T, calls chan string, interrupt string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		send := func(v any) {
			b, _ := json.Marshal(v)
			c.Write(r.Context(), websocket.MessageText, b)
		}
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			json.Unmarshal(data, &req)
			calls <- req.Method + " " + string(req.Params)
			switch req.Method {
			case "thread/start":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"thread_id": "T1", "cli_session_id": "S1"}})
			case "thread/attach":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"thread_id": "T2", "cli_session_id": "S1", "attached": true}})
			case "turn/start", "turn/steer":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"turn_id": "U"}})
			case "turn/interrupt":
				if interrupt == "noturn" {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32004, "message": "no active turn to interrupt"}})
				} else {
					send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"status": "interrupted"}})
				}
			default:
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func startStuck(t *testing.T, interrupt string) (*fakeMM, chan string) {
	calls := make(chan string, 64)
	asrv := newFakeStuckApp(t, calls, interrupt)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(), CancelWait: 500 * time.Millisecond,
	}
	stop := startBridge(t, cfg, msrv)
	t.Cleanup(stop)
	return fm, calls
}

func TestCancelSaysSoWhenTheTurnDoesNotEnd(t *testing.T) {
	fm, _ := startStuck(t, "ack")
	fm.say("D", "dmchan", "p1", "", "work")
	time.Sleep(300 * time.Millisecond)
	fm.say("D", "dmchan", "p2", "", "!cancel")
	msg := fm.waitPost(t, "did not end its turn")["message"].(string)
	if !strings.Contains(msg, "!reset") {
		t.Fatalf("the hint must name the way out: %s", msg)
	}
}

func TestCancelCorrectsAStaleCount(t *testing.T) {
	fm, _ := startStuck(t, "noturn")
	fm.say("D", "dmchan", "p1", "", "work")
	time.Sleep(300 * time.Millisecond)
	fm.say("D", "dmchan", "p2", "", "!cancel")
	fm.waitPost(t, "cleared the stale state")
	fm.say("D", "dmchan", "p3", "", "!status")
	fm.waitPost(t, "Running turns: 0")
}

func TestResetKeepsTheConversation(t *testing.T) {
	fm, calls := startStuck(t, "ack")
	fm.say("D", "dmchan", "p1", "", "work")
	time.Sleep(300 * time.Millisecond)
	fm.say("D", "dmchan", "p2", "", "!status")
	fm.waitPost(t, "Running turns: 1")

	fm.say("D", "dmchan", "p3", "", "!reset")
	fm.waitPost(t, "Reconnected")
	fm.say("D", "dmchan", "p4", "", "!status")
	fm.waitPost(t, "Running turns: 0")

	// The old thread is closed, and the next message re-attaches to the same
	// session instead of starting a fresh one.
	fm.say("D", "dmchan", "p5", "", "carry on")
	var log []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(log, "\n"), "thread/attach") {
		log = append(log, drain(calls)...)
		time.Sleep(30 * time.Millisecond)
	}
	all := strings.Join(log, "\n")
	if !strings.Contains(all, `thread/close {"thread_id":"T1"}`) {
		t.Errorf("reset must close the old thread:\n%s", all)
	}
	if !strings.Contains(all, `thread/attach {"cli_session_id":"S1"`) || strings.Count(all, "thread/start") != 1 {
		t.Errorf("reset must re-attach to the same session, not start over:\n%s", all)
	}
}
