# syntax=docker/dockerfile:1
FROM golang:1.24-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /metrika-alert ./cmd/server

FROM debian:bookworm-slim

RUN groupadd -g 1000 app && useradd -u 1000 -g 1000 -m -d /home/app -s /bin/bash app

COPY --from=builder /metrika-alert /usr/bin/metrika-alert
COPY config.example.yaml /etc/metrika-alert/config.yaml

VOLUME ["/data"]

ENV METRIKA_DB_DIR=/data
ENV HOME=/home/app

EXPOSE 8090

USER app

ENTRYPOINT ["/usr/bin/metrika-alert", "/etc/metrika-alert/config.yaml"]
