# Развёртывание metrika-alert

Сервис опрашивает счётчики Яндекс.Метрики, проверяет события по настроенным
триггерам и доставляет алерты и периодические отчёты в **Telegram**, **ВК Тимс**
и на **webhook**.

Один процесс, одна SQLite-база, только исходящие соединения — сервис работает за
NAT и не требует публичного адреса.

---

## 1. Что понадобится заранее

| Что | Где взять |
|---|---|
| Токен Telegram-бота | [@BotFather](https://t.me/BotFather) → `/newbot` |
| Свой Telegram user ID | [@userinfobot](https://t.me/userinfobot), либо команда `/whoami` уже запущенному боту |
| Токен бота ВК Тимс | бот **@Metabot** в ВК Тимс → `/newbot` (см. §2) |
| Свой ID в ВК Тимс | команда `/whoami` боту после запуска |
| OAuth-токен Метрики | [oauth.yandex.ru](https://oauth.yandex.ru/) → приложение с правом `metrika:read` |
| Номер счётчика | Метрика → Настройки → номер счётчика (8 цифр) |

Нужен минимум один канал доставки. Telegram и ВК Тимс включаются независимо:
пустой токен просто отключает канал.

---

## 2. Бот в ВК Тимс

1. В ВК Тимс найдите бота **@Metabot** и отправьте `/newbot`.
2. Укажите имя бота (например `metrika-alert`). Metabot вернёт токен вида
   `001.0123456789.0123456789:700000001` — это значение `vkteams.bot_token`.
3. Напишите своему боту любое сообщение и отправьте `/whoami` — он ответит вашим
   ID. Добавьте этот ID в `vkteams.admin_ids`.
4. Чтобы алерты шли в групповой чат — добавьте бота в чат, отправьте там
   `/whoami` и используйте показанный `Чат:` как адрес destination.

**Облако или on-premise.** По умолчанию используется облачный контур
`https://myteam.mail.ru/bot/v1`. Для корпоративной установки задайте свой адрес:

```yaml
vkteams:
  base_url: https://myteam.corp.example/bot/v1
```

или переменной `METRIKA_VKTEAMS_BASE`. Сервис проверяет токен при старте и
падает с понятной ошибкой, если контур недоступен — неверный адрес вы увидите
сразу, а не в момент первого алерта.

---

## 3. Вариант A — Docker Compose (рекомендуется)

```bash
git clone https://github.com/isklv/metrika-alert.git
cd metrika-alert

cat > .env <<'EOF'
METRIKA_BOT_TOKEN=7304642400:AAH...
METRIKA_ADMIN_IDS=123456789
METRIKA_VKTEAMS_TOKEN=001.0123456789.0123456789:700000001
METRIKA_VKTEAMS_ADMINS=admin@corp.example
TZ=Europe/Moscow
EOF
chmod 600 .env

docker compose up -d --build
docker compose logs -f
```

В логе при успешном старте:

```
config loaded from /etc/metrika-alert/config.yaml
db ready at /data/metrika.db
telegram bot ready: @metrika_alert_bot
vkteams bot ready: metrika-alert (700000001) at https://myteam.mail.ru/bot/v1
alert channels: telegram, vkteams
metrika-alert running — press Ctrl+C to stop
```

База лежит в именованном томе `metrika-data` и переживает пересоздание
контейнера. Обновление:

```bash
git pull && docker compose up -d --build
```

### Без Compose

```bash
docker build -t metrika-alert .
docker run -d --name metrika-alert --restart unless-stopped \
  -v metrika-data:/data \
  -e METRIKA_BOT_TOKEN=... \
  -e METRIKA_ADMIN_IDS=123456789 \
  -e METRIKA_VKTEAMS_TOKEN=... \
  -e METRIKA_VKTEAMS_ADMINS=admin@corp.example \
  metrika-alert
```

Чтобы подменить встроенный config.yaml целиком:

```bash
-v $(pwd)/config.yaml:/etc/metrika-alert/config.yaml:ro
```

---

## 4. Вариант B — systemd

```bash
# 1. Собрать статический бинарь (Go 1.25+)
CGO_ENABLED=0 go build -mod=vendor -ldflags="-s -w" -o metrika-alert ./cmd/server
sudo install -m 755 metrika-alert /usr/local/bin/

# 2. Системный пользователь без shell и домашнего каталога
sudo useradd --system --no-create-home --shell /usr/sbin/nologin metrika

# 3. Конфиг и секреты
sudo mkdir -p /etc/metrika-alert
sudo install -m 644 config.example.yaml /etc/metrika-alert/config.yaml
sudo install -m 600 -o root -g root deploy/env.example /etc/metrika-alert/env
sudo nano /etc/metrika-alert/env     # вписать токены

# 4. Служба
sudo install -m 644 deploy/metrika-alert.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now metrika-alert
sudo journalctl -u metrika-alert -f
```

Токены держите в `/etc/metrika-alert/env` (режим `600`), а не в `config.yaml` —
переменные окружения имеют приоритет над файлом. Юнит уже ограничен
(`ProtectSystem=strict`, `NoNewPrivileges`, сетевые семейства только IPv4/IPv6),
база создаётся systemd в `/var/lib/metrika-alert`.

---

## 5. Первичная настройка через бота

Напишите боту в Telegram или ВК Тимс — команды одинаковые в обоих.

```
/addcounter
→ Магазин 12345678 y0_AgAAAAA... 15
   имя, номер счётчика, OAuth-токен, интервал опроса в минутах

/addmonitor 1
→ Чекаут /checkout* visits,revenue
   имя страницы, URL-шаблон, метрики

/addtrigger 1
→ Ошибки чекаута | status_code == 500 | 3 15 60
   имя | условие | порог, окно (мин), кулдаун (мин)

/addaction
→ vkteams team-chat@corp.example
   куда доставлять: telegram <chat_id> | vkteams <chat_id> | webhook <url>
```

Формат триггера разделён вертикальной чертой намеренно: и имя, и условие могут
содержать пробелы, позиционный разбор их бы перепутал.

Условия триггеров проверяются по событиям из Logs API — с задержкой около
суток (§6). Формулируйте их как «за вчера накопилось», а не «прямо сейчас».

| Условие | Смысл |
|---|---|
| `status_code == 500` | ошибки сервера |
| `revenue > 10000` | крупный заказ |
| `page_url contains /checkout` | события на чекауте |
| `goals_id contains 42` | достигнута цель 42 |

Операторы: `==`, `!=`, `>`, `<`, `>=`, `<=`, `contains`.

> ⚠️ **Новый счётчик начинает опрашиваться только после перезапуска сервиса** —
> цикл опроса строится на старте. После `/addcounter` выполните
> `docker compose restart` или `systemctl restart metrika-alert`.
>
> Проверить, что токен и номер счётчика верны, можно сразу командой `/report`:
> она идёт через Reporting API и отвечает за секунды. `/poll` проверяет доступ к
> Logs API, но события принесёт не сразу — см. §6.

Полный список команд — `/help`.

### Куда идут алерты

Пока ни один destination не создан, алерты уходят администраторам из
`admin_ids`. **Как только появился первый destination, рассылка админам
прекращается** — если нужно продолжать получать алерты лично, добавьте себя
через `/addaction` явно.

---

## 6. Как сервис читает Метрику

Сервис использует два разных API Яндекс.Метрики, и разница между ними
определяет, что можно ожидать от алертов.

### Reporting API — отчёты, почти в реальном времени

`GET /stat/v1/data`. Агрегированные цифры, отвечает одним запросом, принимает
`date2=today`. На нём построены периодические отчёты и команда `/report`:
сегодняшний день против вчера, недели и месяца назад — сравниваются
эквивалентные периоды целиком.

### Logs API — сырые события, асинхронный и с задержкой

`POST /management/v1/counter/{id}/logrequests`. На нём построены триггеры по
событиям, и у него две особенности, которые надо понимать до развёртывания.

**Он асинхронный.** Выгрузка не скачивается в момент запроса, а проходит цикл:

```
заказать → Метрика готовит (минуты) → скачать TSV → освободить квоту
```

Сервис ведёт этот цикл сам, по шагу за такт опроса, и хранит состояние выгрузки
в базе — заказ переживает и такт, и перезапуск процесса. Поэтому `/poll` не
приносит события сразу: он либо заказывает выгрузку, либо забирает готовую.

**Он не отдаёт текущий день.** Метрика отклоняет запрос, у которого `date2` —
сегодня, и свежие данные продолжают досчитываться ещё несколько часов. Поэтому
окно выгрузки заканчивается на `lag_hours` (по умолчанию 26) раньше текущего
момента.

> ⚠️ **Отсюда главное следствие: триггеры по событиям срабатывают на данных
> примерно суточной давности.** Это ограничение API, а не настройка сервиса —
> уменьшение `lag_hours` ниже ~26 приводит к отказам выгрузки или к неполным
> данным, а не к более свежим алертам.
>
> Триггеры имеет смысл формулировать как «за вчера накопилось 300 ошибок 500 на
> чекауте», а не «упало прямо сейчас». Для реакции в реальном времени нужен
> мониторинг на стороне приложения, а не веб-аналитика.

### Что настраивается

| Ключ | Смысл | По умолчанию |
|---|---|---|
| `metrika.lag_hours` | на сколько окно выгрузки отстаёт от текущего момента | `26` |
| `metrika.max_window_hours` | максимальный размер одной выгрузки | `24` |

Счётчик, отставший после простоя, догоняет по одному окну за такт — за месяц
простоя будет 30 выгрузок подряд, а не одна, которую Метрика отклонит по квоте.

Незавершённые выгрузки, оставшиеся от убитого процесса, отменяются при старте:
Метрика ограничивает число подготовленных выгрузок на счётчик, и брошенная
занимала бы квоту вечно.

---

## 7. REST API (опционально)

Выключен по умолчанию. Включение:

```yaml
api:
  enabled: true
  listen_addr: "127.0.0.1:8090"
  auth_token: "..."   # openssl rand -hex 32
```

API создаёт счётчики и читает историю алертов, поэтому **без `auth_token`
публиковать порт наружу нельзя** — при пустом токене сервис пишет предупреждение
в лог при старте. В `docker-compose.yml` порт по умолчанию привязан к `127.0.0.1`.

```bash
TOKEN=...
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8090/api/status
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8090/api/counters
curl -H "Authorization: Bearer $TOKEN" -X POST http://127.0.0.1:8090/api/reports?counter_id=1
```

| Метод и путь | Назначение |
|---|---|
| `GET /healthz` | проба живости, без авторизации |
| `GET /api/status` | счётчики и время последнего алерта |
| `GET,POST /api/counters` | список и создание счётчиков |
| `GET,POST /api/monitors?counter_id=N` | страницы мониторинга |
| `GET,POST /api/triggers?counter_id=N` | триггеры |
| `GET /api/alert-actions` | destinations доставки |
| `GET /api/alerts?counter_id=N&limit=50` | история алертов |
| `GET,POST /api/reports?counter_id=N` | снимки отчётов, запуск отчёта |

OAuth-токены счётчиков никогда не отдаются в ответах API.

---

## 8. Резервное копирование

Всё состояние — один файл SQLite (плюс WAL-файлы рядом). Копировать нужно
средствами SQLite, а не `cp`, иначе можно получить незавершённую транзакцию:

```bash
# Docker
docker compose exec metrika-alert sh -c 'sqlite3 /data/metrika.db ".backup /data/backup.db"'
docker compose cp metrika-alert:/data/backup.db ./metrika-$(date +%F).db

# systemd
sudo -u metrika sqlite3 /var/lib/metrika-alert/metrika.db \
  ".backup /var/lib/metrika-alert/backup.db"
```

Восстановление — остановить сервис, положить файл на место `metrika.db`
(удалив `-wal` и `-shm`), запустить.

Схема обновляется автоматически при старте: база, созданная версией без
поддержки ВК Тимс, мигрируется на месте с сохранением всех строк и их ID.

---

## 9. Диагностика

| Симптом | Причина и что делать |
|---|---|
| `WARNING: telegram.admin_ids is empty` | не задан список админов — бот отклоняет все команды. Напишите боту, он ответит вашим ID |
| `⛔ Доступ запрещён` в ответе бота | ваш ID не в `admin_ids`. Бот показывает нужный ID прямо в сообщении |
| `vkteams: verify token against ...: Invalid token` | неверный `vkteams.bot_token` или указан облачный `base_url` для on-premise контура |
| `vkteams ... : HTTP 404` | неверный `base_url` — проверьте, что путь заканчивается на `/bot/v1` |
| `poller started: no counters configured` | счётчиков нет — добавьте через `/addcounter` |
| Счётчик добавлен, но опроса нет | перезапустите сервис: цикл опроса строится при старте |
| `x509: certificate signed by unknown authority` | в образе нет CA-сертификатов. Пересоберите образ текущим Dockerfile |
| `database is locked` | база на сетевом томе (NFS/SMB). Перенесите на локальный диск |
| `metrika API 403: Access denied` | OAuth-токен счётчика не даёт доступа. Нужно право `metrika:read`, а для разбивки по целям — доступ к управлению счётчиком |
| `Metrika will not export … (quota allows N day(s))` | исчерпана квота Logs API. Уменьшите `max_window_hours` либо дождитесь восстановления квоты |
| `export N ended as "processing_failed"` | Метрика не смогла подготовить выгрузку. Сервис сам закажет её заново на тот же период, события не теряются |
| Алерты приходят с задержкой около суток | так и работает Logs API, см. §6. Это не настройка |
| `/poll` не принёс событий | выгрузка заказана и готовится. Повторите через несколько минут |
| `alert dropped — no destinations and no admins configured` | не задан ни один получатель: заполните `admin_ids` или добавьте `/addaction` |
| Алерты перестали приходить лично после `/addaction` | так и задумано: появился явный destination. Добавьте себя через `/addaction` |

Логи:

```bash
docker compose logs -f --tail 100        # Docker
journalctl -u metrika-alert -f           # systemd
```

---

## 10. Переменные окружения

Переопределяют одноимённые ключи `config.yaml` — удобно для контейнеров, чтобы
не пересобирать образ ради секрета.

| Переменная | Ключ конфига | Пример |
|---|---|---|
| `METRIKA_DB_PATH` | `database` | `/data/metrika.db` |
| `METRIKA_DB_DIR` | — | `/var/lib/metrika-alert` (только для относительных путей) |
| `METRIKA_BOT_TOKEN` | `telegram.bot_token` | `7304642400:AAH…` |
| `METRIKA_ADMIN_IDS` | `telegram.admin_ids` | `123456789,987654321` |
| `METRIKA_PROXY_URL` | `telegram.proxy_url` | `socks5://proxy:1080` |
| `METRIKA_VKTEAMS_TOKEN` | `vkteams.bot_token` | `001.0123…:700000001` |
| `METRIKA_VKTEAMS_BASE` | `vkteams.base_url` | `https://myteam.corp.example/bot/v1` |
| `METRIKA_VKTEAMS_ADMINS` | `vkteams.admin_ids` | `admin@corp.example,ops@corp.example` |
| `METRIKA_VKTEAMS_PROXY` | `vkteams.proxy_url` | `http://proxy:3128` |
| `METRIKA_METRIKA_BASE` | `metrika.base_url` | `https://api-metrika.yandex.net` |
| `METRIKA_LAG_HOURS` | `metrika.lag_hours` | `26` |
| `METRIKA_MAX_WINDOW_HOURS` | `metrika.max_window_hours` | `24` |
| `METRIKA_API_ENABLED` | `api.enabled` | `true` |
| `METRIKA_API_LISTEN` | `api.listen_addr` | `127.0.0.1:8090` |
| `METRIKA_API_TOKEN` | `api.auth_token` | `openssl rand -hex 32` |
| `METRIKA_REPORT_HOURS` | `report_interval_hours` | `6` (`0` — отключить отчёты) |
