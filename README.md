# metrika-alert

Yandex Metrika event monitor: polls counters for pageview events, evaluates
configurable triggers against them, and delivers alerts via Telegram.

## Quickstart

```bash
cp config.example.yaml ~/.metrika-alert/config.yaml
# edit bot_token, admin_ids, oauth credentials in config.yaml
go build -o metrika-alert ./cmd/server
./metrika-alert
```

Or with a single binary (vendored deps, no Go toolchain needed):

```bash
./metrika-alert
```

Config is loaded from `~/.metrika-alert/config.yaml`, then current dir.
Override: `./metrika-alert /path/to/config.yaml`.

## Config (config.yaml)

| Key | Description | Default |
|---|---|---|
| `database` | SQLite file path (relative to `~/.metrika-alert/`) | `metrika.db` |
| `telegram.bot_token` | Telegram bot token from BotFather | required |
| `telegram.admin_ids` | List of Telegram user IDs allowed to manage the bot | `[]` |
| `telegra...[truncated]