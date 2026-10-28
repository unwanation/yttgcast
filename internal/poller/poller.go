// Package poller ties the pieces together: poll the feed, download new audio,
// upload + pin it on Telegram, and record the result.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/tg"

	"github.com/unwanation/yttgcast/internal/audio"
	"github.com/unwanation/yttgcast/internal/feed"
	"github.com/unwanation/yttgcast/internal/state"
	"github.com/unwanation/yttgcast/internal/telegram"
)

// Poller runs the poll loop.
type Poller struct {
	Feed           feed.Source
	Audio          *audio.Extractor
	Telegram       *telegram.Client
	State          *state.Store
	Interval       time.Duration
	InitialBacklog int
	MaxAttempts    int
	Performer      string // empty = use the channel name reported by yt-dlp
	HeartbeatFile  string
	Log            *slog.Logger
}

// Run polls until ctx is cancelled. It is meant to be called inside
// telegram.Client.Run so api stays valid for the whole loop.
func (p *Poller) Run(ctx context.Context, api *tg.Client) error {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		p.heartbeat()
		if err := p.PollOnce(ctx, api); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.Log.Error("poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// PollOnce fetches the feed once and processes every unseen video.
func (p *Poller) PollOnce(ctx context.Context, api *tg.Client) error {
	videos, err := p.fetchFeed(ctx)
	if err != nil {
		return err
	}
	_ = p.State.TouchPoll()

	if !p.State.Initialized() {
		p.bootstrap(videos)
	}

	var pending []feed.Video
	for _, v := range videos {
		if !p.State.Has(v.ID) {
			pending = append(pending, v)
		}
	}
	p.Log.Info("feed polled", "videos", len(videos), "new", len(pending))

	for i, v := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i > 0 {
			select { // be gentle with both YouTube and Telegram
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		p.process(ctx, api, v)
	}
	return nil
}

// bootstrap marks all but the newest InitialBacklog videos as seen so the
// first run does not upload the whole channel history.
func (p *Poller) bootstrap(videos []feed.Video) {
	keep := p.InitialBacklog
	if keep < 0 {
		keep = 0
	}
	if keep > len(videos) {
		keep = len(videos)
	}
	skipped := 0
	for _, v := range videos[:len(videos)-keep] {
		if p.State.Has(v.ID) {
			continue
		}
		_ = p.State.Mark(v.ID, state.Entry{Title: v.Title, Skipped: true, Reason: "existed before first run"})
		skipped++
	}
	_ = p.State.SetInitialized()
	p.Log.Info("first run: bootstrapped state", "skipped_existing", skipped, "will_process", keep)
}

func (p *Poller) process(ctx context.Context, api *tg.Client, v feed.Video) {
	log := p.Log.With("video_id", v.ID, "title", v.Title)
	log.Info("new video detected", "published", v.Published)

	res, err := p.extract(ctx, v)
	if err != nil {
		if errors.Is(err, audio.ErrNotReady) {
			log.Info("video not downloadable yet (live/upcoming/processing); will retry next poll", "reason", err)
			return
		}
		p.fail(v, fmt.Errorf("extract: %w", err))
		return
	}
	defer func() {
		if rmErr := os.Remove(res.Path); rmErr == nil {
			log.Debug("temporary audio removed", "path", res.Path)
		}
	}()
	log.Info("audio extracted", "path", res.Path, "bytes", res.Size, "duration_s", res.Duration)

	track := telegram.Track{Path: res.Path, Title: v.Title, Performer: p.Performer, Duration: res.Duration}
	if track.Title == "" {
		track.Title = res.Title
	}
	if track.Performer == "" {
		track.Performer = res.Performer
	}
	var pinned telegram.Pinned
	err = retry(ctx, 4, func() error {
		var err error
		pinned, err = p.Telegram.PinTrack(ctx, api, track)
		return err
	}, func(err error, next time.Duration) {
		log.Warn("telegram upload/pin failed, retrying", "error", err, "in", next.String())
	})
	if err != nil {
		p.fail(v, fmt.Errorf("telegram: %w", err))
		return
	}
	if err := p.State.Mark(v.ID, state.Entry{Title: v.Title, MessageID: pinned.MessageID, DocumentID: pinned.DocumentID}); err != nil {
		log.Error("state save failed after successful pin", "error", err)
	}
	log.Info("track pinned to profile music", "message_id", pinned.MessageID, "document_id", pinned.DocumentID)
}

func (p *Poller) extract(ctx context.Context, v feed.Video) (audio.Result, error) {
	var res audio.Result
	err := retry(ctx, 3, func() error {
		var err error
		res, err = p.Audio.Extract(ctx, v.ID, v.Title)
		if errors.Is(err, audio.ErrNotReady) {
			return backoff.Permanent(err)
		}
		return err
	}, func(err error, next time.Duration) {
		p.Log.Warn("audio extraction failed, retrying", "video_id", v.ID, "error", err, "in", next.String())
	})
	return res, err
}

func (p *Poller) fail(v feed.Video, err error) {
	attempts, _ := p.State.RecordFailure(v.ID, err)
	log := p.Log.With("video_id", v.ID, "attempt", attempts, "max_attempts", p.MaxAttempts)
	if p.MaxAttempts > 0 && attempts >= p.MaxAttempts {
		_ = p.State.Mark(v.ID, state.Entry{Title: v.Title, Skipped: true, Reason: "gave up: " + err.Error()})
		log.Error("giving up on video", "error", err)
		return
	}
	log.Error("processing failed; will retry next poll", "error", err)
}

func (p *Poller) fetchFeed(ctx context.Context) ([]feed.Video, error) {
	var videos []feed.Video
	err := retry(ctx, 5, func() error {
		var err error
		videos, err = p.Feed.Latest(ctx)
		return err
	}, func(err error, next time.Duration) {
		p.Log.Warn("feed fetch failed, retrying", "error", err, "in", next.String())
	})
	return videos, err
}

func (p *Poller) heartbeat() {
	if p.HeartbeatFile == "" {
		return
	}
	_ = os.WriteFile(p.HeartbeatFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// retry runs op with exponential backoff (2s, 4s, 8s, ... capped at 2 min)
// up to maxRetries retries, honouring ctx cancellation.
func retry(ctx context.Context, maxRetries uint64, op func() error, notify func(error, time.Duration)) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 2 * time.Second
	b.MaxInterval = 2 * time.Minute
	b.Multiplier = 2
	b.RandomizationFactor = 0.3
	b.MaxElapsedTime = 0
	return backoff.RetryNotify(op, backoff.WithContext(backoff.WithMaxRetries(b, maxRetries), ctx), notify)
}
