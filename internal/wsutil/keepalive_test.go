package wsutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func dial(t *testing.T, serve func(ctx context.Context, c *websocket.Conn)) *websocket.Conn {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		serve(r.Context(), c)
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	// Pongs are only processed while the client reads, as in real use.
	go func() {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	return conn
}

const (
	fastInterval = 50 * time.Millisecond
	fastTimeout  = 100 * time.Millisecond
)

func TestKeepAliveLeavesQuietHealthyLinkAlone(t *testing.T) {
	conn := dial(t, func(ctx context.Context, c *websocket.Conn) {
		for { // answers pings by reading, sends nothing
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	dead := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go KeepAlive(ctx, conn, fastInterval, fastTimeout, func() { dead <- struct{}{} })

	select {
	case <-dead:
		t.Fatal("a quiet but healthy link was declared dead")
	case <-time.After(600 * time.Millisecond): // a dozen ping intervals
	}
}

func TestKeepAliveDetectsDeadPeer(t *testing.T) {
	conn := dial(t, func(ctx context.Context, c *websocket.Conn) {
		<-ctx.Done() // accepts the handshake, then never reads: no pong ever comes back
	})
	dead := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go KeepAlive(ctx, conn, fastInterval, fastTimeout, func() { dead <- struct{}{} })

	select {
	case <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("a peer that never answers was not detected")
	}
}
