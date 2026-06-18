# backitup server image: control plane (API + webgui + lifecycle).
# Multi-stage, cgo-free (modernc sqlite) so it cross-compiles cleanly to
# amd64 + arm64. Final image is Alpine + rclone (offsite engine) + CA certs.

# --- build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0: pure-Go build, static binary, no libc dependency at runtime.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backitup-server ./cmd/server

# --- runtime ---
FROM alpine:3.20
RUN apk add --no-cache rclone ca-certificates tzdata wget \
    && adduser -D -u 10001 backitup \
    && mkdir -p /data /srv/backups /srv/authkeys \
    && chown -R backitup /data /srv/backups /srv/authkeys
COPY --from=build /out/backitup-server /usr/local/bin/backitup-server
USER backitup
ENV BACKITUP_DB=/data/backitup.db \
    BACKITUP_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/backitup-server"]
