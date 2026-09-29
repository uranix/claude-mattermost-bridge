// Package appclient is a JSON-RPC 2.0 client for claude-app-server over
// WebSocket. It reconnects on its own and reports every reconnect to the
// consumer as a synthetic "_reset" notification, because the server keeps
// threads in memory per connection: after a reconnect all thread IDs are dead.
package appclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Error codes of claude-app-server.
const (
	ErrThreadNotFound = -32001
	ErrTurnBusy       = -32003
	ErrNoActiveTurn   = -32004
)

type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// IsCode reports whether err is an RPCError with the given code.
func IsCode(err error, code int) bool {
	var re *RPCError
	return errors.As(err, &re) && re.Code == code
}

var ErrNotConnected = errors.New("app server is not connected")

type Notification struct {
	Method string
	Params json.RawMessage
}

// ResetMethod is the synthetic notification emitted when the connection drops.
const ResetMethod = "_reset"

type Client struct {
	url   string
	notif chan Notification

	mu      sync.Mutex
	conn    *websocket.Conn
	ready   bool
	nextID  int64
	pending map[int64]chan rpcResponse
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func New(url string) *Client {
	return &Client{url: url, notif: make(chan Notification, 4096), pending: map[int64]chan rpcResponse{}}
}

// Notifications delivers server notifications in order, plus ResetMethod.
func (c *Client) Notifications() <-chan Notification { return c.notif }

// Run keeps the connection alive until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("app server connection lost", "err", err)
		c.notif <- Notification{Method: ResetMethod}
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

func (c *Client) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, c.url, nil)
	cancel()
	if err != nil {
		return err
	}
	conn.SetReadLimit(16 << 20)

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(sctx, conn) }()

	defer func() {
		c.mu.Lock()
		c.ready = false
		c.conn = nil
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	ictx, icancel := context.WithTimeout(sctx, 15*time.Second)
	_, err = c.call(ictx, "initialize", map[string]any{"client": map[string]any{"name": "claude-mattermost-bridge"}}, false)
	icancel()
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	slog.Info("connected to app server")

	return <-readErr
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			rpcResponse
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			slog.Warn("bad message from app server", "err", err)
			continue
		}
		if msg.ID != nil && msg.Method == "" {
			c.mu.Lock()
			ch, ok := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			if ok {
				ch <- msg.rpcResponse
			}
			continue
		}
		if msg.Method != "" {
			select {
			case c.notif <- Notification{Method: msg.Method, Params: msg.Params}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Call performs one request and decodes the result into out (may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	raw, err := c.call(ctx, method, params, true)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, needReady bool) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil || (needReady && !c.ready) {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		forget()
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		forget()
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if resp.Error != nil {
			return nil, &RPCError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		return resp.Result, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}
