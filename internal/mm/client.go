// Package mm is a minimal Mattermost client: REST calls plus the WebSocket
// event stream, authenticated with a bot access token.
package mm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/uranix/claude-mattermost-bridge/internal/wsutil"
)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

type Post struct {
	ID        string   `json:"id"`
	ChannelID string   `json:"channel_id"`
	UserID    string   `json:"user_id"`
	RootID    string   `json:"root_id"`
	Message   string   `json:"message"`
	Type      string   `json:"type"`
	FileIDs   []string `json:"file_ids"`
}

type FileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}

// Reaction is one "reaction_added" WebSocket event.
type Reaction struct {
	UserID string `json:"user_id"`
	PostID string `json:"post_id"`
	Emoji  string `json:"emoji_name"`
}

// Posted is one "posted" WebSocket event.
type Posted struct {
	Post        Post
	ChannelType string // D direct, G group, O open, P private
	ChannelName string // the channel's URL name (a hash for DMs and group DMs)
	Mentions    []string
}

type Client struct {
	base   string
	token  string
	http   *http.Client
	upload *http.Client // same, without a total timeout, for large files
}

func New(base, token string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"), token: token,
		http:   &http.Client{Timeout: 60 * time.Second},
		upload: &http.Client{},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v4"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("mattermost %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, "GET", "/users/me", nil, &u)
	return u, err
}

func (c *Client) User(ctx context.Context, id string) (User, error) {
	var u User
	err := c.do(ctx, "GET", "/users/"+url.PathEscape(id), nil, &u)
	return u, err
}

func (c *Client) CreatePost(ctx context.Context, channelID, rootID, message string, fileIDs []string) (Post, error) {
	body := map[string]any{"channel_id": channelID, "root_id": rootID, "message": message}
	if len(fileIDs) > 0 {
		body["file_ids"] = fileIDs
	}
	var p Post
	err := c.do(ctx, "POST", "/posts", body, &p)
	return p, err
}

// PatchPost replaces the message of an existing post (used to record the
// outcome of a permission prompt).
func (c *Client) PatchPost(ctx context.Context, postID, message string) error {
	return c.do(ctx, "PUT", "/posts/"+url.PathEscape(postID)+"/patch", map[string]any{"message": message}, nil)
}

// AddReaction reacts to a post as userID (the bot).
func (c *Client) AddReaction(ctx context.Context, userID, postID, emoji string) error {
	return c.do(ctx, "POST", "/reactions", map[string]any{"user_id": userID, "post_id": postID, "emoji_name": emoji}, nil)
}

// MaxFilesPerPost is Mattermost's limit of attachments on one post.
const MaxFilesPerPost = 10

// UploadFile streams a local file to Mattermost and returns its file ID, to be
// attached to a post in the same channel. Large files are not buffered.
func (c *Client) UploadFile(ctx context.Context, channelID, filename, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := mw.WriteField("channel_id", channelID) // must precede the file part
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile("files", filename); err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/api/v4/files", pr)
	if err != nil {
		pr.Close()
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.upload.Do(req) // no overall timeout; ctx bounds it
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("upload %s: %s: %s", filename, resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		FileInfos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.FileInfos) == 0 {
		return "", fmt.Errorf("upload %s: unexpected response", filename)
	}
	return out.FileInfos[0].ID, nil
}

// Typing shows the "bot is typing" indicator; it expires after a few seconds.
func (c *Client) Typing(ctx context.Context, userID, channelID, rootID string) error {
	return c.do(ctx, "POST", "/users/"+url.PathEscape(userID)+"/typing",
		map[string]any{"channel_id": channelID, "parent_id": rootID}, nil)
}

func (c *Client) FileInfo(ctx context.Context, id string) (FileInfo, error) {
	var fi FileInfo
	err := c.do(ctx, "GET", "/files/"+url.PathEscape(id)+"/info", nil, &fi)
	return fi, err
}

// DownloadFile streams a file to dest (mode 0600).
func (c *Client) DownloadFile(ctx context.Context, id, dest string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/v4/files/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("download %s: %s", id, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(dest)
		return err
	}
	return f.Close()
}

// Listen streams "posted" events until ctx is done, reconnecting with backoff.
// Events posted while disconnected are not replayed.
func (c *Client) Listen(ctx context.Context, onPost func(Posted), onReaction func(Reaction)) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.listenOnce(ctx, onPost, onReaction)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("mattermost websocket lost", "err", err)
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) listenOnce(ctx context.Context, onPost func(Posted), onReaction func(Reaction)) error {
	u, err := url.Parse(c.base)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v4/websocket"

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(4 << 20)
	slog.Info("mattermost websocket connected")

	// Liveness: a read deadline would fire on a healthy but quiet link, because
	// control frames are handled inside Read without returning. Ping instead, and
	// drop the connection when a ping is not answered.
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go wsutil.KeepAlive(rctx, conn, wsutil.PingInterval, wsutil.PingTimeout, stop)

	for {
		_, data, err := conn.Read(rctx)
		if err != nil {
			return err
		}
		if ev, ok := parsePosted(data); ok {
			onPost(ev)
		} else if r, ok := parseReaction(data); ok {
			onReaction(r)
		}
	}
}

func parseReaction(data []byte) (Reaction, bool) {
	var e struct {
		Event string `json:"event"`
		Data  struct {
			Reaction string `json:"reaction"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &e) != nil || e.Event != "reaction_added" {
		return Reaction{}, false
	}
	var r Reaction
	if json.Unmarshal([]byte(e.Data.Reaction), &r) != nil || r.PostID == "" {
		return Reaction{}, false
	}
	return r, true
}

func parsePosted(data []byte) (Posted, bool) {
	var e struct {
		Event string `json:"event"`
		Data  struct {
			Post        string `json:"post"`
			ChannelType string `json:"channel_type"`
			ChannelName string `json:"channel_name"`
			Mentions    string `json:"mentions"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &e) != nil || e.Event != "posted" {
		return Posted{}, false
	}
	var p Post
	if json.Unmarshal([]byte(e.Data.Post), &p) != nil {
		return Posted{}, false
	}
	var mentions []string
	if e.Data.Mentions != "" {
		_ = json.Unmarshal([]byte(e.Data.Mentions), &mentions)
	}
	return Posted{Post: p, ChannelType: e.Data.ChannelType, ChannelName: e.Data.ChannelName, Mentions: mentions}, true
}
