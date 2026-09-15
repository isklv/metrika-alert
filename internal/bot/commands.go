package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"time"

	"github.com/isklv/metrika-alert/internal/engine"
)

func (b *Bot) sendMenu(ctx context.Context, chatID string) {
	b.reply(ctx, chatID, `*Metrika Alert — меню*

/counters — список счётчиков
/addcounter — добавить счётчик Метрики
/deletecounter <id> — удалить счётчик

/goals <counter_id> — цели счётчика и их ID
/triggers <counter_id> — правила алертов
/addtrigger <counter_id> — создать триггер
/deletetrigger <id> — удалить триггер

/actions — куда идут алерты
/addaction — добавить destination (telegram/vkteams/webhook)
/deleteaction <id> — удалить destination

/alerts [counter_id] — история алертов
/reports <counter_id> — какие отчёты настроены
/addreport <counter_id> — отчёт по страницам и целям
/deletereport <id> — удалить отчёт
/report [counter_id] — запустить отчёты сейчас
/schedule [расписание] — когда присылать отчёты
/poll — опросить все счётчики один раз
/whoami — показать свой ID
/version — версия запущенной сборки
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
	b.reply(ctx, chatID, fmt.Sprintf("✅ Счётчик *%s* удалён вместе с его правилами и историей", c.Name))
}

// showVersion reports the running build. A deployment that silently kept an old
// image looks exactly like a feature that was never added; this tells them apart.
func (b *Bot) showVersion(ctx context.Context, chatID string) {
	v := b.version
	if v == "" {
		v = "неизвестна"
	}
	b.reply(ctx, chatID, "*Сборка:* `"+v+"`\n\nЕсли команды из документации нет в /help — запущен старый бинарь.")
}

// reportSchedule shows or changes when periodic reports are sent.
func (b *Bot) reportSchedule(ctx context.Context, chatID, args string) {
	if b.scheduler == nil {
		b.reply(ctx, chatID, "❌ Расписание недоступно: движок не подключён.")
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		b.showReportSchedule(ctx, chatID)
		return
	}

	schedule, err := engine.ParseSchedule(args)
	if err != nil {
		b.reply(ctx, chatID, "❌ "+err.Error()+"\n\n"+reportScheduleHelp)
		return
	}
	if err := b.scheduler.SetSchedule(ctx, schedule); err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}

	reply := "✅ Отчёты: *" + schedule.Describe() + "*"
	if next, ok := b.scheduler.NextRun(ctx); ok {
		reply += "\nБлижайший — " + formatNextRun(next)
	}
	reply += "\n\nПрименится в течение минуты, перезапуск не нужен."
	b.reply(ctx, chatID, reply)
}

func (b *Bot) showReportSchedule(ctx context.Context, chatID string) {
	schedule := b.scheduler.Schedule(ctx)

	reply := "*Отчёты:* " + schedule.Describe()
	if next, ok := b.scheduler.NextRun(ctx); ok {
		reply += "\nБлижайший — " + formatNextRun(next)
	}
	b.reply(ctx, chatID, reply+"\n\n"+reportScheduleHelp)
}

const reportScheduleHelp = "Изменить:\n" +
	"`/schedule 10:00` — каждый день в 10:00\n" +
	"`/schedule 6h` — каждые 6 часов\n" +
	"`/schedule off` — не присылать\n\n" +
	"Время — в часовом поясе сервиса."

// formatNextRun renders when the next report lands, with how long that is away.
func formatNextRun(next time.Time) string {
	wait := time.Until(next).Round(time.Minute)
	if wait < 0 {
		return "сейчас"
	}
	return fmt.Sprintf("%s (через %s)", next.Format("02.01 15:04"), formatWait(wait))
}

func formatWait(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours == 0 {
		return fmt.Sprintf("%d мин", minutes)
	}
	return fmt.Sprintf("%d ч %d мин", hours, minutes)
}

// ---- Reports ----

func (b *Bot) listReports(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/reports 1")
	if !ok {
		return
	}
	counter, err := b.db.GetCounter(ctx, id)
	if err != nil {
		b.reply(ctx, chatID, "❌ Счётчик не найден. Список: /counters")
		return
	}

	reports, err := b.db.ListReports(ctx, id)
	if err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Отчёты — %s:*\n\n", counter.Name)
	if len(reports) == 0 {
		sb.WriteString("_Отдельных отчётов нет — присылается сводка по всему счётчику._\n\n" +
			"Сузить до страниц и целей: /addreport " + strconv.FormatInt(id, 10))
	} else {
		for _, r := range reports {
			fmt.Fprintf(&sb, "• #%d %s *%s*\n    %s\n    %s\n",
				r.ID, enabledMark(r.Enabled), r.Name,
				engine.URLFilterLabel(r.URLFilter, r.URLMatch), describeGoals(r.GoalIDs))
		}
	}
	b.reply(ctx, chatID, sb.String())
}

// describeGoals renders which goals a report covers.
func describeGoals(ids []int64) string {
	if len(ids) == 0 {
		return "все цели"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "goal:"+strconv.FormatInt(id, 10))
	}
	return "цели: " + strings.Join(parts, ", ")
}

func (b *Bot) deleteReport(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/deletereport 1")
	if !ok {
		return
	}
	if err := b.db.DeleteReport(ctx, id); err != nil {
		b.replyErr(ctx, chatID, err)
		return
	}
	b.reply(ctx, chatID, "✅ Отчёт удалён")
}

// ---- Goals ----

// listGoals answers the question a rule cannot: which goal IDs exist. Writing
// `goal:42` requires knowing that 42 is the order confirmation, and that number
// lives only in Metrika.
func (b *Bot) listGoals(ctx context.Context, chatID, args string) {
	id, ok := b.parseID(ctx, chatID, args, "/goals 1")
	if !ok {
		return
	}
	counter, err := b.db.GetCounter(ctx, id)
	if err != nil {
		b.reply(ctx, chatID, "❌ Счётчик не найден. Список: /counters")
		return
	}

	if b.metrika == nil {
		b.reply(ctx, chatID, "❌ Справочник целей недоступен: движок не подключён.")
		return
	}

	goals, err := b.metrika.Goals(ctx, counter)
	if err != nil {
		// Only say "check your token" when the API actually refused on
		// credentials; for anything else that advice sends people to fix
		// something that was never wrong.
		hint := "\n\nАлерты по `goals` и по конкретной цели работают независимо — " +
			"ID цели можно взять в интерфейсе Метрики."
		if engine.IsAccessDenied(err) {
			hint = "\n\nOAuth-токен счётчика не даёт доступа к его настройкам." + hint
		}
		b.reply(ctx, chatID, "❌ Не удалось получить цели: "+err.Error()+hint)
		return
	}
	if len(goals) == 0 {
		b.reply(ctx, chatID, "У счётчика *"+counter.Name+"* нет настроенных целей.")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "*Цели — %s:*\n\n", counter.Name)
	for _, g := range goals {
		mark := ""
		if g.Favorite() {
			mark = "⭐ "
		}
		if !g.Active() {
			mark = "⛔ "
		}
		fmt.Fprintf(&sb, "• %s*%s* — `goal:%d`\n    %s\n", mark, g.Name, g.ID, g.TypeLabel())
	}
	sb.WriteString("\nПравило по цели:\n`Заказы просели | goal:" +
		strconv.FormatInt(goals[0].ID, 10) + " | drop | 50`")

	b.reply(ctx, chatID, sb.String())
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
			sb.WriteString(fmt.Sprintf("• #%d %s *%s*\n    %s %s на %d%%+ от обычного для этого времени\n    область: %s\n    база %d нед., мин. %d, кулдаун %d мин\n",
				t.ID, enabledMark(t.Enabled), t.Name,
				engine.MetricLabel(t.Metric), directionLabel(t.Direction), t.DeviationPct,
				engine.URLFilterLabel(t.URLFilter, t.URLMatch),
				t.BaselineWeeks, t.MinBaseline, t.Cooldown))
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

// directionLabel renders which way a trigger watches.
func directionLabel(direction string) string {
	switch direction {
	case engine.DirectionDrop:
		return "падают"
	case engine.DirectionRise:
		return "растут"
	default:
		return "меняются"
	}
}

func enabledMark(enabled bool) string {
	if enabled {
		return "✅"
	}
	return "⛔"
}
