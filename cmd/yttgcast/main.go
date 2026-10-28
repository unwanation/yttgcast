// Command yttgcast is the daemon: it watches a YouTube channel, extracts the
// audio of new uploads, sends it to Telegram Saved Messages and pins it as
// profile music.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/tg"

	"github.com/unwanation/yttgcast/internal/audio"
	"github.com/unwanation/yttgcast/internal/config"
	"github.com/unwanation/yttgcast/internal/feed"
	"github.com/unwanation/yttgcast/internal/poller"
	"github.com/unwanation/yttgcast/internal/state"
	"github.com/unwanation/yttgcast/internal/telegram"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
		return err
	}
	store, err := state.Open(cfg.StateFile)
	if err != nil {
		return err
	}
	extractor := &audio.Extractor{
		Bin: cfg.YTDLPPath, Format: cfg.AudioFormat, TempDir: cfg.TempDir, Performer: cfg.Performer,
		Cookies: cfg.YTDLPCookies, ExtraArgs: cfg.YTDLPExtraArgs, Timeout: cfg.DownloadTimeout, Log: log,
	}
	if v, err := extractor.Version(ctx); err == nil {
		log.Info("yt-dlp", "version", v)
	} else {
		log.Warn("yt-dlp not runnable", "error", err)
	}
	if cfg.YTDLPAutoUpdate {
		go ytdlpUpdater(ctx, extractor, log)
	}

	log.Info("yttgcast starting",
		"channel_id", cfg.ChannelID, "poll_interval", cfg.PollInterval.String(),
		"audio_format", cfg.AudioFormat, "state_file", cfg.StateFile, "processed", store.Count())

	heartbeatFile := cfg.DataDir + "/heartbeat"
	beat := func() { _ = os.WriteFile(heartbeatFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600) }
	beat()

	if !cfg.ChannelConfigured() {
		return waitUnconfigured(ctx, log, beat, "YT_CHANNEL_ID is not set; fill .env and restart the service")
	}
	if !cfg.TelegramConfigured() {
		return waitUnconfigured(ctx, log, beat, "TG_API_ID / TG_API_HASH are not set; fill .env and restart the service")
	}

	p := &poller.Poller{
		Feed: &feed.Fallback{
			Primary:   &feed.RSS{ChannelID: cfg.ChannelID},
			Secondary: &feed.YTDLP{Bin: cfg.YTDLPPath, ChannelURL: cfg.ChannelURL, ExtraArgs: cfg.YTDLPExtraArgs},
			Log:       log,
		},
		Audio:          extractor,
		Telegram:       telegram.New(telegram.Options{APIID: cfg.APIID, APIHash: cfg.APIHash, SessionFile: cfg.SessionFile, SessionString: cfg.SessionString, Log: log}),
		State:          store,
		Interval:       cfg.PollInterval,
		InitialBacklog: cfg.InitialBacklog,
		MaxAttempts:    cfg.MaxAttempts,
		Performer:      cfg.Performer,
		HeartbeatFile:  heartbeatFile,
		Log:            log,
	}

	// Outer loop: (re)connect to Telegram with exponential backoff. gotd
	// reconnects transparently on transient errors; this handles the cases
	// where the whole client gives up, and the "not logged in yet" case.
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 5 * time.Second
	bo.MaxInterval = 5 * time.Minute
	bo.MaxElapsedTime = 0
	for {
		err := p.Telegram.Run(ctx, func(ctx context.Context, api *tg.Client, self *tg.User) error {
			bo.Reset()
			log.Info("telegram connected", "user_id", self.ID, "username", self.Username)
			return p.Run(ctx, api)
		})
		if ctx.Err() != nil {
			log.Info("shutting down")
			return nil
		}
		wait := bo.NextBackOff()
		if errors.Is(err, telegram.ErrNotAuthorized) {
			wait = time.Minute
			log.Error("telegram session missing or logged out; run the auth command "+
				"(docker compose run --rm auth) - checking again in 60s", "session_file", cfg.SessionFile)
		} else {
			log.Error("telegram client stopped; reconnecting", "error", err, "in", wait.String())
		}
		beat() // the process is alive, just not connected
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-time.After(wait):
		}
	}
}

// waitUnconfigured keeps the container alive and healthy (so the deploy
// succeeds and the operator can read the log) and reminds every 10 minutes
// what is missing.
func waitUnconfigured(ctx context.Context, log *slog.Logger, beat func(), msg string) error {
	log.Error("not configured: " + msg)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			beat()
			if i%10 == 0 {
				log.Error("still not configured: " + msg)
			}
		}
	}
}

// ytdlpUpdater self-updates yt-dlp at start and once a day. YouTube changes
// break old versions regularly, so this is what keeps the daemon alive.
func ytdlpUpdater(ctx context.Context, e *audio.Extractor, log *slog.Logger) {
	for {
		if out, err := e.Update(ctx); err != nil {
			log.Warn("yt-dlp self-update failed", "error", err)
		} else {
			log.Info("yt-dlp self-update", "result", out)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
