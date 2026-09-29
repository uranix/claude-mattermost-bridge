// Command claude-mattermost-bridge connects a Mattermost bot account to a
// running claude-app-server. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
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
	bridge.New(cfg, client, app, me).Run(ctx)
}
