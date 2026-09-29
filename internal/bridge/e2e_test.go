package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
	"github.com/uranix/claude-mattermost-bridge/internal/config"
	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

type fakeMM struct {
	mu        sync.Mutex
	posts     []map[string]any
	uploads   []string          // "channel:filename:content"
	reactions []string          // "postID:emoji" added by the bot
	patches   map[string]string // postID -> latest message
	ws        chan []byte
}

func newFakeMM(t *testing.T) (*fakeMM, *httptest.Server) {
	f := &fakeMM{ws: make(chan []byte, 8), patches: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mm.User{ID: "bot1", Username: "claude"})
	})
	mux.HandleFunc("/api/v4/users/alice", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mm.User{ID: "alice", Username: "Alice"})
	})
	mux.HandleFunc("/api/v4/users/mallory", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mm.User{ID: "mallory", Username: "mallory"})
	})
	mux.HandleFunc("POST /api/v4/reactions", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.reactions = append(f.reactions, b["post_id"]+":"+b["emoji_name"])
		f.mu.Unlock()
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("PUT /api/v4/posts/{id}/patch", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.patches[r.PathValue("id")] = b["message"]
		f.mu.Unlock()
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/v4/users/bot1/typing", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/api/v4/files", func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var channel, name, body string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "channel_id":
				channel = string(b)
			case "files":
				name, body = part.FileName(), string(b)
			}
		}
		f.mu.Lock()
		f.uploads = append(f.uploads, channel+":"+name+":"+body)
		id := "F" + strconv.Itoa(len(f.uploads))
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"file_infos": []map[string]any{{"id": id}}})
	})
	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		id := "post" + strconv.Itoa(len(f.posts)+1)
		p["id"] = id
		f.posts = append(f.posts, p)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
	mux.HandleFunc("/api/v4/websocket", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for msg := range f.ws {
			if c.Write(r.Context(), websocket.MessageText, msg) != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeMM) say(channelType, channelID, id, rootID, text string) {
	post, _ := json.Marshal(mm.Post{ID: id, ChannelID: channelID, UserID: "alice", RootID: rootID, Message: text})
	ev, _ := json.Marshal(map[string]any{"event": "posted", "data": map[string]any{"post": string(post), "channel_type": channelType}})
	f.ws <- ev
}

func (f *fakeMM) react(userID, postID, emoji string) {
	r, _ := json.Marshal(mm.Reaction{UserID: userID, PostID: postID, Emoji: emoji})
	ev, _ := json.Marshal(map[string]any{"event": "reaction_added", "data": map[string]any{"reaction": string(r)}})
	f.ws <- ev
}

// waitPatch waits until the post's message was updated to contain text.
func (f *fakeMM) waitPatch(t *testing.T, postID, contains string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		m := f.patches[postID]
		f.mu.Unlock()
		if strings.Contains(m, contains) {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("post %s was never patched with %q; patches: %v", postID, contains, f.patches)
	return ""
}

func (f *fakeMM) waitPost(t *testing.T, contains string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, p := range f.posts {
			if strings.Contains(p["message"].(string), contains) {
				f.mu.Unlock()
				return p
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no post containing %q; got %v", contains, f.posts)
	return nil
}

// fakeApp answers turn/start with one text item and turn/completed.
func newFakeApp(t *testing.T, calls chan string) *httptest.Server {
	return newFakeAppText(t, calls, "hello from claude")
}

func newFakeAppText(t *testing.T, calls chan string, reply string) *httptest.Server {
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
			case "model/list":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
					"models": []string{"opus", "haiku"},
					"model_info": []map[string]any{
						{"value": "opus", "resolved_model": "claude-opus-5-5", "display_name": "Opus", "description": "big"},
						{"value": "haiku", "resolved_model": "claude-haiku-4-5", "display_name": "Haiku", "description": "fast"},
					}, "live": true}})
			case "thread/close":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"closed": true, "cli_session_id": "S1"}})
			case "thread/attach":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"thread_id": "T1", "cli_session_id": "S1", "attached": true}})
			case "turn/start":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"turn_id": "U1"}})
				var sp struct {
					MessageID string `json:"message_id"`
				}
				json.Unmarshal(req.Params, &sp)
				send(map[string]any{"jsonrpc": "2.0", "method": "message/consumed", "params": map[string]any{"thread_id": "T1", "message_id": sp.MessageID}})
				send(map[string]any{"jsonrpc": "2.0", "method": "item/created", "params": map[string]any{
					"thread_id": "T1", "turn_id": "U1", "item": map[string]any{"item": map[string]any{"type": "text", "text": reply}}}})
				send(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"thread_id": "T1", "turn_id": "U1", "status": "completed"}})
			default:
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEndToEnd(t *testing.T) {
	fm, msrv := newFakeMM(t)
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)

	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AllowChannels: true, AppServerURL: "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
	}
	client := mm.New(msrv.URL, "tok")
	me, err := client.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	br, err := New(cfg, client, appclient.New(cfg.AppServerURL), me)
	if err != nil {
		t.Fatal(err)
	}
	go br.Run(ctx)
	time.Sleep(300 * time.Millisecond) // let both websockets connect

	// DM: no mention needed, reply is flat.
	fm.say("D", "dmchan", "p1", "", "hi there")
	p := fm.waitPost(t, "hello from claude")
	if p["channel_id"] != "dmchan" || p["root_id"] != "" {
		t.Fatalf("bad DM reply: %v", p)
	}

	// Channel: ignored without a mention, answered in-thread with one.
	fm.say("O", "chan", "p2", "", "chatter")
	fm.say("O", "chan", "p3", "", "@claude do it")
	deadline := time.Now().Add(5 * time.Second)
	var got map[string]any
	for time.Now().Before(deadline) && got == nil {
		fm.mu.Lock()
		for _, q := range fm.posts {
			if q["channel_id"] == "chan" {
				got = q
			}
		}
		fm.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil || got["root_id"] != "p3" {
		t.Fatalf("channel reply should thread under p3: %v", got)
	}

	// The prompt for the channel turn carries the author and no @mention.
	var sawPrompt bool
	for len(calls) > 0 {
		if c := <-calls; strings.HasPrefix(c, "turn/start") && strings.Contains(c, "@Alice: do it") {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatal("channel prompt missing author label / mention not stripped")
	}

	// Messages delivered to the app server get an :eyes: reaction; commands do not.
	fm.say("D", "dmchan", "p4", "", "!help")
	fm.waitPost(t, "Commands")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fm.mu.Lock()
		n := 0
		for _, r := range fm.reactions {
			if r == "p1:eyes" || r == "p3:eyes" {
				n++
			}
		}
		fm.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if !slices.Contains(fm.reactions, "p1:eyes") || !slices.Contains(fm.reactions, "p3:eyes") {
		t.Errorf("missing :eyes: reactions: %v", fm.reactions)
	}
	if slices.Contains(fm.reactions, "p4:eyes") || slices.Contains(fm.reactions, "p2:eyes") {
		t.Errorf("unexpected :eyes: reaction: %v", fm.reactions)
	}
}

func startBridge(t *testing.T, cfg *config.Config, msrv *httptest.Server) context.CancelFunc {
	client := mm.New(msrv.URL, "tok")
	me, err := client.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	br, err := New(cfg, client, appclient.New(cfg.AppServerURL), me)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go br.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	return cancel
}

func drain(calls chan string) (out []string) {
	for len(calls) > 0 {
		out = append(out, <-calls)
	}
	return
}

func TestResumeAfterRestart(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)
	cfg := &config.Config{
		MattermostURL: "unused", Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
		StateFile: t.TempDir() + "/state.json",
	}

	// First run: fresh thread, session id is saved after the first completed turn.
	fm1, msrv1 := newFakeMM(t)
	stop := startBridge(t, cfg, msrv1)
	fm1.say("D", "dmchan", "p1", "", "hi")
	fm1.waitPost(t, "hello from claude")
	stop()
	if got := strings.Join(drain(calls), "\n"); !strings.Contains(got, "thread/start") || strings.Contains(got, "thread/attach") {
		t.Fatalf("first run should start a thread:\n%s", got)
	}

	// Second run (bridge restarted, new app-server connection): must attach to S1.
	fm2, msrv2 := newFakeMM(t)
	stop = startBridge(t, cfg, msrv2)
	defer stop()
	fm2.say("D", "dmchan", "p2", "", "again")
	fm2.waitPost(t, "hello from claude")
	got := strings.Join(drain(calls), "\n")
	if !strings.Contains(got, `thread/attach {"cli_session_id":"S1"`) || strings.Contains(got, "thread/start") {
		t.Fatalf("second run should attach to the saved session:\n%s", got)
	}

	// !new forgets the session.
	fm2.say("D", "dmchan", "p3", "", "!new")
	fm2.waitPost(t, "fresh context")
	deadline := time.Now().Add(3 * time.Second)
	var closed bool
	for !closed && time.Now().Before(deadline) {
		closed = strings.Contains(strings.Join(drain(calls), "\n"), `thread/close {"thread_id":"T1"}`)
		time.Sleep(20 * time.Millisecond)
	}
	if !closed {
		t.Fatal("!new should close the old thread on the server")
	}
	if b, _ := os.ReadFile(cfg.StateFile); strings.Contains(string(b), "S1") {
		t.Fatalf("!new should delete the saved session: %s", b)
	}
}

func TestIdleCloseThenReattach(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
		StateFile: t.TempDir() + "/state.json", IdleClose: 400 * time.Millisecond,
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()

	fm.say("D", "dmchan", "p1", "", "hi")
	fm.waitPost(t, "hello from claude")

	var log []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(log, "\n"), "thread/close") {
		log = append(log, drain(calls)...)
		time.Sleep(30 * time.Millisecond)
	}
	if !strings.Contains(strings.Join(log, "\n"), `thread/close {"thread_id":"T1"}`) {
		t.Fatalf("idle conversation was not closed:\n%s", strings.Join(log, "\n"))
	}

	// The next message resumes the same session instead of starting over.
	fm.say("D", "dmchan", "p2", "", "back again")
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(log, "\n"), "thread/attach") {
		log = append(log, drain(calls)...)
		time.Sleep(30 * time.Millisecond)
	}
	got := strings.Join(log, "\n")
	if !strings.Contains(got, `thread/attach {"cli_session_id":"S1"`) || strings.Count(got, "thread/start") != 1 {
		t.Fatalf("expected one thread/start then thread/attach:\n%s", got)
	}
}

func TestModelCommand(t *testing.T) {
	calls := make(chan string, 64)
	asrv := newFakeApp(t, calls)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL:   "ws" + strings.TrimPrefix(asrv.URL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(), StateFile: t.TempDir() + "/state.json",
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()

	fm.say("D", "dmchan", "p1", "", "!model")
	p := fm.waitPost(t, "Available")
	if m := p["message"].(string); !strings.Contains(m, "`opus` - Opus: big") || !strings.Contains(m, "Model: `default`") {
		t.Fatalf("bad listing: %s", m)
	}

	// Selecting before any thread exists just remembers it...
	fm.say("D", "dmchan", "p2", "", "!model hai")
	fm.waitPost(t, "Model: `haiku`.")
	fm.say("D", "dmchan", "p3", "", "work")
	fm.waitPost(t, "hello from claude")
	var start string
	for _, c := range drain(calls) {
		if strings.HasPrefix(c, "thread/start") {
			start = c
		}
	}
	if !strings.Contains(start, `"model":"haiku"`) {
		t.Fatalf("thread/start should carry the selected model: %s", start)
	}

	// ...and with a live thread it switches through thread/set_model.
	fm.say("D", "dmchan", "p4", "", "!model opus")
	fm.waitPost(t, "Model: `opus`.")
	var got string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(got, "thread/set_model") {
		got += strings.Join(drain(calls), "\n")
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, `thread/set_model {"model":"opus","thread_id":"T1"}`) {
		t.Fatalf("expected a live switch: %s", got)
	}

	// Ambiguous and unknown names change nothing.
	fm.say("D", "dmchan", "p5", "", "!model zzz")
	fm.waitPost(t, "Unknown model")
	if b, _ := os.ReadFile(cfg.StateFile); !strings.Contains(string(b), `"model": "opus"`) {
		t.Fatalf("selection should be persisted: %s", b)
	}
}

func TestSendFilesBack(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(cwd+"/plot.png", []byte("PNGDATA"), 0o644)
	os.MkdirAll(cwd+"/out", 0o755)
	os.WriteFile(cwd+"/out/data.csv", []byte("a,b\n1,2\n"), 0o644)
	// Outside every allowed root (created next to the sources, not under /tmp).
	outside, _ := os.MkdirTemp(".", "outside-")
	t.Cleanup(func() { os.RemoveAll(outside) })
	os.WriteFile(outside+"/secret.txt", []byte("s3cret"), 0o644)
	secretPath, _ := filepath.Abs(outside + "/secret.txt")

	reply := "Here you go\n\n" +
		"![the plot](" + cwd + "/plot.png)\n" +
		"[results.csv](<out/data.csv>)\n" +
		"[key](" + secretPath + ")\n" +
		"```\n![example](/not/uploaded.png)\n```"

	calls := make(chan string, 64)
	asrv := newFakeAppText(t, calls, reply)
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL: "ws" + strings.TrimPrefix(asrv.URL, "http"), Cwd: cwd, SendFiles: true,
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()

	fm.say("D", "dmchan", "p1", "", "make a plot")
	p := fm.waitPost(t, "Here you go")

	msg := p["message"].(string)
	if strings.Contains(msg, "plot.png") || strings.Contains(msg, "data.csv") {
		t.Errorf("file lines should be removed from the text:\n%s", msg)
	}
	if !strings.Contains(msg, "Could not attach `key`") || strings.Contains(msg, "s3cret") {
		t.Errorf("the disallowed file must be refused with a notice:\n%s", msg)
	}
	if !strings.Contains(msg, "![example](/not/uploaded.png)") {
		t.Errorf("code fences must be left alone:\n%s", msg)
	}
	ids, _ := p["file_ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("expected two attachments, got %v", p["file_ids"])
	}

	fm.mu.Lock()
	uploads := append([]string(nil), fm.uploads...)
	fm.mu.Unlock()
	want := []string{"dmchan:plot.png:PNGDATA", "dmchan:results.csv:a,b\n1,2\n"}
	if len(uploads) != 2 || uploads[0] != want[0] || uploads[1] != want[1] {
		t.Fatalf("uploads = %q, want %q", uploads, want)
	}

	// The agent was told the convention when the thread started.
	if got := strings.Join(drain(calls), "\n"); !strings.Contains(got, "append_system_prompt") {
		t.Fatalf("thread/start should carry the file-sending instructions:\n%s", got)
	}
}

func TestSendFilesDisabled(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(cwd+"/plot.png", []byte("x"), 0o644)
	calls := make(chan string, 64)
	asrv := newFakeAppText(t, calls, "text\n![p]("+cwd+"/plot.png)")
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AppServerURL: "ws" + strings.TrimPrefix(asrv.URL, "http"), Cwd: cwd, SendFiles: false,
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
	}
	stop := startBridge(t, cfg, msrv)
	defer stop()
	fm.say("D", "dmchan", "p1", "", "hi")
	p := fm.waitPost(t, "text")
	if !strings.Contains(p["message"].(string), "plot.png") || len(fm.uploads) != 0 {
		t.Fatalf("with sending disabled the text must pass through untouched: %v uploads=%v", p, fm.uploads)
	}
	if got := strings.Join(drain(calls), "\n"); strings.Contains(got, "append_system_prompt") {
		t.Fatalf("no instructions should be sent when disabled:\n%s", got)
	}
}
