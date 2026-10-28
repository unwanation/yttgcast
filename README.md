# yttgcast

Background daemon (Go) that watches a **YouTube channel**, extracts the audio of every new upload with
`yt-dlp` + `ffmpeg`, sends it to your Telegram **Saved Messages** and pins it as **Profile Music** through
the MTProto user API (`account.saveMusic`).

Originally built to keep the latest [Theo (@t3dotgg)](https://www.youtube.com/@t3dotgg) video in a Telegram
profile, it works with any channel: see `.env.example`.

```
YouTube Atom feed ──(fallback: yt-dlp --flat-playlist)──> new video ids
        │
        ▼
yt-dlp -x  (bestaudio -> m4a/mp3, tags: title = video title, performer = channel name or AUDIO_PERFORMER)
        │
        ▼
Telegram MTProto (gotd/td): upload -> messages.sendMedia to "me" -> InputDocument -> account.saveMusic
        │
        ▼
data/state.json  (processed ids, failures)   +   temp file deleted
```

## Layout

| Path | Purpose |
|---|---|
| `cmd/yttgcast` | the daemon |
| `cmd/yttgcast-auth` | one-time interactive Telegram login (writes `data/session.json`, prints `TG_SESSION_STRING`) |
| `internal/feed` | RSS source + yt-dlp fallback |
| `internal/audio` | yt-dlp/ffmpeg extraction, live/upcoming detection, self-update |
| `internal/telegram` | gotd client, session storage, upload + `account.saveMusic`, FLOOD_WAIT handling |
| `internal/poller` | poll loop, first-run bootstrap, retries with exponential backoff, failure budget |
| `internal/state` | atomic JSON state store |
| `data/` (volume) | `session.json` (secret), `state.json`, `heartbeat`, `tmp/` |

## Setup

1. Copy the config and keep it private:

       cp .env.example .env && chmod 600 .env

2. Create an app at https://my.telegram.org/apps and put `TG_API_ID`, `TG_API_HASH` (and `TG_PHONE`)
   into `.env`.
3. Set `YT_CHANNEL_ID` to the channel you want to follow (the `UC…` id). To find it, open the channel page
   → *About* → *Share channel* → *Copy channel ID*, or run
   `yt-dlp --print channel_id --playlist-items 1 https://www.youtube.com/@handle`.
4. Start the daemon:

       docker compose up -d --build

5. Log in to Telegram once (interactive: login code, 2FA password if enabled):

       docker compose run --rm auth

   This writes `data/session.json`; the running daemon notices it within a minute (no restart needed).
   It also prints a `TG_SESSION_STRING` you can put in `.env` to seed a fresh volume on another host.
6. Watch it:

       docker compose logs -f daemon

Until the channel, the Telegram credentials and the session are in place, the daemon idles and logs what is
missing.

## Configuration

All settings are environment variables (read from `.env`); `.env.example` documents each of them.

| Variable | Default | |
|---|---|---|
| `TG_API_ID`, `TG_API_HASH` | — | required, from my.telegram.org |
| `YT_CHANNEL_ID` | — | required, channel to watch |
| `YT_CHANNEL_URL` | `https://www.youtube.com/channel/<id>/videos` | used by the yt-dlp fallback |
| `AUDIO_PERFORMER` | channel name | performer tag shown in Telegram |
| `AUDIO_FORMAT` | `m4a` | `m4a` or `mp3` |
| `POLL_INTERVAL_SECONDS` | `900` | min 60 |
| `INITIAL_BACKLOG` | `1` | videos processed on the first run |
| `MAX_ATTEMPTS` | `10` | failed polls before a video is skipped |

## Behaviour

- Polls every `POLL_INTERVAL_SECONDS` (default 900). Set 86400 for once a day.
- **First run**: only the newest `INITIAL_BACKLOG` videos (default 1) are processed; older feed entries are
  marked as seen so the channel history is not uploaded.
- Live streams, premieres and streams whose VOD is still processing are skipped and retried on later polls
  without counting as failures.
- Each video gets up to `MAX_ATTEMPTS` (default 10) failed polls, then it is marked as skipped in `state.json`
  (delete the entry to retry). Network/RPC errors inside an attempt are retried with exponential backoff
  (2s → 2min); Telegram `FLOOD_WAIT` is honoured.
- yt-dlp self-updates at start and daily (`YTDLP_AUTO_UPDATE`). If YouTube starts demanding a login for this
  IP, export browser cookies to `data/cookies.txt` and set `YTDLP_COOKIES_FILE=/data/cookies.txt`.
- Structured JSON logs on stdout; `SIGINT`/`SIGTERM` stop the loop cleanly (an in-flight download is
  cancelled and its partial files removed).
- Health: the container is healthy while `data/heartbeat` is fresher than 2 h.

## Operate

    docker compose logs -f daemon                     # follow the log
    docker compose up -d                              # apply .env changes (recreates the container)
    jq . data/state.json                              # what was processed / failed
    docker compose run --rm auth                      # re-login if the session was revoked
    docker compose exec daemon /opt/ytdlp/yt-dlp --version

Forget a video (re-process it): remove its key from `processed` in `data/state.json` and restart.
Unpin a track: Telegram app → profile → music (the daemon only adds).

## Security

`data/session.json` and `TG_SESSION_STRING` grant full access to your Telegram account. Never commit them;
`.env` and `data/` are git-ignored.

## Dev

    go build ./... && go test ./...      # needs Go 1.25; yt-dlp + ffmpeg on PATH to run
    LOG_FORMAT=text DATA_DIR=./data go run ./cmd/yttgcast

## License

[MIT](LICENSE)
