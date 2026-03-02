package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/bishenghua/lazycoding/internal/agent/claude"
	"github.com/bishenghua/lazycoding/internal/channel"
	dtadapter "github.com/bishenghua/lazycoding/internal/channel/dingtalk"
	fsadapter "github.com/bishenghua/lazycoding/internal/channel/feishu"
	qqadapter "github.com/bishenghua/lazycoding/internal/channel/qqbot"
	tgadapter "github.com/bishenghua/lazycoding/internal/channel/telegram"
	wwadapter "github.com/bishenghua/lazycoding/internal/channel/wework"
	"github.com/bishenghua/lazycoding/internal/config"
	"github.com/bishenghua/lazycoding/internal/lazycoding"
	"github.com/bishenghua/lazycoding/internal/session"
	"github.com/bishenghua/lazycoding/internal/transcribe"
)

func main() {
	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// Configure structured logger.
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Log.Level)}
	if cfg.Log.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))

	// Build the speech-to-text transcriber (nil if disabled).
	tr, err := transcribe.New(cfg.Transcription)
	if err != nil {
		slog.Error("transcription init failed", "err", err)
		os.Exit(1)
	}
	if tr != nil {
		slog.Info("transcription enabled", "backend", cfg.Transcription.Backend)
	}

	// Initialise all configured adapters; at least one must be present.
	var adapters []channel.Channel

	if cfg.Feishu.AppID != "" {
		fsCh, err := fsadapter.New(cfg, tr)
		if err != nil {
			slog.Error("feishu adapter init", "err", err)
			os.Exit(1)
		}
		adapters = append(adapters, fsCh)
		slog.Info("feishu channel enabled")
	}

	if cfg.Telegram.Token != "" {
		tgCh, err := tgadapter.New(cfg, tr)
		if err != nil {
			slog.Error("telegram adapter init", "err", err)
			os.Exit(1)
		}
		adapters = append(adapters, tgCh)
		slog.Info("telegram channel enabled")
	}

	if cfg.QQBot.AppID != "" {
		qqCh, err := qqadapter.New(cfg)
		if err != nil {
			slog.Error("qqbot adapter init", "err", err)
			os.Exit(1)
		}
		adapters = append(adapters, qqCh)
		slog.Info("qqbot channel enabled")
	}

	if cfg.DingTalk.AppKey != "" {
		dtCh, err := dtadapter.New(cfg)
		if err != nil {
			slog.Error("dingtalk adapter init", "err", err)
			os.Exit(1)
		}
		adapters = append(adapters, dtCh)
		slog.Info("dingtalk channel enabled")
	}

	if cfg.WeWork.CorpID != "" {
		wwCh, err := wwadapter.New(cfg)
		if err != nil {
			slog.Error("wework adapter init", "err", err)
			os.Exit(1)
		}
		adapters = append(adapters, wwCh)
		slog.Info("wework channel enabled")
	}

	if len(adapters) == 0 {
		slog.Error("no platform configured: set feishu.app_id, telegram.token, qqbot.app_id, dingtalk.app_key, or wework.corp_id in config.yaml")
		os.Exit(1)
	}

	ch := channel.NewMultiAdapter(adapters...)

	runner := claude.New(&cfg.Claude)

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("cannot find home directory", "err", err)
		os.Exit(1)
	}
	sessionFile := filepath.Join(home, ".lazycoding", "sessions.json")
	store, err := session.NewFileStore(sessionFile)
	if err != nil {
		slog.Error("session store init failed", "err", err)
		os.Exit(1)
	}
	slog.Info("session store loaded", "path", sessionFile)

	b := lazycoding.New(ch, runner, store, cfg)

	// Graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutting down…")
		cancel()
	}()

	slog.Info("lazycoding started", "config", cfgPath)
	if err := b.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("bot exited with error", "err", err)
		os.Exit(1)
	}
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
