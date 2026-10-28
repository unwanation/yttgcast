// Package audio downloads the audio track of a YouTube video with yt-dlp and
// tags it (title / performer) with ffmpeg.
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrNotReady means the video cannot be downloaded *yet* (live, upcoming, or
// a stream whose VOD is still being processed). Callers should retry later
// without counting it as a failure.
var ErrNotReady = errors.New("video not ready for download")

// Extractor runs yt-dlp.
type Extractor struct {
	Bin       string
	Format    string // m4a | mp3
	TempDir   string
	Performer string // empty = use the channel name
	Cookies   string
	ExtraArgs []string
	Timeout   time.Duration
	Log       *slog.Logger
}

// Result describes the produced audio file.
type Result struct {
	Path      string
	Title     string
	Performer string
	Duration  int // seconds
	Size      int64
}

// Info is the pre-download probe of a video.
type Info struct {
	Title      string
	Channel    string
	Duration   int
	LiveStatus string
}

// Probe fetches metadata without downloading.
func (e *Extractor) Probe(ctx context.Context, videoID string) (Info, error) {
	args := e.baseArgs()
	args = append(args, "--skip-download", "--print", "%(live_status)s\t%(duration)s\t%(channel)s\t%(title)s", watchURL(videoID))
	out, err := e.run(ctx, args)
	if err != nil {
		return Info{}, err
	}
	line := strings.TrimSpace(out)
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	parts := strings.SplitN(line, "\t", 4)
	if len(parts) != 4 {
		return Info{}, fmt.Errorf("yt-dlp probe: unexpected output %q", line)
	}
	info := Info{LiveStatus: parts[0], Channel: parts[2], Title: parts[3]}
	if info.Channel == "NA" {
		info.Channel = ""
	}
	if d, err := strconv.ParseFloat(parts[1], 64); err == nil {
		info.Duration = int(d)
	}
	return info, nil
}

// Extract downloads the best audio stream of videoID into TempDir as
// <videoID>.<Format>, with title/performer tags embedded.
func (e *Extractor) Extract(ctx context.Context, videoID, title string) (Result, error) {
	info, err := e.Probe(ctx, videoID)
	if err != nil {
		return Result{}, err
	}
	switch info.LiveStatus {
	case "is_live", "is_upcoming", "post_live":
		return Result{}, fmt.Errorf("%w (live_status=%s)", ErrNotReady, info.LiveStatus)
	}
	if title == "" {
		title = info.Title
	}
	performer := e.Performer
	if performer == "" {
		performer = info.Channel
	}
	if err := os.MkdirAll(e.TempDir, 0o700); err != nil {
		return Result{}, err
	}
	final := filepath.Join(e.TempDir, videoID+"."+e.Format)
	_ = os.Remove(final) // leftovers from an interrupted run
	e.cleanupPartials(videoID)

	args := e.baseArgs()
	args = append(args,
		"--no-playlist", "--no-simulate", "--no-progress", "--no-mtime",
		"-f", "bestaudio[ext=m4a]/bestaudio/best",
		"-x", "--audio-format", e.Format, "--audio-quality", "0",
		"--embed-metadata",
		"--parse-metadata", metadataLiteral(performer)+":%(meta_artist)s",
		"--parse-metadata", metadataLiteral(performer)+":%(meta_album_artist)s",
		"--parse-metadata", "%(title)s:%(meta_title)s",
		"-o", filepath.Join(e.TempDir, videoID+".%(ext)s"),
		"--print", "after_move:%(duration)s\t%(filepath)s",
		watchURL(videoID),
	)
	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	out, err := e.run(ctx, args)
	if err != nil {
		e.cleanupPartials(videoID)
		return Result{}, err
	}

	res := Result{Path: final, Title: title, Performer: performer, Duration: info.Duration}
	if line := lastLine(out); line != "" {
		if d, p, ok := strings.Cut(line, "\t"); ok {
			if n, err := strconv.ParseFloat(d, 64); err == nil && n > 0 {
				res.Duration = int(n)
			}
			if p != "" {
				res.Path = p
			}
		}
	}
	st, err := os.Stat(res.Path)
	if err != nil {
		return Result{}, fmt.Errorf("yt-dlp finished but %s is missing: %w", res.Path, err)
	}
	res.Size = st.Size()
	return res, nil
}

// Update runs `yt-dlp -U` (self-update). Errors are returned but callers treat
// them as non-fatal.
func (e *Extractor) Update(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := e.run(ctx, []string{"-U"})
	return strings.TrimSpace(lastLine(out)), err
}

// Version returns the yt-dlp version string.
func (e *Extractor) Version(ctx context.Context) (string, error) {
	out, err := e.run(ctx, []string{"--version"})
	return strings.TrimSpace(out), err
}

func (e *Extractor) baseArgs() []string {
	args := []string{"--no-warnings", "--quiet", "--no-cache-dir"}
	if e.Cookies != "" {
		args = append(args, "--cookies", e.Cookies)
	}
	args = append(args, e.ExtraArgs...)
	return args
}

func (e *Extractor) run(ctx context.Context, args []string) (string, error) {
	bin := e.Bin
	if bin == "" {
		bin = "yt-dlp"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if e.Log != nil {
		e.Log.Debug("running yt-dlp", "args", strings.Join(args, " "))
	}
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		if ctx.Err() != nil {
			return stdout.String(), fmt.Errorf("yt-dlp: %w: %s", ctx.Err(), msg)
		}
		return stdout.String(), fmt.Errorf("yt-dlp: %w: %s", err, msg)
	}
	return stdout.String(), nil
}

func (e *Extractor) cleanupPartials(videoID string) {
	matches, _ := filepath.Glob(filepath.Join(e.TempDir, videoID+".*"))
	for _, m := range matches {
		if strings.HasSuffix(m, "."+e.Format) && !strings.Contains(m, ".part") {
			continue
		}
		_ = os.Remove(m)
	}
}

func (e *Extractor) timeout() time.Duration {
	if e.Timeout <= 0 {
		return time.Hour
	}
	return e.Timeout
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func watchURL(id string) string { return "https://www.youtube.com/watch?v=" + id }

// metadataLiteral escapes s for the FROM side of --parse-metadata, which is an
// output template where "%" starts a field and an unescaped ":" ends FROM.
func metadataLiteral(s string) string {
	return strings.NewReplacer("%", "%%", ":", `\:`).Replace(s)
}
