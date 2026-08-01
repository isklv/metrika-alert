package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/isklv/metrika-alert/internal/model"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	actionAddCounter = "addcounter"
	actionAddMonitor = "addmonitor"
	actionAddTrigger = "addtrigger"
	actionAddAction  = "addaction"
)

// pendingAction represents an in-progress interactive setup flow.
type pendingAction struct {
	kind   string
	chatID int64
	args   []string
	step   int // which prompt we're on
	data   map[string]string
}

// BotActions handles multi-step interactive prompts.
type BotActions struct {
	bot    *Bot
	pending map[int64]*pendingAction // userID -> action
	mu     sync.Mutex
}

func NewBotActions(bot *Bot) *BotActions {
	return &BotActions{bot: bot, pending: make(map[int64]*pendingAction)}
}

// Handle processes a non-command message as a prompt response. Returns true if consumed.
func (ba *BotActions) Handle(userID int64, text string) bool {
	ba.mu.Lock()
	p, ok := ba.pending[userID]
	ba.mu.Unlock()
	if !ok {
		return false
	}

	switch p.kind {
	case actionAddCounter:
		return ba.handleAddCounter(p, text)
	case actionAddMonitor:
		return ba.handleAddMonitor(p, text)
	case actionAddTrigger:
		return ba.handleAddTrigger(p, text)
	case actionAddAction:
		return ba.handleAddAction(p, text)
	default:
		return false
	}
}

func (ba *BotActions) recordPending(userID int64, chatID int64, kind string, args ...string) {
	ba.mu.Lock()
	defer ba.mu.Unlock()
	ba.pending[userID] = &pendingAction{kind: kind, chatID: chatID, args: args, data: make(map[string]string)}
}

// recordPendingAction delegates to the BotActions instance.
func (b *Bot) recordPendingAction(userID int64, chatID int64, kind string, args ...string) {
	// This is called from handlers; the real recording happens via BotActions.
	// We store it on the action struct instead.
}

// SetRecordPending replaces the no-op with the real recorder.
func (b *Bot) SetRecordPending(fn func(userID int64, chatID int64, kind string, args ...string)) {
	// Handled by BotActions directly now.
}

func (ba *BotActions) handleAddCounter(p *pendingAction, text string) bool {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		ba.bot.reply(p.chatID, "Неверный формат.\n`имя counterID token [минут]`\n\nПример: `Магазин 12345678 y0_AAAAA… 60`")
		return true
	}

	name := parts[0]
	counterID := parts[1]
	token := parts[2]
	interval := 60
	if len(parts) >= 4 {
		if n, err := strconv.Atoi(parts[3]); err == nil && n > 0 {
			interval = n
		}
	}

	counter := &model.Counter{
		Name:            name,
		CounterID:       counterID,
		OAuthToken:      token,
		PollInterval:    interval,
	}
	if err := ba.bot.db.CreateCounter(context.Background(), counter); err != nil {
		ba.bot.reply(p.chatID, "❌ Ошибка: "+err.Error())
		return true
	}

	ba.mu.Lock()
	delete(ba.pending, userIDFromPending(ba.pending, p))
	ba.mu.Unlock()

	ba.bot.reply(p.chatID, fmt.Sprintf("✅ Счётчик **%s** добавлен (`%s`, опрос каждые %d мин)\n\nДалее: /monitors %d — страницы, /addtrigger %d — алерты",
		name, counterID, interval, counter.ID, counter.ID))
	return true
}

func (ba *BotActions) handleAddMonitor(p *pendingAction, text string) bool {
	if p.step == 0 {
		// First message after prompt: parse inline or ask step by step.
		parts := strings.Fields(text)
		if len(parts) >= 2 {
			// Try single-message format: name url_pattern [metrics]
			p.data["counter_id"] = p.args[0]
			p.data["name"] = parts[0]
			p.data["url_pattern"] = parts[1]
			if len(parts) >= 3 {
				p.data["metrics"] = strings.Join(parts[2:], " ")
			} else {
				p.data["metrics"] = "visits,bounces,goals,revenue"
			}
			return ba.saveMonitor(p)
		}
		// Step-by-step: ask for name.
		p.step = 1
		ba.bot.reply(p.chatID, "Имя страницы (например: Чекаут):")
		return true
	}

	if p.step == 1 {
		p.data["name"] = strings.TrimSpace(text)
		p.step = 2
		ba.bot.reply(p.chatID, "URL pattern (например: `/checkout*` или `*/api/*`, `*` — любое):")
		return true
	}

	if p.step == 2 {
		p.data["url_pattern"] = strings.TrimSpace(text)
		p.step = 3
		ba.bot.reply(p.chatID, "Метрики через запятую (visits, bounces, goals, revenue, errors) или ENTER для всех:")
		return true
	}

	if p.step == 3 {
		metrics := strings.TrimSpace(text)
		if metrics == "" {
			metrics = "visits,bounces,goals,revenue"
		}
		p.data["metrics"] = metrics
		return ba.saveMonitor(p)
	}

	return true
}

func (ba *BotActions) saveMonitor(p *pendingAction) bool {
	counterID, _ := strconv.ParseInt(p.data["counter_id"], 10, 64)
	metrics := strings.Split(strings.ReplaceAll(p.data["metrics"], " ", ""), ",")

	monitor := &model.PageMonitor{
		CounterID:  counterID,
		Name:       p.data["name"],
		URLPattern: p.data["url_pattern"],
		Metrics:    metrics,
		Enabled:    true,
	}
	if err := ba.bot.db.CreateMonitor(context.Background(), monitor); err != nil {
		ba.bot.reply(p.chatID, "❌ Ошибка: "+err.Error())
		return true
	}

	ba.mu.Lock()
	delete(ba.pending, userIDFromPending(ba.pending, p))
	ba.mu.Unlock()

	ba.bot.reply(p.chatID, fmt.Sprintf("✅ Монитор **%s** создан (`%s`, метрики: %s)\n\n/addtrigger %d — добавить алерт на эту страницу",
		monitor.Name, monitor.URLPattern, strings.Join(metrics, ", "), counterID))
	return true
}

func (ba *BotActions) handleAddTrigger(p *pendingAction, text string) bool {
	parts := strings.Fields(text)
	if len(parts) < 4 {
		ba.bot.reply(p.chatID, "Неверный формат.\n`имя condition порог окно_мин кулдаун_мин [monitor_id]`\n\nПример: `Ошибки status_code == 500 3 15 60`")
		return true
	}

	counterID, _ := strconv.ParseInt(p.args[0], 10, 64)
	name := parts[0]
	threshold, _ := strconv.Atoi(parts[2])
	window, _ := strconv.Atoi(parts[3])
	cooldown, _ := strconv.Atoi(parts[4])
	if threshold <= 0 {
		threshold = 1
	}
	if window <= 0 {
		window = 30
	}
	if cooldown <= 0 {
		cooldown = 60
	}

	var monitorID *int64
	if len(parts) >= 6 {
		if mid, err := strconv.ParseInt(parts[5], 10, 64); err == nil && mid > 0 {
			monitorID = &mid
		}
	}

	// Rejoin parts[1..n-3] as condition (may contain spaces like "page_url contains /checkout").
	condEnd := len(parts) - 3
	if len(parts) >= 6 {
		condEnd = len(parts) - 4
	}
	condition := strings.Join(parts[1:condEnd], " ")

	trigger := &model.Trigger{
		CounterID:    counterID,
		MonitorID:    monitorID,
		Name:         name,
		Condition:    condition,
		Threshold:    threshold,
		Window:       window,
		Cooldown:     cooldown,
		Enabled:      true,
	}
	if err := ba.bot.db.CreateTrigger(context.Background(), trigger); err != nil {
		ba.bot.reply(p.chatID, "❌ Ошибка: "+err.Error())
		return true
	}

	ba.mu.Lock()
	delete(ba.pending, userIDFromPending(ba.pending, p))
	ba.mu.Unlock()

	scope := "весь счётчик"
	if monitorID != nil {
		scope = fmt.Sprintf("monitor #%d", *monitorID)
	}
	ba.bot.reply(p.chatID, fmt.Sprintf("✅ Триггер **%s** создан\n`%s` — алерт при %d+ событиях за %d мин\nОбласть: %s | Кулдаун: %d мин",
		name, condition, threshold, window, scope, cooldown))
	return true
}

func (ba *BotActions) handleAddAction(p *pendingAction, text string) bool {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		ba.bot.reply(p.chatID, "Формат:\n`telegram chat_id` — чат Telegram\n`webhook https://url` — webhook")
		return true
	}

	action := &model.AlertAction{Name: parts[0] + " " + parts[1], Type: parts[0]}
	switch parts[0] {
	case "telegram":
		if cid, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			action.ChatID = &cid
			action.Name = fmt.Sprintf("telegram-%d", cid)
		}
	case "webhook":
		action.URL = parts[1]
		action.Name = "webhook-" + parts[1][:min(len(parts[1]), 32)]
	default:
		ba.bot.reply(p.chatID, "Неизвестный тип. Используй `telegram` или `webhook`.")
		return true
	}

	if err := ba.bot.db.CreateAlertAction(context.Background(), action); err != nil {
		ba.bot.reply(p.chatID, "❌ Ошибка: "+err.Error())
		return true
	}

	ba.mu.Lock()
	delete(ba.pending, userIDFromPending(ba.pending, p))
	ba.mu.Unlock()

	ba.bot.reply(p.chatID, fmt.Sprintf("✅ Destination **%s** добавлен", action.Name))
	return true
}

func userIDFromPending(pending map[int64]*pendingAction, p *pendingAction) int64 {
	for uid, pa := range pending {
		if pa == p {
			return uid
		}
	}
	return 0
}

// ProcessMessage is the entry point: command or prompt response.
func (ba *BotActions) ProcessMessage(msg tgbotapi.Message) bool {
	if msg.IsCommand() {
		return ba.bot.Handle(msg)
	}
	if msg.Text != "" {
		return ba.Handle(msg.From.ID, msg.Text)
	}
	return false
}
