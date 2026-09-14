# syntax=docker/dockerfile:1

# The Go version must match the toolchain in go.mod; an older image refuses to
# build the module outright.
FROM golang:1.25-bookworm AS builder

WORKDIR /src
# The module vendors its dependencies, so the build needs no network.
# .git comes along deliberately: Go stamps the revision into the binary from
# it, which is the only way a running container can say which build it is.
COPY . .

# The repository arrives owned by root from the build context; git refuses to
# read it otherwise and the stamp would silently go missing.
RUN git config --global --add safe.directory /src

# VERSION overrides the stamp for tagged releases. Left unset, the binary
# reports the commit it was built from, including a +dirty marker when the
# tree had uncommitted changes.
ARG VERSION=""
RUN set -eu; \
	ldflags="-s -w"; \
	if [ -n "$VERSION" ]; then ldflags="$ldflags -X main.version=$VERSION"; fi; \
	CGO_ENABLED=0 go build -mod=vendor -ldflags="$ldflags" -o /metrika-alert ./cmd/server

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
