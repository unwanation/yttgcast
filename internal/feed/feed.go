// Package feed lists the latest uploads of a YouTube channel. The primary
// source is the public Atom feed (no API quota); yt-dlp is the fallback.
package feed

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Video is one channel upload.
type Video struct {
	ID        string
	Title     string
	Published time.Time
}

// URL returns the watch URL for the video.
func (v Video) URL() string { return "https://www.youtube.com/watch?v=" + v.ID }

// Source lists recent uploads, oldest first.
type Source interface {
	Latest(ctx context.Context) ([]Video, error)
}

// RSS reads https://www.youtube.com/feeds/videos.xml?channel_id=... .
type RSS struct {
	ChannelID string
	Client    *http.Client
}

type atomFeed struct {
	Entries []struct {
		VideoID   string `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
		Title     string `xml:"title"`
		Published string `xml:"published"`
	} `xml:"entry"`
}

// Latest implements Source.
func (r *RSS) Latest(ctx context.Context) ([]Video, error) {
	url := "https://www.youtube.com/feeds/videos.xml?channel_id=" + r.ChannelID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "yttgcast/1.0 (+https://github.com/unwanation/yttgcast)")
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch feed: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read feed: %w", err)
	}
	return parseAtom(body)
}

func parseAtom(body []byte) ([]Video, error) {
	var f atomFeed
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&f); err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	if len(f.Entries) == 0 {
		return nil, errors.New("parse feed: no entries (feed format changed?)")
	}
	out := make([]Video, 0, len(f.Entries))
	for _, e := range f.Entries {
		if e.VideoID == "" {
			continue
		}
		t, _ := time.Parse(time.RFC3339, e.Published)
		out = append(out, Video{ID: e.VideoID, Title: strings.TrimSpace(e.Title), Published: t})
	}
	sortOldestFirst(out)
	return out, nil
}

// YTDLP lists uploads with `yt-dlp --flat-playlist`; slower, but works when
// the feed is unreachable.
type YTDLP struct {
	Bin        string
	ChannelURL string
	Limit      int
	ExtraArgs  []string
}

// Latest implements Source.
func (y *YTDLP) Latest(ctx context.Context) ([]Video, error) {
	limit := y.Limit
	if limit <= 0 {
		limit = 15
	}
	args := []string{"--flat-playlist", "--no-warnings", "--playlist-end", strconv.Itoa(limit),
		"--print", "%(id)s\t%(timestamp)s\t%(title)s"}
	args = append(args, y.ExtraArgs...)
	args = append(args, y.ChannelURL)
	cmd := exec.CommandContext(ctx, y.Bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var videos []Video
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		v := Video{ID: parts[0], Title: strings.TrimSpace(parts[2])}
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			v.Published = time.Unix(ts, 0).UTC()
		}
		videos = append(videos, v)
	}
	if len(videos) == 0 {
		return nil, errors.New("yt-dlp list: no videos returned")
	}
	sortOldestFirst(videos)
	return videos, nil
}

// Fallback tries Primary and, on error, Secondary.
type Fallback struct {
	Primary, Secondary Source
	Log                *slog.Logger
}

// Latest implements Source.
func (f *Fallback) Latest(ctx context.Context) ([]Video, error) {
	videos, err := f.Primary.Latest(ctx)
	if err == nil {
		return videos, nil
	}
	if f.Secondary == nil {
		return nil, err
	}
	if f.Log != nil {
		f.Log.Warn("primary feed source failed, falling back to yt-dlp", "error", err)
	}
	videos, err2 := f.Secondary.Latest(ctx)
	if err2 != nil {
		return nil, fmt.Errorf("both feed sources failed: rss: %v; yt-dlp: %w", err, err2)
	}
	return videos, nil
}

func sortOldestFirst(v []Video) {
	sort.SliceStable(v, func(i, j int) bool { return v[i].Published.Before(v[j].Published) })
}
