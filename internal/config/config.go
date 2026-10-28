// Package config loads the daemon configuration from the environment (and an
// optional .env file in the working directory).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved daemon configuration.
type Config struct {
	// Telegram MTProto user credentials.
	APIID         int
	APIHash       string
	Phone         string
	SessionString string // optional base64 gotd session, used to seed SessionFile
	SessionFile   string

	// YouTube source.
	ChannelID      string // required, e.g. UCxxxxxxxxxxxxxxxxxxxxxx
	ChannelURL     string // used by the yt-dlp fallback feed source; derived from ChannelID if empty
	PollInterval   time.Duration
	InitialBacklog int // how many of the newest videos to process on the very first run
	MaxAttempts    int // give up on a video after this many failed attempts

	// Audio extraction.
	Performer       string // empty = use the YouTube channel name
	AudioFormat     string // m4a | mp3
	YTDLPPath       string
	YTDLPCookies    string
	YTDLPExtraArgs  []string
	YTDLPAutoUpdate bool
	DownloadTimeout time.Duration

	// Storage.
	DataDir   string
	StateFile string
	TempDir   string

	// Logging.
	LogLevel  string
	LogFormat string // json | text
}

// ErrTelegramNotConfigured is returned when the MTProto credentials are missing.
var ErrTelegramNotConfigured = errors.New("TG_API_ID and TG_API_HASH are not set")

// Load reads .env (if present, without overriding real environment variables)
// and builds the Config. Missing Telegram credentials are reported through
// Config.TelegramConfigured rather than as a load error, so the daemon can
// start in an "unconfigured" state and explain what is missing.
func Load() (Config, error) {
	_ = loadDotEnv(".env")

	c := Config{
		APIHash:        os.Getenv("TG_API_HASH"),
		Phone:          os.Getenv("TG_PHONE"),
		SessionString:  os.Getenv("TG_SESSION_STRING"),
		ChannelID:      os.Getenv("YT_CHANNEL_ID"),
		ChannelURL:     os.Getenv("YT_CHANNEL_URL"),
		Performer:      os.Getenv("AUDIO_PERFORMER"),
		AudioFormat:    strings.ToLower(envOr("AUDIO_FORMAT", "m4a")),
		YTDLPPath:      envOr("YTDLP_PATH", "yt-dlp"),
		YTDLPCookies:   os.Getenv("YTDLP_COOKIES_FILE"),
		YTDLPExtraArgs: strings.Fields(os.Getenv("YTDLP_EXTRA_ARGS")),
		DataDir:        envOr("DATA_DIR", "data"),
		LogLevel:       strings.ToLower(envOr("LOG_LEVEL", "info")),
		LogFormat:      strings.ToLower(envOr("LOG_FORMAT", "json")),
	}

	var err error
	if v := os.Getenv("TG_API_ID"); v != "" {
		if c.APIID, err = strconv.Atoi(v); err != nil {
			return c, fmt.Errorf("TG_API_ID must be an integer: %w", err)
		}
	}
	if c.PollInterval, err = envDuration("POLL_INTERVAL_SECONDS", 900*time.Second); err != nil {
		return c, err
	}
	if c.DownloadTimeout, err = envDuration("DOWNLOAD_TIMEOUT_SECONDS", 3600*time.Second); err != nil {
		return c, err
	}
	if c.InitialBacklog, err = envInt("INITIAL_BACKLOG", 1); err != nil {
		return c, err
	}
	if c.MaxAttempts, err = envInt("MAX_ATTEMPTS", 10); err != nil {
		return c, err
	}
	if c.YTDLPAutoUpdate, err = envBool("YTDLP_AUTO_UPDATE", true); err != nil {
		return c, err
	}
	if c.AudioFormat != "m4a" && c.AudioFormat != "mp3" {
		return c, fmt.Errorf("AUDIO_FORMAT must be m4a or mp3, got %q", c.AudioFormat)
	}
	if c.PollInterval < 60*time.Second {
		return c, errors.New("POLL_INTERVAL_SECONDS must be at least 60")
	}

	if c.ChannelURL == "" && c.ChannelID != "" {
		c.ChannelURL = "https://www.youtube.com/channel/" + c.ChannelID + "/videos"
	}

	c.SessionFile = envOr("TG_SESSION_FILE", c.DataDir+"/session.json")
	c.StateFile = envOr("STATE_FILE", c.DataDir+"/state.json")
	c.TempDir = envOr("TEMP_DIR", c.DataDir+"/tmp")
	return c, nil
}

// ChannelConfigured reports whether a YouTube channel to watch is set.
func (c Config) ChannelConfigured() bool { return c.ChannelID != "" }

// TelegramConfigured reports whether the MTProto app credentials are present.
func (c Config) TelegramConfigured() bool { return c.APIID != 0 && c.APIHash != "" }

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return b, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number of seconds", key)
	}
	return time.Duration(n) * time.Second, nil
}

// loadDotEnv reads KEY=VALUE lines from path into the process environment.
// Existing variables win, so container/compose environment always overrides
// the file. Supports comments, blank lines, `export KEY=...` and quoted values.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if i := strings.Index(v, " #"); i >= 0 && !strings.HasPrefix(v, "\"") && !strings.HasPrefix(v, "'") {
			v = strings.TrimSpace(v[:i])
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
