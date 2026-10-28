// Package telegram wraps the gotd MTProto client: session handling, upload of
// an audio file to Saved Messages and pinning it as profile music via
// account.saveMusic.
package telegram

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrNotAuthorized means the session file is missing or logged out; the
// one-time auth command must be run.
var ErrNotAuthorized = errors.New("telegram session is not authorized (run yttgcast-auth)")

// Options configures the client.
type Options struct {
	APIID         int
	APIHash       string
	SessionFile   string
	SessionString string // optional base64(session JSON) used when SessionFile does not exist
	Log           *slog.Logger
}

// Client is a thin wrapper around telegram.Client.
type Client struct {
	opts Options
	log  *slog.Logger
}

// New creates a client wrapper (no connection yet).
func New(o Options) *Client {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Client{opts: o, log: o.Log}
}

// Run connects, verifies the session is authorized and calls f with a live
// API client. It returns when f returns or the connection dies.
func (c *Client) Run(ctx context.Context, f func(ctx context.Context, api *tg.Client, self *tg.User) error) error {
	client := telegram.NewClient(c.opts.APIID, c.opts.APIHash, telegram.Options{
		SessionStorage: c.storage(),
		NoUpdates:      true,
		Device:         deviceConfig(),
	})
	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			return ErrNotAuthorized
		}
		return f(ctx, client.API(), status.User)
	})
}

// Login runs the interactive auth flow (used by the auth command) and returns
// the logged-in user. The session is persisted to SessionFile.
func (c *Client) Login(ctx context.Context, flow func(ctx context.Context, client *telegram.Client) error) (*tg.User, error) {
	client := telegram.NewClient(c.opts.APIID, c.opts.APIHash, telegram.Options{
		SessionStorage: c.storage(),
		NoUpdates:      true,
		Device:         deviceConfig(),
	})
	var user *tg.User
	err := client.Run(ctx, func(ctx context.Context) error {
		if err := flow(ctx, client); err != nil {
			return err
		}
		u, err := client.Self(ctx)
		if err != nil {
			return err
		}
		user = u
		return nil
	})
	return user, err
}

// SessionString returns the persisted session encoded as base64, suitable for
// TG_SESSION_STRING.
func (c *Client) SessionString() (string, error) {
	b, err := os.ReadFile(c.opts.SessionFile)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Track is what gets uploaded.
type Track struct {
	Path      string
	Title     string
	Performer string
	Duration  int
}

// Pinned is the outcome of PinTrack.
type Pinned struct {
	MessageID  int
	DocumentID int64
}

// PinTrack uploads the file to Saved Messages as an audio document and calls
// account.saveMusic on the resulting document. FLOOD_WAIT errors are honoured.
func (c *Client) PinTrack(ctx context.Context, api *tg.Client, t Track) (Pinned, error) {
	st, err := os.Stat(t.Path)
	if err != nil {
		return Pinned{}, err
	}
	up := uploader.NewUploader(api).WithThreads(2)
	c.log.Info("uploading audio to Saved Messages", "file", filepath.Base(t.Path), "bytes", st.Size())
	var file tg.InputFileClass
	err = withFloodWait(ctx, func() error {
		var err error
		file, err = up.FromPath(ctx, t.Path)
		return err
	})
	if err != nil {
		return Pinned{}, fmt.Errorf("upload: %w", err)
	}

	mimeType := mime.TypeByExtension(filepath.Ext(t.Path))
	if mimeType == "" {
		mimeType = "audio/mpeg"
	}
	var msg *tg.Message
	err = withFloodWait(ctx, func() error {
		doc := message.UploadedDocument(file).
			MIME(mimeType).
			Audio().
			Title(t.Title).
			Performer(t.Performer).
			DurationSeconds(t.Duration).
			Filename(filepath.Base(t.Path))
		var err error
		msg, err = unpack.Message(message.NewSender(api).Self().Media(ctx, doc))
		return err
	})
	if err != nil {
		return Pinned{}, fmt.Errorf("send to saved messages: %w", err)
	}

	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return Pinned{}, fmt.Errorf("unexpected media type %T in sent message", msg.Media)
	}
	document, ok := media.Document.AsNotEmpty()
	if !ok {
		return Pinned{}, errors.New("sent message has an empty document")
	}
	c.log.Info("audio uploaded", "message_id", msg.ID, "document_id", document.ID)

	err = withFloodWait(ctx, func() error {
		ok, err := api.AccountSaveMusic(ctx, &tg.AccountSaveMusicRequest{ID: document.AsInput()})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("account.saveMusic returned false")
		}
		return nil
	})
	if err != nil {
		return Pinned{}, fmt.Errorf("account.saveMusic: %w", err)
	}
	return Pinned{MessageID: msg.ID, DocumentID: document.ID}, nil
}

// withFloodWait retries fn after FLOOD_WAIT_X (up to a few times).
func withFloodWait(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		d, ok := tgerr.AsFloodWait(err)
		if !ok {
			return err
		}
		d += time.Second
		slog.Warn("telegram flood wait", "sleep", d.String(), "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return err
}

// storage returns a session storage that prefers SessionFile and seeds it
// from SessionString when the file does not exist yet.
func (c *Client) storage() session.Storage {
	return &seededStorage{file: &session.FileStorage{Path: c.opts.SessionFile}, seed: c.opts.SessionString, log: c.log}
}

type seededStorage struct {
	file *session.FileStorage
	seed string
	log  *slog.Logger
}

func (s *seededStorage) LoadSession(ctx context.Context) ([]byte, error) {
	b, err := s.file.LoadSession(ctx)
	if err == nil || !errors.Is(err, session.ErrNotFound) || s.seed == "" {
		return b, err
	}
	raw, derr := base64.StdEncoding.DecodeString(s.seed)
	if derr != nil {
		return nil, fmt.Errorf("TG_SESSION_STRING is not valid base64: %w", derr)
	}
	s.log.Info("seeding session file from TG_SESSION_STRING", "path", s.file.Path)
	if err := s.file.StoreSession(ctx, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *seededStorage) StoreSession(ctx context.Context, data []byte) error {
	return s.file.StoreSession(ctx, data)
}

func deviceConfig() telegram.DeviceConfig {
	return telegram.DeviceConfig{
		DeviceModel:    "yttgcast daemon",
		SystemVersion:  "linux",
		AppVersion:     "1.0",
		SystemLangCode: "en",
		LangCode:       "en",
	}
}
