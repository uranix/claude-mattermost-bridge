// Package wsutil holds small helpers shared by the bridge's WebSocket clients.
package wsutil

import (
	"context"
	"log/slog"
	"time"

	"github.com/coder/websocket"
)

// Default ping timing for the bridge's connections.
const (
	PingInterval = 30 * time.Second
	PingTimeout  = 15 * time.Second
)

// KeepAlive pings conn every interval and calls onDead when a ping gets no
// answer within timeout. It returns when ctx is done.
//
// A read deadline cannot do this job: control frames are handled inside Read
// without returning, so a healthy but quiet link would look dead.
func KeepAlive(ctx context.Context, conn *websocket.Conn, interval, timeout time.Duration, onDead func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("websocket ping failed", "err", err)
			onDead()
			return
		}
	}
}
