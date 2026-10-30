// Command claude-mattermost-bridge connects a Mattermost bot account to a
// running claude-app-server. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/appclient"
	"github.com/uranix/claude-mattermost-bridge/internal/bridge"
	"github.com/uranix/claude-mattermost-bridge/internal/config"
	"github.com/uranix/claude-mattermost-bridge/internal/mm"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "event" {
		os.Exit(sendEvent(os.Args[2:]))
	}
	check := flag.Bool("check", false, "verify Mattermost credentials, print the bot identity and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	client := mm.New(cfg.MattermostURL, cfg.Token)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	me, err := client.Me(cctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "mattermost:", err)
		os.Exit(1)
	}
	slog.Info("mattermost bot", "username", me.Username, "id", me.ID)
	if *check {
		fmt.Printf("ok: bot @%s (%s)\n", me.Username, me.ID)
		return
	}

	app := appclient.New(cfg.AppServerURL)
	br, err := bridge.New(cfg, client, app, me)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	br.Run(ctx)
}

// Exit codes of the event command.
const (
	exitDelivered   = 0 // accepted, or a duplicate of an accepted event
	exitUnreachable = 1 // the bridge is not running: retry later
	exitInvalid     = 2 // bad arguments, unknown chat: do not retry
	exitRateLimited = 3 // too many events for this chat: retry later
)

// sendEvent is the event command: it sends text to a conversation through the
// bridge's event socket. It needs none of the bridge's configuration.
func sendEvent(args []string) int {
	fs := flag.NewFlagSet("event", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: claude-mattermost-bridge event [--to CHAT] [--source NAME] [--id ID] [--text TEXT]")
		fmt.Fprintln(fs.Output(), "Sends TEXT (or stdin) to a conversation; it is posted in the chat and reaches the agent.")
		fs.PrintDefaults()
	}
	to := fs.String("to", "", "chat ID: thread root post ID or DM channel ID (!status shows it); default: the chat whose directory this runs in")
	source := fs.String("source", "event", "who sends the event, shown in the chat")
	id := fs.String("id", "", "unique event ID; an event with the same source and ID is delivered once, so retries are safe")
	text := fs.String("text", "", "event text; read from stdin if empty")
	socket := fs.String("socket", config.EventSocketPath(), "bridge event socket (BRIDGE_EVENT_SOCKET)")
	timeout := fs.Duration("timeout", 60*time.Second, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return exitInvalid
	}
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "event: no socket (BRIDGE_EVENT_SOCKET=off)")
		return exitInvalid
	}
	if *text == "" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			fmt.Fprintln(os.Stderr, "event: reading stdin:", err)
			return exitInvalid
		}
		*text = string(b)
	}
	cwd, _ := os.Getwd()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	resp, code, err := bridge.SendEvent(ctx, *socket, bridge.EventRequest{To: *to, Cwd: cwd, Source: *source, ID: *id, Text: *text})
	switch {
	case err != nil && code == 0:
		fmt.Fprintln(os.Stderr, "event: bridge not reachable:", err)
		return exitUnreachable
	case err != nil:
		fmt.Fprintln(os.Stderr, "event:", err)
		return exitUnreachable
	case code == http.StatusTooManyRequests:
		fmt.Fprintln(os.Stderr, "event:", resp.Error)
		return exitRateLimited
	case code == http.StatusBadGateway || code >= 500:
		fmt.Fprintln(os.Stderr, "event:", resp.Error)
		return exitUnreachable
	case code != http.StatusOK:
		fmt.Fprintln(os.Stderr, "event:", resp.Error)
		return exitInvalid
	}
	fmt.Printf("%s (chat %s)\n", resp.Status, resp.Chat)
	return exitDelivered
}
