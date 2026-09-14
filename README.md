# metrika-alert

Мониторинг Яндекс.Метрики: выгружает события счётчиков, проверяет их по
настраиваемым триггерам и доставляет алерты и отчёты в **Telegram**,
**ВК Тимс** и на **webhook**.

Один процесс, SQLite, только исходящие соединения — работает за NAT.

> **Важно о задержке.** Триггеры по событиям строятся на Logs API Метрики,
> а он асинхронный и не отдаёт текущий день: выгрузка заказывается, готовится
> несколько минут и покрывает период, закончившийся примерно сутки назад.
> Это ограничение самого API, а не настройка. Отчёты (`/report`) идут через
> Reporting API и показывают сегодняшний день почти в реальном времени.
> Подробно — [DEPLOY.md §6](DEPLOY.md).

## Быстрый старт

```bash
cp config.example.yaml ~/.metrika-alert/config.yaml
# вписать bot_token и admin_ids
go build -mod=vendor -o metrika-alert ./cmd/server
./metrika-alert
```

Конфиг ищется в таком порядке: путь из аргумента, `~/.metrika-alert/config.yaml`,
`./config.yaml`.

Docker:

```bash
docker compose up -d --build
```

**Полная инструкция по развёртыванию — [DEPLOY.md](DEPLOY.md)** (Docker Compose,
systemd, настройка ботов, резервное копирование, диагностика).

## Каналы доставки

| Канал | Настройка | Адрес destination |
|---|---|---|
| Telegram | `telegram.bot_token` от [@BotFather](https://t.me/BotFather) | числовой `chat_id` |
| ВК Тимс | `vkteams.bot_token` от `@Metabot` | строковый chatId, например `user@corp.example` |
| Webhook | не требует бота | URL для `POST` с JSON |

Каналы независимы: пустой токен просто отключает канал. ВК Тимс работает и с
облаком, и с on-premise — эндпоинт задаётся в `vkteams.base_url`.

Пока ни один destination не создан, алерты уходят администраторам из `admin_ids`.
Как только появился первый destination, рассылка админам прекращается.

## Команды бота

Одинаковы в Telegram и в ВК Тимс.

```
/counters            список счётчиков
/addcounter          добавить счётчик Метрики
/monitors <id>       страницы мониторинга
/addmonitor <id>     добавить страницу
/triggers <id>       триггеры алертов
/addtrigger <id>     создать триггер
/actions             куда идут алерты
/addaction           добавить destination
/alerts [id]         история алертов
/report [id]         запустить отчёт сейчас
/poll                опросить все счётчики один раз
/whoami              показать свой ID
/help                меню
```

Команды принимаются только от пользователей из `admin_ids`. **Пустой список
запрещает всё**: в счётчиках хранятся OAuth-токены Метрики, поэтому токен бота
сам по себе не считается достаточной авторизацией. Бот в ответ на отказ покажет
ваш ID, чтобы его было куда вписать.

## Триггеры

Проверяются по событиям из Logs API, то есть с суточной задержкой (см. врезку
выше). Годятся для «вчера было 300 ошибок 500 на чекауте», не для «упало прямо
сейчас».

| Условие | Смысл |
|---|---|
| `status_code == 500` | ошибки сервера |
| `revenue > 10000` | крупный заказ |
| `page_url contains /checkout` | события на чекауте |
| `goals_id contains 42` | достигнута цель 42 |

Операторы: `==`, `!=`, `>`, `<`, `>=`, `<=`, `contains`. Поля: `page_url`,
`title`, `status_code`, `revenue`, `order_id`, `client_id`, `user_id`,
`goals_id`.

У триггера есть порог (сколько событий в окне), окно в минутах и кулдаун —
минимальный интервал между повторными алертами.

## Конфигурация

| Ключ | Описание | По умолчанию |
|---|---|---|
| `database` | путь к SQLite (относительный — от `~/.metrika-alert/`) | `metrika.db` |
| `telegram.bot_token` | токен от @BotFather | пусто (канал выключен) |
| `telegram.admin_ids` | ID пользователей, которым разрешено управление | `[]` |
| `telegram.proxy_url` | SOCKS5/HTTP-прокси для Telegram API | нет |
| `vkteams.bot_token` | токен от @Metabot | пусто (канал выключен) |
| `vkteams.base_url` | эндпоинт Bot API | `https://myteam.mail.ru/bot/v1` |
| `vkteams.admin_ids` | ID пользователей ВК Тимс | `[]` |
| `vkteams.proxy_url` | прокси для ВК Тимс | нет |
| `metrika.base_url` | хост API Метрики | `https://api-metrika.yandex.net` |
| `metrika.lag_hours` | отставание окна выгрузки Logs API | `26` |
| `metrika.max_window_hours` | максимальный размер одной выгрузки | `24` |
| `api.enabled` | включить REST API | `false` |
| `api.listen_addr` | адрес REST API | `:8090` |
| `api.auth_token` | обязательный bearer-токен для `/api/` | пусто |
| `report_interval_hours` | интервал периодических отчётов, `0` — отключить | `1` |

Любой ключ переопределяется переменной окружения `METRIKA_*` — таблица в
[DEPLOY.md §9](DEPLOY.md#9-переменные-окружения).

OAuth-токены счётчиков хранятся в базе, а не в конфиге, и никогда не
возвращаются REST API. Добавляются через бота (`/addcounter`) или
`POST /api/counters`.

## Разработка

```bash
go build -mod=vendor ./...
go vet -mod=vendor ./...
go test -mod=vendor -race ./...
```

Требуется Go 1.25+ (см. `go.mod`). Зависимости завендорены — сборка работает без
сети.

### Структура

```
cmd/server        точка входа, связывание компонентов
internal/config   конфиг и переопределения из окружения
internal/model    схема SQLite, миграции, доступ к данным
internal/engine   клиенты Logs и Reporting API, опрос, триггеры, отчёты
internal/alert    маршрутизация алертов по каналам
internal/bot      команды чат-бота, общие для всех транспортов
internal/vkteams  клиент Bot API ВК Тимс
internal/api      REST API
```

Логика команд написана один раз и не зависит от транспорта: Telegram и ВК Тимс
различаются только приёмом сообщений и разметкой, поэтому оба реализуют
`bot.Transport`.
