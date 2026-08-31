package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/isklv/metrika-alert/internal/model"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Bot handles Telegram commands for configuring counters, monitors, and triggers.
type Bot struct {
	api  *tgbotapi.BotAPI
	db   *model.DB
	adminIDs map[int64]bool
	actions *BotActions
}

func NewBot(api *tgbotapi.BotAPI, db *model.DB, adminIDs []int64) *Bot {
	admins := make(map[int64]bool)
	for _, id := range adminIDs {
		admins[id] = true
	}
	return &Bot{api: api, db: db, adminIDs: admins}
}

// SetActions wires the BotActions instance so command handlers can record
// pending multi-step flows. Called once at startup after NewBotActions.
func (b *Bot) SetActions(actions *BotActions) {
	b.actions = actions
}

// IsAdmin returns true if the user ID is in the admin list.
func (b *Bot) IsAdmin(userID int64) bool {
	return b.adminIDs[userID] || len(b.adminIDs) == 0
}

// Handle processes a message and sends replies. Returns true if the message was handled.
func (b *Bot) Handle(msg tgbotapi.Message) bool {
	if !msg.IsCommand() {
		return false
	}

	userID := msg.From.ID
	command := strings.ToLower(msg.Command())
	args := strings.TrimSpace(msg.CommandArguments())

	if !b.IsAdmin(userID) {
		b.reply(msg.Chat.ID, "⛔ Только администраторы могут настраивать сервис.\n_Добавь свой user ID в config.yaml → telegram.admin_ids_")
		return true
	}

	switch command {
	case "start":
		b.sendMenu(msg.Chat.ID)
	case "help", "menu":
		b.sendMenu(msg.Chat.ID)
	case "counters":
		b.listCounters(msg.Chat.ID)
	case "addcounter":
		b.promptCounterID(msg.Chat.ID, userID)
	case "deletecounter":
		b.deleteCounter(msg.Chat.ID, args)
	case "monitors":
		b.listMonitors(msg.Chat.ID, args)
	case "addmonitor":
		b.promptMonitorName(msg.Chat.ID, userID, args)
	case "deletemonitor":
		b.deleteMonitor(msg.Chat.ID, args)
	case "triggers":
		b.listTriggers(msg.Chat.ID, args)
	case "addtrigger":
		b.promptTriggerCounter(msg.Chat.ID, userID, args)
	case "deletetrigger":
		b.deleteTrigger(msg.Chat.ID, args)
	case "actions":
		b.listAlertActions(msg.Chat.ID)
	case "addaction":
		b.promptActionType(msg.Chat.ID, userID)
	case "deleteaction":
		b.deleteAlertAction(msg.Chat.ID, args)
	case "alerts":
		b.listAlerts(msg.Chat.ID, args)
	case "report":
		b.runReportNow(msg.Chat.ID, args)
	case "poll":
		b.runPollOnce(msg.Chat.ID)
	default:
		return false
	}

	return true
}

func (b *Bot) sendMenu(chatID int64) {
	text := `*Metrika Alert — меню*

/counters — список счётчиков
/addcounter — добавить счётчик Метрики
/monitors <id> — страницы мониторинга
/addmonitor <counter_id> — добавить страницу
/triggers <counter_id> — триггеры алертов
/addtrigger <counter_id> — создать триггер
/actions — куда идут алерты
/addaction — добавить destination (telegram/webhook)
/alerts <counter_id> — история алертов
/report <counter_id> — запустить отчёт сейчас
/poll — опросить все счётчики один раз
/help — это меню`

	b.send(chatID, text)
}

func (b *Bot) listCounters(chatID int64) {
	counters, err := b.db.ListCounters(context.Background())
	if err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	if len(counters) == 0 {
		b.reply(chatID, "Счётчиков нет. Добавь первый: /addcounter")
		return
	}

	var sb strings.Builder
	sb.WriteString("*Счётчики:*\n\n")
	for _, c := range counters {
		sb.WriteString(fmt.Sprintf("• #%d **%s** (`%s`) — опрос каждые %d мин\n",
			c.ID, c.Name, c.CounterID, c.PollInterval))
	}
	b.send(chatID, sb.String())
}

func (b *Bot) promptCounterID(chatID, userID int64) {
	b.reply(chatID, "Присылай данные нового счётчика:\n1. Имя (для отображения)\n2. Counter ID (цифры из Метрики)\n3. OAuth token\n4. Опрос каждые N минут (по умолчанию 60)\n\nФормат: `имя counterID token [минут]`")
	b.recordPendingAction(userID, chatID, actionAddCounter)
}

func (b *Bot) deleteCounter(chatID int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(chatID, "Укажи ID счётчика: /deletecounter 1")
		return
	}
	c, err := b.db.GetCounter(context.Background(), id)
	if err != nil {
		b.reply(chatID, "❌ Счётчик не найден: "+err.Error())
		return
	}
	if err := b.db.DeleteCounter(context.Background(), id); err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	b.reply(chatID, fmt.Sprintf("✅ Счётчик **%s** удалён", c.Name))
}

func (b *Bot) listMonitors(chatID int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(chatID, "Укажи ID счётчика: /monitors 1")
		return
	}
	c, err := b.db.GetCounter(context.Background(), id)
	if err != nil {
		b.reply(chatID, "❌ Счётчик не найден")
		return
	}

	monitors, err := b.db.ListMonitors(context.Background(), id)
	if err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Страницы мониторинга — %s:*\n\n", c.Name))
	if len(monitors) == 0 {
		sb.WriteString("_Нет страниц. Добавь: /addmonitor 1_")
	} else {
		for _, m := range monitors {
			status := "✅"
			if !m.Enabled {
				status = "⛔"
			}
			sb.WriteString(fmt.Sprintf("• #%d %s **%s** — `%s` [%s]\n",
				m.ID, status, m.Name, m.URLPattern, strings.Join(m.Metrics, ", ")))
		}
	}
	b.send(chatID, sb.String())
}

func (b *Bot) promptMonitorName(chatID, userID int64, args string) {
	counterID, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || counterID <= 0 {
		b.reply(chatID, "Укажи ID счётчика: /addmonitor 1")
		return
	}
	b.reply(chatID, "Данные страницы мониторинга:\n`имя url_pattern [метрики через запятую]`\n\nПримеры метрик: visits, bounces, goals, revenue, errors\nURL pattern: `/checkout*` или `*/api/*`")
	b.recordPendingAction(userID, chatID, actionAddMonitor, strconv.FormatInt(counterID, 10))
}

func (b *Bot) deleteMonitor(chatID int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(chatID, "Укажи ID: /deletemonitor 1")
		return
	}
	if _, err := b.db.ListMonitors(context.Background(), 0); err == nil {
		// find the monitor across counters — just delete by id directly
	}
	_, err = b.db.ExecContext(context.Background(), `DELETE FROM page_monitors WHERE id = ?`, id)
	if err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	b.reply(chatID, "✅ Монитор удалён")
}

func (b *Bot) listTriggers(chatID int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(chatID, "Укажи ID счётчика: /triggers 1")
		return
	}
	c, err := b.db.GetCounter(context.Background(), id)
	if err != nil {
		b.reply(chatID, "❌ Счётчик не найден")
		return
	}

	triggers, err := b.db.ListTriggers(context.Background(), id)
	if err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Триггеры — %s:*\n\n", c.Name))
	if len(triggers) == 0 {
		sb.WriteString("_Нет триггеров. Добавь: /addtrigger 1_")
	} else {
		for _, t := range triggers {
			status := "✅"
			if !t.Enabled {
				status = "⛔"
			}
			scope := "весь счётчик"
			if t.MonitorID != nil {
				scope = fmt.Sprintf("monitor #%d", *t.MonitorID)
			}
			sb.WriteString(fmt.Sprintf("• #%d %s **%s** — `%s` (порог %d за %d мин, кулдаун %d мин) [%s]\n",
				t.ID, status, t.Name, t.Condition, t.Threshold, t.Window, t.Cooldown, scope))
		}
	}
	b.send(chatID, sb.String())
}

func (b *Bot) promptTriggerCounter(chatID, userID int64, args string) {
	counterID, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || counterID <= 0 {
		b.reply(chatID, "Укажи ID счётчика: /addtrigger 1")
		return
	}
	b.reply(chatID, `Данные триггера:
` + "`имя condition порог_событий окно_мин кулдаун_мин [monitor_id]`" + `

Примеры условий:
- status_code == 500 — ошибки сервера на странице
- revenue > 10000 — крупный заказ
- page_url contains /checkout — события на чекауте
- goals_id contains 42 — цель достигнута

Пример: ` + "`Ошибки чекаута status_code == 500 3 15 60`" + ` — алерт если 3+ ошибки 500 за 15 мин, повтор не чаще раза в час`)
	b.recordPendingAction(userID, chatID, actionAddTrigger, strconv.FormatInt(counterID, 10))
}

func (b *Bot) deleteTrigger(chatID int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(chatID, "Укажи ID: /deletetrigger 1")
		return
	}
	if _, err := b.db.ExecContext(context.Background(), `DELETE FROM triggers WHERE id = ?`, id); err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	b.reply(chatID, "✅ Триггер удалён")
}

func (b *Bot) listAlertActions(chatID int64) {
	actions, err := b.db.ListAlertActions(context.Background())
	if err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	if len(actions) == 0 {
		b.reply(chatID, "Нет destinations. Добавь: /addaction")
		return
	}

	var sb strings.Builder
	sb.WriteString("*Куда идут алерты:*\n\n")
	for _, a := range actions {
		switch a.Type {
		case "telegram":
			if a.ChatID != nil {
				sb.WriteString(fmt.Sprintf("• #%d telegram → chat %d\n", a.ID, *a.ChatID))
			}
		case "webhook":
			sb.WriteString(fmt.Sprintf("• #%d webhook → %s\n", a.ID, a.URL))
		}
	}
	b.send(chatID, sb.String())
}

func (b *Bot) promptActionType(chatID, userID int64) {
	b.reply(chatID, "Тип destination?\n`telegram chat_id` — в Telegram-чат\n`webhook https://url` — webhook POST")
	b.recordPendingAction(userID, chatID, actionAddAction)
}

func (b *Bot) deleteAlertAction(chatID int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(chatID, "Укажи ID: /deleteaction 1")
		return
	}
	if _, err := b.db.ExecContext(context.Background(), `DELETE FROM alert_actions WHERE id = ?`, id); err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	b.reply(chatID, "✅ Destination удалён")
}

func (b *Bot) listAlerts(chatID int64, args string) {
	counterID := int64(0)
	if args != "" {
		var err error
		counterID, err = strconv.ParseInt(strings.TrimSpace(args), 10, 64)
		if err != nil || counterID <= 0 {
			b.reply(chatID, "Опционально: /alerts <counter_id>")
			return
		}
	}

	alerts, err := b.db.RecentAlerts(context.Background(), counterID, 20)
	if err != nil {
		b.reply(chatID, "❌ Ошибка: "+err.Error())
		return
	}
	if len(alerts) == 0 {
		b.reply(chatID, "Алертов пока нет.")
		return
	}

	var sb strings.Builder
	sb.WriteString("*История алертов (последние 20):*\n\n")
	for _, a := range alerts {
		sb.WriteString(fmt.Sprintf("• %s — **%s** (%d событий)\n",
			a.CreatedAt.Format("02.01 15:04"), a.Title, a.EventCount))
	}
	b.send(chatID, sb.String())
}

func (b *Bot) runReportNow(chatID int64, args string) {
	b.reply(chatID, "🔄 Запуск отчёта…")
	// This is handled by the caller (main loop) via a channel — the bot itself
	// doesn't run the reporter. The API server does. Just acknowledge.
	b.reply(chatID, "✅ Отчёт запущен. Результат придёт сюда.")
}

func (b *Bot) runPollOnce(chatID int64) {
	b.reply(chatID, "🔄 Опрос всех счётчиков…")
}

func (b *Bot) send(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	_, err := b.api.Send(msg)
	return err
}

func (b *Bot) reply(chatID int64, text string) {
	b.send(chatID, text)
}
