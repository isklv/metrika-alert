package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/isklv/metrika-alert/internal/model"
)

// Interactive flows: a command asks for data, the user's next plain message
// carries it. State lives per user so two admins can configure in parallel.
const (
	actionAddCounter = "addcounter"
	actionAddMonitor = "addmonitor"
	actionAddTrigger = "addtrigger"
	actionAddAction  = "addaction"
)

type pendingAction struct {
	kind   string
	chatID string
	// counterID is the counter the flow is scoped to, for monitors and triggers.
	counterID int64
}

func (b *Bot) setPending(userID string, p *pendingAction) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[userID] = p
}

func (b *Bot) clearPending(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, userID)
}

// handlePending routes a plain message to the flow the user started, if any.
func (b *Bot) handlePending(ctx context.Context, userID, text string) bool {
	b.mu.Lock()
	p, ok := b.pending[userID]
	b.mu.Unlock()
	if !ok {
		return false
	}

	var done bool
	switch p.kind {
	case actionAddCounter:
		done = b.saveCounter(ctx, p, text)
	case actionAddMonitor:
		done = b.saveMonitor(ctx, p, text)
	case actionAddTrigger:
		done = b.saveTrigger(ctx, p, text)
	case actionAddAction:
		done = b.saveAlertAction(ctx, p, text)
	default:
		b.clearPending(userID)
		return false
	}

	// The flow stays open after a malformed answer so the user can simply retry.
	if done {
		b.clearPending(userID)
	}
	return true
}

// ---- add counter ----

func (b *Bot) promptAddCounter(ctx context.Context, msg Message) {
	b.setPending(msg.UserID, &pendingAction{kind: actionAddCounter, chatID: msg.ChatID})
	b.reply(ctx, msg.ChatID, `Пришли данные счётчика одним сообщением:

`+"`имя counterID oauth_token [интервал_опроса_мин]`"+`

Пример: `+"`Магазин 12345678 y0_AgAAAAA... 15`"+`

Интервал по умолчанию — 60 минут. Отменить: /help`)
}

func (b *Bot) saveCounter(ctx context.Context, p *pendingAction, text string) bool {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.reply(ctx, p.chatID, "Неверный формат. Нужно минимум три поля:\n`имя counterID oauth_token [минут]`")
		return false
	}

	if _, err := strconv.Atoi(parts[1]); err != nil {
		b.reply(ctx, p.chatID, "Counter ID должен быть числом из Метрики, например `12345678`.")
		return false
	}

	interval := 60
	if len(parts) >= 4 {
		n, err := strconv.Atoi(parts[3])
		if err != nil || n <= 0 {
			b.reply(ctx, p.chatID, "Интервал опроса должен быть положительным числом минут.")
			return false
		}
		interval = n
	}

	counter := &model.Counter{
		Name:         parts[0],
		CounterID:    parts[1],
		OAuthToken:   parts[2],
		PollInterval: interval,
	}
	if err := b.db.CreateCounter(ctx, counter); err != nil {
		b.replyErr(ctx, p.chatID, err)
		return false
	}

	b.reply(ctx, p.chatID, fmt.Sprintf(
		"✅ Счётчик *%s* добавлен (`%s`, опрос каждые %d мин)\n\n⚠️ Опрос начнётся после перезапуска сервиса.\n\nДалее: /addmonitor %d — страницы, /addtrigger %d — алерты",
		counter.Name, counter.CounterID, interval, counter.ID, counter.ID))
	return true
}

// ---- add monitor ----

func (b *Bot) promptAddMonitor(ctx context.Context, msg Message, args string) {
	counterID, ok := b.parseID(ctx, msg.ChatID, args, "/addmonitor 1")
	if !ok {
		return
	}
	if _, err := b.db.GetCounter(ctx, counterID); err != nil {
		b.reply(ctx, msg.ChatID, "❌ Счётчик не найден. Список: /counters")
		return
	}

	b.setPending(msg.UserID, &pendingAction{kind: actionAddMonitor, chatID: msg.ChatID, counterID: counterID})
	b.reply(ctx, msg.ChatID, `Пришли данные страницы одним сообщением:

`+"`имя url_pattern [метрики_через_запятую]`"+`

URL pattern: `+"`/checkout*`"+`, `+"`*/api/*`"+`, `+"`*`"+` — любая страница
Метрики: visits, bounces, goals, revenue, errors (по умолчанию все)

Пример: `+"`Чекаут /checkout* visits,revenue`")
}

func (b *Bot) saveMonitor(ctx context.Context, p *pendingAction, text string) bool {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.reply(ctx, p.chatID, "Неверный формат. Нужно минимум два поля:\n`имя url_pattern [метрики]`")
		return false
	}

	metrics := []string{"visits", "bounces", "goals", "revenue"}
	if len(parts) >= 3 {
		metrics = splitCSV(strings.Join(parts[2:], ""))
		if len(metrics) == 0 {
			b.reply(ctx, p.chatID, "Список метрик пуст. Укажи их через запятую или пропусти поле.")
			return false
		}
	}

	monitor := &model.PageMonitor{
		CounterID:  p.counterID,
		Name:       parts[0],
		URLPattern: parts[1],
		Metrics:    metrics,
		Enabled:    true,
	}
	if err := b.db.CreateMonitor(ctx, monitor); err != nil {
		b.replyErr(ctx, p.chatID, err)
		return false
	}

	b.reply(ctx, p.chatID, fmt.Sprintf(
		"✅ Монитор #%d *%s* создан (`%s`, метрики: %s)\n\n/addtrigger %d — повесить алерт на эту страницу (monitor_id = %d)",
		monitor.ID, monitor.Name, monitor.URLPattern, strings.Join(metrics, ", "), p.counterID, monitor.ID))
	return true
}

// ---- add trigger ----

func (b *Bot) promptAddTrigger(ctx context.Context, msg Message, args string) {
	counterID, ok := b.parseID(ctx, msg.ChatID, args, "/addtrigger 1")
	if !ok {
		return
	}
	if _, err := b.db.GetCounter(ctx, counterID); err != nil {
		b.reply(ctx, msg.ChatID, "❌ Счётчик не найден. Список: /counters")
		return
	}

	b.setPending(msg.UserID, &pendingAction{kind: actionAddTrigger, chatID: msg.ChatID, counterID: counterID})
	b.reply(ctx, msg.ChatID, `Пришли данные триггера одним сообщением:

`+"`имя | условие | порог окно_мин кулдаун_мин [monitor_id]`"+`

Условия:
• `+"`status_code == 500`"+` — ошибки сервера
• `+"`revenue > 10000`"+` — крупный заказ
• `+"`page_url contains /checkout`"+` — события на чекауте
• `+"`goals_id contains 42`"+` — достигнута цель 42

Пример: `+"`Ошибки чекаута | status_code == 500 | 3 15 60`"+`
— алерт, если 3+ ошибки за 15 минут, не чаще раза в час.`)
}

// saveTrigger parses the pipe-delimited trigger form. Separating the name and
// the condition with "|" is what makes this unambiguous: both may contain
// spaces, so a purely positional format cannot tell where one ends.
func (b *Bot) saveTrigger(ctx context.Context, p *pendingAction, text string) bool {
	fields := strings.Split(text, "|")
	if len(fields) != 3 {
		b.reply(ctx, p.chatID, "Неверный формат — нужны три части через `|`:\n`имя | условие | порог окно_мин кулдаун_мин [monitor_id]`")
		return false
	}

	name := strings.TrimSpace(fields[0])
	condition := strings.TrimSpace(fields[1])
	if name == "" || condition == "" {
		b.reply(ctx, p.chatID, "Имя и условие не должны быть пустыми.")
		return false
	}

	nums := strings.Fields(fields[2])
	if len(nums) < 3 || len(nums) > 4 {
		b.reply(ctx, p.chatID, "В третьей части нужны три числа (и опционально monitor_id):\n`порог окно_мин кулдаун_мин [monitor_id]`")
		return false
	}

	threshold, err1 := strconv.Atoi(nums[0])
	window, err2 := strconv.Atoi(nums[1])
	cooldown, err3 := strconv.Atoi(nums[2])
	if err1 != nil || err2 != nil || err3 != nil || threshold <= 0 || window <= 0 || cooldown < 0 {
		b.reply(ctx, p.chatID, "Порог и окно должны быть положительными числами, кулдаун — неотрицательным.")
		return false
	}

	var monitorID *int64
	if len(nums) == 4 {
		mid, err := strconv.ParseInt(nums[3], 10, 64)
		if err != nil || mid <= 0 {
			b.reply(ctx, p.chatID, "monitor_id должен быть положительным числом. Список: /monitors "+strconv.FormatInt(p.counterID, 10))
			return false
		}
		monitors, err := b.db.ListMonitors(ctx, p.counterID)
		if err != nil {
			b.replyErr(ctx, p.chatID, err)
			return false
		}
		found := false
		for _, m := range monitors {
			if m.ID == mid {
				found = true
				break
			}
		}
		if !found {
			b.reply(ctx, p.chatID, fmt.Sprintf("Монитор #%d не принадлежит счётчику #%d. Список: /monitors %d", mid, p.counterID, p.counterID))
			return false
		}
		monitorID = &mid
	}

	trigger := &model.Trigger{
		CounterID: p.counterID,
		MonitorID: monitorID,
		Name:      name,
		Condition: condition,
		Threshold: threshold,
		Window:    window,
		Cooldown:  cooldown,
		Enabled:   true,
	}
	if err := b.db.CreateTrigger(ctx, trigger); err != nil {
		b.replyErr(ctx, p.chatID, err)
		return false
	}

	scope := "весь счётчик"
	if monitorID != nil {
		scope = fmt.Sprintf("monitor #%d", *monitorID)
	}
	b.reply(ctx, p.chatID, fmt.Sprintf(
		"✅ Триггер #%d *%s* создан\n`%s` — алерт при %d+ событиях за %d мин\nОбласть: %s | Кулдаун: %d мин",
		trigger.ID, name, condition, threshold, window, scope, cooldown))
	return true
}

// ---- add alert action ----

func (b *Bot) promptAddAction(ctx context.Context, msg Message) {
	b.setPending(msg.UserID, &pendingAction{kind: actionAddAction, chatID: msg.ChatID})
	b.reply(ctx, msg.ChatID, `Куда доставлять алерты? Пришли одной строкой:

`+"`telegram <chat_id>`"+` — чат Telegram, например `+"`telegram -1001234567890`"+`
`+"`vkteams <chat_id>`"+` — чат ВК Тимс, например `+"`vkteams user@corp.ru`"+`
`+"`webhook <url>`"+` — HTTP POST, например `+"`webhook https://ops.example.com/hook`"+`

Свой ID можно узнать командой /whoami.`)
}

func (b *Bot) saveAlertAction(ctx context.Context, p *pendingAction, text string) bool {
	parts := strings.Fields(text)
	if len(parts) != 2 {
		b.reply(ctx, p.chatID, "Нужны ровно два поля: `тип адрес`\nНапример: `vkteams user@corp.ru`")
		return false
	}

	kind, target := strings.ToLower(parts[0]), parts[1]
	action := &model.AlertAction{Type: kind}

	switch kind {
	case "telegram":
		chatID, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			b.reply(ctx, p.chatID, "chat_id в Telegram — число (у групп отрицательное). Узнать свой: /whoami")
			return false
		}
		action.ChatID = &chatID
		action.Target = target
		action.Name = "telegram-" + target
	case "vkteams":
		action.Target = target
		action.Name = "vkteams-" + target
	case "webhook":
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			b.reply(ctx, p.chatID, "URL вебхука должен начинаться с `http://` или `https://`.")
			return false
		}
		action.URL = target
		action.Name = "webhook-" + target
	default:
		b.reply(ctx, p.chatID, "Неизвестный тип. Доступны: `telegram`, `vkteams`, `webhook`.")
		return false
	}

	if err := b.db.CreateAlertAction(ctx, action); err != nil {
		b.replyErr(ctx, p.chatID, err)
		return false
	}

	b.reply(ctx, p.chatID, fmt.Sprintf(
		"✅ Destination #%d добавлен: %s → `%s`\n\n⚠️ Как только появился хотя бы один destination, алерты перестают идти админам из config.yaml — добавь себя явно, если нужно.",
		action.ID, action.Type, action.Destination()))
	return true
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
