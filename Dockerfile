# syntax=docker/dockerfile:1

# The Go version must match the toolchain in go.mod; an older image refuses to
# build the module outright.
FROM golang:1.25-bookworm AS builder

WORKDIR /src
# The module vendors its dependencies, so the build needs no network.
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w" -o /metrika-alert ./cmd/server

FROM debian:bookworm-slim

# The service talks HTTPS to Metrika, Telegram and VK Teams. Without the CA
# bundle every one of those calls fails x509 verification.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd -g 1000 app \
 && useradd -u 1000 -g 1000 -m -d /home/app -s /usr/sbin/nologin app

COPY --from=builder /metrika-alert /usr/bin/metrika-alert
COPY config.example.yaml /etc/metrika-alert/config.yaml

# The container runs unprivileged, so the data directory must belong to it —
# a root-owned volume mount leaves the service unable to create its database.
RUN mkdir -p /data && chown 1000:1000 /data
VOLUME ["/data"]

ENV METRIKA_DB_DIR=/data \
    HOME=/home/app \
    TZ=Europe/Moscow

EXPOSE 8090
USER app

ENTRYPOINT ["/usr/bin/metrika-alert", "/etc/metrika-alert/config.yaml"]
