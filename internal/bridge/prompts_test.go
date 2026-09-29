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

// newFakePermApp asks one Bash permission question per turn and finishes the
// turn once it is answered. A turn whose text contains "expire" gets its
// request withdrawn by the server instead.
func newFakePermApp(t *testing.T, calls chan string) *httptest.Server {
	suggestions := []map[string]any{
		{"type": "addRules", "rules": []map[string]any{{"toolName": "Bash", "ruleContent": "ls -la /tmp"}}, "behavior": "allow", "destination": "localSettings"},
		{"type": "setMode", "mode": "acceptEdits", "destination": "session"},
	}
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
		reply := func(id int64, result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		notify := func(method string, params any) {
			send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		}
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			json.Unmarshal(data, &req)
			raw, _ := json.Marshal(req.Params)
			calls <- req.Method + " " + string(raw)
			switch req.Method {
			case "thread/start":
				reply(req.ID, map[string]any{"thread_id": "T1", "cli_session_id": "S1"})
			case "turn/start":
				reply(req.ID, map[string]any{"turn_id": "U1"})
				notify("approval/requested", map[string]any{
					"thread_id": "T1", "turn_id": "U1", "request_id": "req1", "tool_name": "Bash",
					"description": "List the directory",
					"input":       map[string]any{"command": "ls -la /tmp", "description": "List the directory"},
					"suggestions": suggestions, "expires_at": time.Now().Add(5 * time.Minute).UnixMilli(),
				})
				if s, _ := req.Params["content"].(string); strings.Contains(s, "expire") {
					go func() {
						time.Sleep(150 * time.Millisecond)
						notify("approval/cancelled", map[string]any{"thread_id": "T1", "request_id": "req1", "reason": "timeout"})
					}()
				}
			case "permission/respond":
				reply(req.ID, map[string]any{})
				text := "ran it"
				if req.Params["behavior"] == "deny" {
					text = "understood, not running it"
				}
				notify("item/created", map[string]any{"thread_id": "T1", "turn_id": "U1", "item": map[string]any{"item": map[string]any{"type": "text", "text": text}}})
				notify("turn/completed", map[string]any{"thread_id": "T1", "turn_id": "U1", "status": "completed"})
			default:
				reply(req.ID, map[string]any{})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type permEnv struct {
	fm    *fakeMM
	calls chan string
	cfg   *config.Config
	stop  func()
}

func startPermBridge(t *testing.T, prompts bool) *permEnv {
	calls := make(chan string, 64)
	asrv := newFakePermApp(t, calls)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "default", AttachDir: t.TempDir(), PermissionPrompts: prompts,
	}
	e := &permEnv{fm: fm, calls: calls, cfg: cfg}
	e.stop = startBridge(t, cfg, msrv)
	t.Cleanup(e.stop)
	return e
}

func (e *permEnv) sawCall(t *testing.T, contains string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var seen []string
	for {
		select {
		case c := <-e.calls: // one at a time, so calls after the match stay queued
			seen = append(seen, c)
			if strings.Contains(c, contains) {
				return c
			}
		case <-deadline:
			t.Fatalf("no call containing %q; saw %q", contains, seen)
		}
	}
}

func (e *permEnv) reactionsOn(postID string) []string {
	e.fm.mu.Lock()
	defer e.fm.mu.Unlock()
	var out []string
	for _, r := range e.fm.reactions {
		if strings.HasPrefix(r, postID+":") {
			out = append(out, strings.TrimPrefix(r, postID+":"))
		}
	}
	return out
}

func TestPermissionPromptShownAndAllowedByReaction(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "list /tmp")

	prompt := e.fm.waitPost(t, "Permission requested")
	msg := prompt["message"].(string)
	for _, want := range []string{"`Bash`", "ls -la /tmp", "List the directory", "```", ":white_check_mark:", ":x:", ":fast_forward:",
		"allow every `Bash` call in this conversation", "!allow", "Denied automatically"} {
		if !strings.Contains(msg, want) {
			t.Errorf("prompt lacks %q:\n%s", want, msg)
		}
	}
	id := prompt["id"].(string)

	// The bot offers the options as reactions so they are one click away.
	deadline := time.Now().Add(3 * time.Second)
	for len(e.reactionsOn(id)) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := strings.Join(e.reactionsOn(id), ","); got != "white_check_mark,x,fast_forward" {
		t.Fatalf("option reactions = %q", got)
	}

	// A plain allow does not apply the suggestions.
	e.fm.react("alice", id, "white_check_mark")
	call := e.sawCall(t, "permission/respond")
	if !strings.Contains(call, `"behavior":"allow"`) || strings.Contains(call, "apply_suggestions") || !strings.Contains(call, `"request_id":"req1"`) {
		t.Fatalf("bad answer: %s", call)
	}
	patched := e.fm.waitPatch(t, id, "**Allowed** by @Alice")
	// Once decided, the prompt collapses to one line instead of staying a wall of text.
	if strings.Contains(patched, "```") || strings.Contains(patched, "React") || strings.Count(patched, "\n") > 0 || !strings.Contains(patched, "`Bash` `ls -la /tmp`") {
		t.Errorf("a decided prompt should collapse to a single line naming the call:\n%s", patched)
	}
	e.fm.waitPost(t, "ran it")
}

func TestPermissionAlwaysReactionTrustsTheTool(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "list /tmp")
	id := e.fm.waitPost(t, "Permission requested")["id"].(string)
	e.fm.react("alice", id, "fast_forward")
	call := e.sawCall(t, "permission/respond")
	if !strings.Contains(call, `"behavior":"allow"`) || strings.Contains(call, "apply_suggestions") {
		t.Fatalf("trusting a tool is the bridge's business; the CLI's own rules must not be applied: %s", call)
	}
	e.fm.waitPatch(t, id, "every later `Bash` call in this conversation")
	e.fm.waitPost(t, "ran it")

	// The next request for the same tool is answered without any post.
	promptsBefore := countPrompts(e.fm)
	e.fm.say("D", "dmchan", "p2", "", "and again")
	e.sawCall(t, "turn/start")
	call = e.sawCall(t, "permission/respond")
	if !strings.Contains(call, `"behavior":"allow"`) {
		t.Fatalf("trusted request must be allowed: %s", call)
	}
	time.Sleep(300 * time.Millisecond)
	if n := countPrompts(e.fm); n != promptsBefore {
		t.Fatalf("a trusted tool must not produce a prompt post (had %d, now %d)", promptsBefore, n)
	}

	// !trust shows it, !trust off revokes it, and prompts come back.
	e.fm.say("D", "dmchan", "p3", "", "!trust")
	e.fm.waitPost(t, "Run without asking in this conversation: `Bash`")
	e.fm.say("D", "dmchan", "p4", "", "!trust off")
	e.fm.waitPost(t, "Trusted now: none")
	e.fm.say("D", "dmchan", "p5", "", "once more")
	deadline := time.Now().Add(5 * time.Second)
	for countPrompts(e.fm) == promptsBefore && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if countPrompts(e.fm) == promptsBefore {
		t.Fatal("after !trust off the tool must ask again")
	}
}

func countPrompts(f *fakeMM) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.posts {
		if strings.Contains(p["message"].(string), "Permission requested") {
			n++
		}
	}
	return n
}

func TestPermissionDenyByReaction(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "list /tmp")
	id := e.fm.waitPost(t, "Permission requested")["id"].(string)
	e.fm.react("alice", id, "x")
	if call := e.sawCall(t, "permission/respond"); !strings.Contains(call, `"behavior":"deny"`) {
		t.Fatalf("expected a deny: %s", call)
	}
	e.fm.waitPatch(t, id, "**Denied** by @Alice")
	e.fm.waitPost(t, "not running it")
}

func TestPermissionReactionFromStrangerIsIgnored(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "list /tmp")
	id := e.fm.waitPost(t, "Permission requested")["id"].(string)

	e.fm.react("mallory", id, "white_check_mark") // not in the allowlist
	e.fm.react("bot1", id, "white_check_mark")    // the bot's own option reaction
	time.Sleep(400 * time.Millisecond)
	for _, c := range drain(e.calls) {
		if strings.HasPrefix(c, "permission/respond") {
			t.Fatalf("an unauthorized reaction was acted on: %s", c)
		}
	}
	// The prompt is still open for the real user.
	e.fm.react("alice", id, "white_check_mark")
	e.sawCall(t, "permission/respond")
}

func TestPermissionAnsweredByCommands(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "list /tmp")
	id := e.fm.waitPost(t, "Permission requested")["id"].(string)

	e.fm.say("D", "dmchan", "p2", "", "!status")
	e.fm.waitPost(t, "Waiting for approval: 1")

	e.fm.say("D", "dmchan", "p3", "", "!deny too risky")
	call := e.sawCall(t, "permission/respond")
	if !strings.Contains(call, `"behavior":"deny"`) || !strings.Contains(call, `"message":"too risky"`) {
		t.Fatalf("bad deny: %s", call)
	}
	e.fm.waitPatch(t, id, "**Denied** by @Alice (too risky):")

	e.fm.say("D", "dmchan", "p4", "", "!allow")
	e.fm.waitPost(t, "Nothing is waiting for approval")
}

func TestPermissionAllowAlwaysCommand(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "list /tmp")
	id := e.fm.waitPost(t, "Permission requested")["id"].(string)
	e.fm.say("D", "dmchan", "p2", "", "!allow always")
	if call := e.sawCall(t, "permission/respond"); !strings.Contains(call, `"behavior":"allow"`) || strings.Contains(call, "apply_suggestions") {
		t.Fatalf("bad answer: %s", call)
	}
	e.fm.waitPatch(t, id, "every later `Bash` call")
}

func TestTrustCommandAndFamilies(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "!trust")
	e.fm.waitPost(t, "Nothing is trusted")

	// Names are case-insensitive, and the file-editing tools are one family.
	e.fm.say("D", "dmchan", "p2", "", "!trust bash write")
	m := e.fm.waitPost(t, "Run without asking in this conversation")["message"].(string)
	if !strings.Contains(m, "`Bash`") || !strings.Contains(m, "`Edit/Write`") {
		t.Fatalf("unexpected trust list: %s", m)
	}
	e.fm.say("D", "dmchan", "p3", "", "!trust off write")
	e.fm.waitPost(t, "Trusted now: `Bash`.")
	e.fm.say("D", "dmchan", "p4", "", "!trust all")
	e.fm.waitPost(t, "everything")
	e.fm.say("D", "dmchan", "p5", "", "!status")
	e.fm.waitPost(t, "Trusted tools: ")
}

func TestTrustSurvivesARestart(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakePermApp(t, calls)
	cfg := &config.Config{
		MattermostURL: "unused", Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "default", AttachDir: t.TempDir(), PermissionPrompts: true,
		StateFile: t.TempDir() + "/state.json",
	}

	fm1, msrv1 := newFakeMM(t)
	stop := startBridge(t, cfg, msrv1)
	fm1.say("D", "dmchan", "p1", "", "!trust bash")
	fm1.waitPost(t, "Run without asking")
	stop()

	fm2, msrv2 := newFakeMM(t)
	stop = startBridge(t, cfg, msrv2)
	defer stop()
	fm2.say("D", "dmchan", "p2", "", "list /tmp")
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) && !strings.Contains(got, "permission/respond") {
		got += strings.Join(drain(calls), "\n")
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, `permission/respond {"behavior":"allow"`) {
		t.Fatalf("the saved trust should have answered without asking:\n%s", got)
	}
	if n := countPrompts(fm2); n != 0 {
		t.Fatalf("no prompt expected after restart, saw %d", n)
	}
}

func TestPermissionExpiryUpdatesThePost(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "please expire")
	id := e.fm.waitPost(t, "Permission requested")["id"].(string)
	e.fm.waitPatch(t, id, "denied automatically")

	// A late click must not produce an answer.
	drain(e.calls)
	e.fm.react("alice", id, "white_check_mark")
	time.Sleep(300 * time.Millisecond)
	for _, c := range drain(e.calls) {
		if strings.HasPrefix(c, "permission/respond") {
			t.Fatalf("answered an expired request: %s", c)
		}
	}
}

func TestPermissionPromptsCanBeDisabled(t *testing.T) {
	e := startPermBridge(t, false)
	e.fm.say("D", "dmchan", "p1", "", "hi")
	if call := e.sawCall(t, "thread/start"); strings.Contains(call, "permission_prompts") {
		t.Fatalf("with prompts disabled the thread must not opt in: %s", call)
	}
}

func TestPermissionOptInIsSent(t *testing.T) {
	e := startPermBridge(t, true)
	e.fm.say("D", "dmchan", "p1", "", "hi")
	if call := e.sawCall(t, "thread/start"); !strings.Contains(call, `"permission_prompts":true`) {
		t.Fatalf("thread/start should opt in: %s", call)
	}
}

func TestDescribeTool(t *testing.T) {
	ticks := strings.Repeat("`", 3)
	bash := describeTool("Bash", json.RawMessage(`{"command":"echo `+ticks+`hi`+ticks+`","description":"say hi"}`), "")
	if !strings.Contains(bash, "`"+ticks+"\necho "+ticks+"hi"+ticks+"\n"+"`"+ticks) || !strings.Contains(bash, "say hi") {
		t.Errorf("a command containing backticks needs a longer fence:\n%s", bash)
	}
	long := describeTool("Write", json.RawMessage(`{"file_path":"/x/y.go","content":"`+strings.Repeat("line\\n", 100)+`"}`), "")
	if strings.Count(long, "line") > 16 || !strings.Contains(long, "...") || !strings.Contains(long, "`/x/y.go`") {
		t.Errorf("a long file must be clipped:\n%s", long)
	}
	edit := describeTool("Edit", json.RawMessage(`{"file_path":"/a","old_string":"foo","new_string":"bar"}`), "")
	if !strings.Contains(edit, "foo") || !strings.Contains(edit, "bar") {
		t.Errorf("edit lacks old/new text:\n%s", edit)
	}
	other := describeTool("WebFetch", json.RawMessage(`{"url":"https://example.com"}`), "fetch a page")
	if !strings.Contains(other, "example.com") || !strings.Contains(other, "fetch a page") {
		t.Errorf("generic tools show their input:\n%s", other)
	}
}
