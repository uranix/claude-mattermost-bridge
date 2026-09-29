package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	mu    sync.Mutex
	posts []map[string]any
	ws    chan []byte
}

func newFakeMM(t *testing.T) (*fakeMM, *httptest.Server) {
	f := &fakeMM{ws: make(chan []byte, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mm.User{ID: "bot1", Username: "claude"})
	})
	mux.HandleFunc("/api/v4/users/alice", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mm.User{ID: "alice", Username: "Alice"})
	})
	mux.HandleFunc("/api/v4/users/bot1/typing", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/api/v4/posts", func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		f.mu.Lock()
		f.posts = append(f.posts, p)
		f.mu.Unlock()
		w.Write([]byte("{}"))
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
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"thread_id": "T1"}})
			case "turn/start":
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"turn_id": "U1"}})
				send(map[string]any{"jsonrpc": "2.0", "method": "item/created", "params": map[string]any{
					"thread_id": "T1", "turn_id": "U1", "item": map[string]any{"item": map[string]any{"type": "text", "text": "hello from claude"}}}})
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
	go New(cfg, client, appclient.New(cfg.AppServerURL), me).Run(ctx)
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

	// Commands are answered locally.
	fm.say("D", "dmchan", "p4", "", "!help")
	fm.waitPost(t, "Commands")
}
