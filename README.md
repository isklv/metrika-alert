# metrika-alert

Yandex Metrika event monitor: polls counters for pageview events, evaluates
configurable triggers against them, and delivers alerts via Telegram.

## Quickstart

```bash
cp config.example.yaml ~/.metrika-alert/config.yaml
# edit bot_token, admin_ids in config.yaml
go build -o metrika-alert ./cmd/server
./metrika-alert
```

Config is loaded from `~/.metrika-alert/config.yaml`, then current dir.
Override: `./metrika-alert /path/to/config.yaml`.

## Docker

Build and run the image:

```bash
docker build -t metrika-alert .
docker run -d --name metrika-alert \
  -v /var/lib/metrika-alert:/data \
  -e METRIKA_BOT_TOKEN=your-bot-token-here \
  -e METRIKA_ADMIN_IDS=123456789 \
  metrika-alert
```

Data (SQLite DB) persists in the mounted `/data` volume. Mount `config.yaml`
into `/etc/metrika-alert/` to override the baked-in example config:

```bash
docker run -d --name metrika-alert \
  -v /var/lib/metrika-alert:/data \
  -v $(pwd)/config.yaml:/etc/metrika-alert/config.yaml:ro \
  metrika-alert
```

## Environment variables

All settings in `config.yaml` can be overridden from the environment. Env vars
take precedence over config file values, so they are useful for container
deployment without rebuilding the image.

| Env var | Config key | Description | Example |
|---|---|---|---|
| `METRIKA_DB_PATH` | `database` | Database file path (absolute or relative to `METRIKA_DB_DIR`) | `/data/metrika.db` |
| `METRIKA_DB_DIR` | — | Directory for relative DB paths | `/var/lib/metrika-alert` |
| `METRIKA_BOT_TOKEN` | `telegram.bot_token` | Telegram bot token from BotFather | `73046424:AA…` |
| `METRIKA_ADMIN_IDS` | `telegram.admin_ids` | Comma-separated Telegram user IDs | `123456789,987654321` |
| `METRIKA_METRIKA_BASE` | `metrika.base_url` | Metrika API base URL | `https://api-metrika.yandex.ru` |
| `METRIKA_METRIKA_LOGS` | `metrika.logs_url` | Logs API base URL | `https://logs.metrika.yandex.ru` |
| `METRIKA_API_LISTEN` | `api.listen_addr` | REST API listen address | `:8090` |
| `METRIKA_API_ENABLED` | `api.enabled` | Enable REST API (`true`/`1`/`yes`) | `true` |
| `METRIKA_REPORT_HOURS` | `report_interval_hours` | Periodic report interval in hours | `6` |
| `METRIKA_PROXY_URL` | `telegram.proxy_url` | SOCKS5/HTTP proxy for Telegram API | `socks5://proxy:1080` |

## Config (config.yaml)

| Key | Description | Default |
|---|---|---|
| `database` | SQLite file path (relative to `~/.metrika-alert/`) | `metrika.db` |
| `telegram.bot_token` | Telegram bot token from BotFather | required |
| `telegram.admin_ids` | List of Telegram user IDs allowed to manage the bot | `[]` |
| `telegram.proxy_url` | SOCKS5/HTTP proxy for Telegram API calls | none |
| `metrika.base_url` | Metrika API base URL | `https://api-metrika.yandex.ru` |
| `metrika.logs_url` | Logs API base URL | `https://logs.metrika.yandex.ru` |
| `api.enabled` | Enable REST API server | `false` |
| `api.listen_addr` | REST API listen address | `:8090` |
| `report_interval_hours` | Periodic report interval in hours | `1` |

## Metrika counter tokens

OAuth tokens for metrika counters are stored per-counter in the database, not
in config. Add counters via the Telegram bot (`/add`) or the REST API endpoint
`POST /api/counters`.
