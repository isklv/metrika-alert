package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func (b *Bot) sendMenu(ctx context.Context, chatID string) {
	b.reply(ctx, chatID, `*Metrika Alert — меню*

/counters — список счётчиков
/addcounter — добавить счётчик Метрики
/deletecounter <id> — удалить счётчик

/monitors <counter_id> — страницы мониторинга
/addmonitor <counter_id> — добавить страницу
/deletemonitor <id> — удалить страницу

/triggers <counter_id> — триггеры алертов
/addtrigger <counter_id> — создать триггер
/deletetrigger <id> — удалить триггер

/actions — куда идут алерты
/addaction — добавить destination (telegram/vkteams/webhook)
/deleteaction <id> — удалить destination

/alerts [counter_id] — история алертов
/report [counter_id] — запустить отчёт сейчас
/poll — опросить все счётчики один раз
/whoami — показать свой ID
/help — это меню`)
}

// ---- Counters ----

func (b *Bot) listCounters(ctx context.Context, chatID string) {
	counters, err := b.db.ListCounters(ctx)
	if err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	if len(counters) == 0 {
		b.reply(ctx, chatID, "Счётчиков нет. Добавь первый: /addcounter")
		return
	}

	var sb strings.Builder
	sb.WriteString("*Счётчики:*\n\n")
	for _, c := range counters {
		sb.WriteString(fmt.Sprintf("• #%d *%s* (`%s`) — опрос каждые %d мин\n",
			c.ID, c.Name, c.CounterID, c.PollInterval))
	}
	b.reply(ctx, chatID, sb.String())
}

func (b *Bot) deleteCounter(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/deletecounter 1")
	if !ok {
		return
	}
	c, err := b.db.GetCounter(ctx, id)
	if err != nil {
		b.reply(ctx, chatID, "❌ Счётчик не найден: "+err.Error())
		return
	}
	if err := b.db.DeleteCounter(ctx, id); err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	b.reply(ctx, chatID, fmt.Sprintf("✅ Счётчик *%s* удалён вместе с его мониторами, триггерами и историей", c.Name))
}

// ---- Monitors ----

func (b *Bot) listMonitors(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/monitors 1")
	if !ok {
		return
	}
	c, err := b.db.GetCounter(ctx, id)
	if err != nil {
		b.reply(ctx, chatID, "❌ Счётчик не найден")
		return
	}
	monitors, err := b.db.ListMonitors(ctx, id)
	if err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Страницы мониторинга — %s:*\n\n", c.Name))
	if len(monitors) == 0 {
		sb.WriteString(fmt.Sprintf("_Нет страниц. Добавь: /addmonitor %d_", id))
	} else {
		for _, m := range monitors {
			sb.WriteString(fmt.Sprintf("• #%d %s *%s* — `%s` [%s]\n",
				m.ID, enabledMark(m.Enabled), m.Name, m.URLPattern, strings.Join(m.Metrics, ", ")))
		}
	}
	b.reply(ctx, chatID, sb.String())
}

func (b *Bot) deleteMonitor(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/deletemonitor 1")
	if !ok {
		return
	}
	if err := b.db.DeleteMonitor(ctx, id); err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	b.reply(ctx, chatID, "✅ Монитор удалён")
}

// ---- Triggers ----

func (b *Bot) listTriggers(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/triggers 1")
	if !ok {
		return
	}
	c, err := b.db.GetCounter(ctx, id)
	if err != nil {
		b.reply(ctx, chatID, "❌ Счётчик не найден")
		return
	}
	triggers, err := b.db.ListTriggers(ctx, id)
	if err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Триггеры — %s:*\n\n", c.Name))
	if len(triggers) == 0 {
		sb.WriteString(fmt.Sprintf("_Нет триггеров. Добавь: /addtrigger %d_", id))
	} else {
		for _, t := range triggers {
			scope := "весь счётчик"
			if t.MonitorID != nil {
				scope = fmt.Sprintf("monitor #%d", *t.MonitorID)
			}
			sb.WriteString(fmt.Sprintf("• #%d %s *%s* — `%s` (порог %d за %d мин, кулдаун %d мин) [%s]\n",
				t.ID, enabledMark(t.Enabled), t.Name, t.Condition, t.Threshold, t.Window, t.Cooldown, scope))
		}
	}
	b.reply(ctx, chatID, sb.String())
}

func (b *Bot) deleteTrigger(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/deletetrigger 1")
	if !ok {
		return
	}
	if err := b.db.DeleteTrigger(ctx, id); err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	b.reply(ctx, chatID, "✅ Триггер удалён")
}

// ---- Alert actions ----

func (b *Bot) listAlertActions(ctx context.Context, chatID string) {
	actions, err := b.db.ListAlertActions(ctx)
	if err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	if len(actions) == 0 {
		b.reply(ctx, chatID, "Destinations не заданы — алерты идут админам из config.yaml.\nДобавить явный адрес: /addaction")
		return
	}

	var sb strings.Builder
	sb.WriteString("*Куда идут алерты:*\n\n")
	for _, a := range actions {
		sb.WriteString(fmt.Sprintf("• #%d %s → `%s`\n", a.ID, a.Type, a.Destination()))
	}
	b.reply(ctx, chatID, sb.String())
}

func (b *Bot) deleteAlertAction(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/deleteaction 1")
	if !ok {
		return
	}
	if err := b.db.DeleteAlertAction(ctx, id); err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	b.reply(ctx, chatID, "✅ Destination удалён")
}

// ---- Alerts & jobs ----

func (b *Bot) listAlerts(ctx context.Context, chatID, args string) {
	var counterID int64
	if args != "" {
		var ok bool
		if counterID, ok = b.parseID(ctx, chatID, args, "/alerts 1"); !ok {
			return
		}
	}

	alerts, err := b.db.RecentAlerts(ctx, counterID, 20)
	if err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	if len(alerts) == 0 {
		b.reply(ctx, chatID, "Алертов пока нет.")
		return
	}

	var sb strings.Builder
	sb.WriteString("*История алертов (последние 20):*\n\n")
	for _, a := range alerts {
		sb.WriteString(fmt.Sprintf("• %s — %s (%d событий)\n",
			a.CreatedAt.Format("02.01 15:04"), a.Title, a.EventCount))
	}
	b.reply(ctx, chatID, sb.String())
}

func (b *Bot) runReportNow(ctx context.Context, chatID, args string) {
	if b.tasks == nil {
		b.reply(ctx, chatID, "❌ Отчёты недоступны: движок не подключён.")
		return
	}

	var counterID int64
	if args != "" {
		var ok bool
		if counterID, ok = b.parseID(ctx, chatID, args, "/report 1"); !ok {
			return
		}
	}

	b.reply(ctx, chatID, "🔄 Запускаю отчёт…")
	// Metrika calls take seconds; running them inline would stall the update
	// loop and every other command behind it.
	go func() {
		// The chat context ends with this handler, so the job gets its own.
		jobCtx := context.Background()
		if err := b.tasks.RunReport(jobCtx, counterID); err != nil {
			b.reply(jobCtx, chatID, "❌ Отчёт не собран: "+err.Error())
			return
		}
		b.reply(jobCtx, chatID, "✅ Отчёт готов — отправлен в настроенные destinations.")
	}()
}

func (b *Bot) runPollOnce(ctx context.Context, chatID string) {
	if b.tasks == nil {
		b.reply(ctx, chatID, "❌ Опрос недоступен: движок не подключён.")
		return
	}

	b.reply(ctx, chatID, "🔄 Двигаю опрос по всем счётчикам…")
	go func() {
		jobCtx := context.Background()
		if err := b.tasks.PollOnce(jobCtx); err != nil {
			b.reply(jobCtx, chatID, "❌ Опрос не выполнен: "+err.Error())
			return
		}
		// Logs API is asynchronous: a tick usually orders an export that
		// Metrika prepares over the next few minutes.
		b.reply(jobCtx, chatID, "✅ Готово. Logs API работает асинхронно: "+
			"эта команда либо заказывает выгрузку, либо забирает уже готовую. "+
			"Сработавшие триггеры придут отдельными алертами.")
	}()
}

// ---- helpers ----

// parseID reads a positive row ID from command arguments, replying with usage
// when it is missing or malformed.
func (b *Bot) parseID(ctx context.Context, chatID, args, usage string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(ctx, chatID, "Укажи числовой ID, например: "+usage)
		return 0, false
	}
	return id, true
}

func (b *Bot) replyErr(ctx context.Context, chatID string, err error) {
	b.reply(ctx, chatID, "❌ Ошибка: "+err.Error())
}

func enabledMark(enabled bool) string {
	if enabled {
		return "✅"
	}
	return "⛔"
}
