# ---- build stage --------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yttgcast ./cmd/yttgcast \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/yttgcast-auth ./cmd/yttgcast-auth

# ---- runtime stage ------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates ffmpeg python3 tzdata \
 && adduser -D -u 1000 -h /app app \
 && mkdir -p /opt/ytdlp /data \
 && wget -qO /opt/ytdlp/yt-dlp https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp \
 && chmod 755 /opt/ytdlp/yt-dlp \
 && chown -R app:app /opt/ytdlp /data
COPY --from=build /out/yttgcast /out/yttgcast-auth /usr/local/bin/
USER app
WORKDIR /app
ENV DATA_DIR=/data \
    YTDLP_PATH=/opt/ytdlp/yt-dlp \
    XDG_CACHE_HOME=/tmp \
    LOG_FORMAT=json
VOLUME ["/data"]
# healthy while the daemon wrote a heartbeat in the last 2 hours
HEALTHCHECK --interval=5m --timeout=10s --start-period=2m --retries=3 \
  CMD test -f /data/heartbeat && test $(( $(date +%s) - $(stat -c %Y /data/heartbeat) )) -lt 7200
ENTRYPOINT ["yttgcast"]
