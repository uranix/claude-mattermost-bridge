package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// External events: local programs (or a watcher the agent left running) send
// text to a conversation through a Unix socket. The bridge posts it visibly in
// the chat and hands it to the agent like a user message: it starts a turn, or
// is steered into the running one.

const (
	maxEventRunes  = 12000 // fits one Mattermost post with its header
	maxEventBody   = 256 << 10
	eventRateLimit = 20          // events per conversation...
	eventRateSpan  = time.Minute // ...in this span
	eventDedupTTL  = 24 * time.Hour
)

var (
	eventSourceRe = regexp.MustCompile(`^[\w.\-]{1,64}$`)
	eventIDRe     = regexp.MustCompile(`^[\w.:\-]{1,128}$`)
	chatIDRe      = regexp.MustCompile(`^[a-z0-9]{26}$`)
)

// EventRequest is what a producer sends to POST /events.
type EventRequest struct {
	To     string `json:"to"`     // thread root post ID or DM channel ID; "" = from Cwd
	Cwd    string `json:"cwd"`    // the producer's working directory, used when To is ""
	Source string `json:"source"` // who sends it, shown in the chat
	ID     string `json:"id"`     // optional; the same Source and ID are delivered once
	Text   string `json:"text"`
}

// EventResponse is the answer: Status is "accepted" or "duplicate" with HTTP
// 200, otherwise Error says what is wrong.
type EventResponse struct {
	Status string `json:"status,omitempty"`
	Chat   string `json:"chat,omitempty"`
	Error  string `json:"error,omitempty"`
}

type eventError struct {
	code int
	msg  string
}

func (e *eventError) Error() string { return e.msg }

func badEvent(code int, format string, a ...any) error {
	return &eventError{code, fmt.Sprintf(format, a...)}
}

// eventGate holds the de-duplication and rate-limit state.
type eventGate struct {
	mu     sync.Mutex
	seen   map[string]time.Time   // source + "\x00" + id -> when
	recent map[string][]time.Time // conversation key -> recent event times
}

// admit decides whether an event goes through; it records it if so.
func (g *eventGate) admit(key, source, id string, now time.Time) (dup bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen, g.recent = map[string]time.Time{}, map[string][]time.Time{}
	}
	for k, t := range g.seen {
		if now.Sub(t) > eventDedupTTL {
			delete(g.seen, k)
		}
	}
	dk := source + "\x00" + id
	if id != "" {
		if _, ok := g.seen[dk]; ok {
			return true, nil
		}
	}
	times := g.recent[key][:0]
	for _, t := range g.recent[key] {
		if now.Sub(t) < eventRateSpan {
			times = append(times, t)
		}
	}
	if len(times) >= eventRateLimit {
		g.recent[key] = times
		return false, badEvent(http.StatusTooManyRequests, "more than %d events in %v for this chat", eventRateLimit, eventRateSpan)
	}
	g.recent[key] = append(times, now)
	if id != "" {
		g.seen[dk] = now
	}
	return false, nil
}

// chatID is the ID a conversation is addressed by: the thread root post in a
// channel, the channel in a DM.
func chatID(key string) string {
	parts := strings.Split(key, ":")
	return parts[len(parts)-1]
}

// eventTarget finds the conversation for an event: by To, or else by the chat
// directory Cwd lies in.
func (b *Bridge) eventTarget(to, cwd string) (key, channelID string, err error) {
	if to == "" {
		to = b.chatFromDir(cwd)
		if to == "" {
			return "", "", badEvent(http.StatusBadRequest, "no chat given (--to) and %q is not inside a chat directory", cwd)
		}
	}
	if !chatIDRe.MatchString(to) {
		return "", "", badEvent(http.StatusBadRequest, "chat ID must be 26 lowercase letters and digits")
	}
	b.mu.Lock()
	keys := make([]string, 0, len(b.convs))
	for k := range b.convs {
		keys = append(keys, k)
	}
	b.mu.Unlock()
	keys = append(keys, b.state.keys()...)
	for _, k := range keys {
		parts := strings.Split(k, ":")
		switch {
		case len(parts) == 2 && parts[0] == "dm" && parts[1] == to:
			return k, to, nil
		case len(parts) == 3 && parts[0] == "ch" && parts[2] == to:
			return k, parts[1], nil
		}
	}
	return "", "", badEvent(http.StatusNotFound, "no conversation %s", to)
}

// chatFromDir maps a directory inside Cwd/chats/<thread-X|dm-X> to X.
func (b *Bridge) chatFromDir(dir string) string {
	if dir == "" || b.cfg.Cwd == "" {
		return ""
	}
	base, err := filepath.EvalSymlinks(filepath.Join(b.cfg.Cwd, "chats"))
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	name := strings.Split(rel, string(filepath.Separator))[0]
	for _, p := range []string{"thread-", "dm-"} {
		if id, ok := strings.CutPrefix(name, p); ok {
			return id
		}
	}
	return ""
}

// deliverEvent checks an event, posts it in its chat and queues it for the agent.
func (b *Bridge) deliverEvent(ctx context.Context, r EventRequest) (EventResponse, error) {
	r.Text = strings.TrimSpace(r.Text)
	switch {
	case r.Text == "":
		return EventResponse{}, badEvent(http.StatusBadRequest, "empty text")
	case !utf8.ValidString(r.Text):
		return EventResponse{}, badEvent(http.StatusBadRequest, "text is not UTF-8")
	case utf8.RuneCountInString(r.Text) > maxEventRunes:
		return EventResponse{}, badEvent(http.StatusBadRequest, "text longer than %d characters", maxEventRunes)
	case !eventSourceRe.MatchString(r.Source):
		return EventResponse{}, badEvent(http.StatusBadRequest, "source must be 1-64 letters, digits, '.', '_' or '-'")
	case r.ID != "" && !eventIDRe.MatchString(r.ID):
		return EventResponse{}, badEvent(http.StatusBadRequest, "id must be 1-128 letters, digits, '.', ':', '_' or '-'")
	}
	key, channelID, err := b.eventTarget(r.To, r.Cwd)
	if err != nil {
		return EventResponse{}, err
	}
	resp := EventResponse{Status: "accepted", Chat: chatID(key)}
	dup, err := b.events.admit(key, r.Source, r.ID, time.Now())
	if err != nil {
		return EventResponse{}, err
	}
	if dup {
		resp.Status = "duplicate"
		return resp, nil
	}

	root := ""
	if strings.HasPrefix(key, "ch:") {
		root = chatID(key)
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	post, err := b.mm.CreatePost(pctx, channelID, root, fmt.Sprintf("_Event from `%s`:_\n%s", r.Source, r.Text), nil)
	if err != nil {
		return EventResponse{}, badEvent(http.StatusBadGateway, "posting to Mattermost failed: %v", err)
	}
	c := b.getConv(key, channelID)
	c.enqueue(inbound{post: post, text: r.Text, rootID: root, direct: strings.HasPrefix(key, "dm:"), event: r.Source})
	slog.Info("external event", "conv", key, "source", r.Source, "id", r.ID)
	return resp, nil
}

// eventPrompt tells the agent how to send events to this conversation.
func (b *Bridge) eventPrompt(key string) string {
	if b.cfg.EventSocket == "" {
		return ""
	}
	bin, err := os.Executable()
	if err != nil {
		bin = "claude-mattermost-bridge"
	}
	cmd := fmt.Sprintf("%s event --to %s", bin, chatID(key))
	return `External events: a program can message this conversation with
  ` + cmd + ` --source <name> [--id <unique id>] --text '<what happened>'
The event is posted in the chat and reaches you like a user message (steered in if you are busy), labelled as an external event. To wait for something slow (a job finishing, a file appearing), start a detached watcher that sends an event when the condition holds, for example
  setsid nohup sh -c 'until test -f out.nc; do sleep 60; done; ` + cmd + ` --source watcher --text "out.nc is ready"' >/dev/null 2>&1 &
and end your turn. Prefer this to a background task of your own session, which dies when the session restarts or is closed for idleness.`
}

// serveEvents takes events on the Unix socket until ctx ends.
func (b *Bridge) serveEvents(ctx context.Context) {
	path := b.cfg.EventSocket
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Error("event socket", "err", err)
		return
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path) // left over from a previous run
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		slog.Error("event socket", "err", err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		slog.Error("event socket", "err", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		var req EventRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, maxEventBody+1))
		if err == nil && len(body) > maxEventBody {
			err = errors.New("request too large")
		}
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		var resp EventResponse
		if err != nil {
			err = badEvent(http.StatusBadRequest, "bad request: %v", err)
		} else {
			resp, err = b.deliverEvent(r.Context(), req)
		}
		code := http.StatusOK
		if err != nil {
			code = http.StatusInternalServerError
			var ee *eventError
			if errors.As(err, &ee) {
				code = ee.code
			}
			resp = EventResponse{Error: err.Error()}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
		os.Remove(path)
	}()
	slog.Info("event socket listening", "path", path)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("event socket", "err", err)
	}
}

// SendEvent delivers an event to a bridge listening on socket.
func SendEvent(ctx context.Context, socket string, r EventRequest) (EventResponse, int, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	body, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://bridge/events", strings.NewReader(string(body)))
	if err != nil {
		return EventResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return EventResponse{}, 0, err
	}
	defer res.Body.Close()
	var out EventResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return EventResponse{}, res.StatusCode, fmt.Errorf("bad response (HTTP %d): %w", res.StatusCode, err)
	}
	return out, res.StatusCode, nil
}
