package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

// Interactive flows: a command asks for data, the user's next plain message
// carries it. State lives per user so two admins can configure in parallel.
// Defaults for a rule created through the bot.
const (
	defaultMinBaseline     = 10
	defaultBaselineWeeks   = 4
	maxBaselineWeeks       = 12
	defaultCooldownMinutes = 180
)

const (
	actionAddCounter = "addcounter"
	actionAddTrigger = "addtrigger"
	actionAddAction  = "addaction"
	actionAddReport  = "addreport"
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
	case actionAddTrigger:
		done = b.saveTrigger(ctx, p, text)
	case actionAddAction:
		done = b.saveAlertAction(ctx, p, text)
	case actionAddReport:
		done = b.saveReport(ctx, p, text)
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
		"✅ Счётчик *%s* добавлен (`%s`, проверка каждые %d мин)\nПроверки начнутся в течение минуты, перезапуск не нужен.\n\nДалее: /addtrigger %d — создать правило алерта",
		counter.Name, counter.CounterID, interval, counter.ID))
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
	b.reply(ctx, msg.ChatID, `Пришли правило одним сообщением:

`+"`имя | метрика | направление | порог% [мин_база] [недель] [url=...]`"+`

Метрики: `+"`visits`"+`, `+"`users`"+`, `+"`pageviews`"+`, `+"`goals`"+`, `+"`goal:42`"+`
Направление: `+"`drop`"+` (падение), `+"`rise`"+` (рост), `+"`both`"+`

Примеры:
`+"`Визиты упали | visits | drop | 40`"+`
`+"`Чекаут просел | visits | drop | 40 | url=/checkout`"+`
`+"`Заказы просели | goal:42 | drop | 50 | 5 | 4`"+`
`+"`Каталог | visits | drop | 35 | url~^/catalog/\\d+`"+`

`+"`url=`"+` — URL содержит подстроку, `+"`url~`"+` — регулярное выражение.
Считаются сессии, в которых была хотя бы одна такая страница.

Сравнение идёт с тем же временем того же дня недели за прошлые недели.
`+"`мин_база`"+` — ниже какого обычного значения не тревожить (по умолчанию 10),
`+"`недель`"+` — сколько недель истории брать (по умолчанию 4).`)
}

// saveTrigger parses the pipe-delimited rule. The separator matters because the
// name may contain spaces and "goal:42" may not be split on anything else.
func (b *Bot) saveTrigger(ctx context.Context, p *pendingAction, text string) bool {
	fields := strings.Split(text, "|")
	if len(fields) < 4 || len(fields) > 7 {
		b.reply(ctx, p.chatID, "Неверный формат. Нужно от четырёх до семи частей через `|`:\n`имя | метрика | направление | порог% [мин_база] [недель] [url=...]`")
		return false
	}
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}

	// The URL scope is labelled rather than positional, so it can follow the
	// threshold directly without forcing the optional numbers to be typed.
	urlFilter, urlMatch, rest, err := takeURLScope(fields[4:])
	if err != nil {
		b.reply(ctx, p.chatID, "❌ "+err.Error())
		return false
	}
	optional := rest

	name := fields[0]
	if name == "" {
		b.reply(ctx, p.chatID, "Имя правила не должно быть пустым.")
		return false
	}

	metric := strings.ToLower(fields[1])
	if _, err := engine.ResolveMetric(metric); err != nil {
		b.reply(ctx, p.chatID, "❌ "+err.Error())
		return false
	}

	direction := strings.ToLower(fields[2])
	if !engine.ValidDirection(direction) {
		b.reply(ctx, p.chatID, "Направление должно быть `drop`, `rise` или `both`.")
		return false
	}

	deviation, convErr := strconv.Atoi(strings.TrimSuffix(fields[3], "%"))
	if convErr != nil || deviation <= 0 || deviation > 100 {
		b.reply(ctx, p.chatID, "Порог — целое число процентов от 1 до 100, например `40`.")
		return false
	}

	minBaseline := defaultMinBaseline
	if len(optional) >= 1 && optional[0] != "" {
		minBaseline, convErr = strconv.Atoi(optional[0])
		if convErr != nil || minBaseline < 0 {
			b.reply(ctx, p.chatID, "`мин_база` — неотрицательное целое число.")
			return false
		}
	}

	weeks := defaultBaselineWeeks
	if len(optional) >= 2 && optional[1] != "" {
		weeks, convErr = strconv.Atoi(optional[1])
		if convErr != nil || weeks < 1 || weeks > maxBaselineWeeks {
			b.reply(ctx, p.chatID, fmt.Sprintf("`недель` — целое число от 1 до %d.", maxBaselineWeeks))
			return false
		}
	}
	if len(optional) > 2 {
		b.reply(ctx, p.chatID, "Лишние поля после `недель`. Фильтр по URL пишется как `url=/checkout` или `url~^/catalog/`.")
		return false
	}

	trigger := &model.Trigger{
		CounterID:     p.counterID,
		Name:          name,
		Metric:        metric,
		Direction:     direction,
		DeviationPct:  deviation,
		MinBaseline:   minBaseline,
		BaselineWeeks: weeks,
		URLFilter:     urlFilter,
		URLMatch:      urlMatch,
		Cooldown:      defaultCooldownMinutes,
		Enabled:       true,
	}
	if err := b.db.CreateTrigger(ctx, trigger); err != nil {
		b.replyErr(ctx, p.chatID, err)
		return false
	}

	b.reply(ctx, p.chatID, fmt.Sprintf(
		"✅ Правило #%d *%s* создано\n%s %s на %d%%+ от обычного для этого времени\nОбласть: %s\nБаза: медиана %d недель, не тревожить ниже %d, кулдаун %d мин",
		trigger.ID, name, engine.MetricLabel(metric), directionLabel(direction),
		deviation, engine.URLFilterLabel(urlFilter, urlMatch), weeks, minBaseline, defaultCooldownMinutes))
	return true
}

// takeURLScope pulls a labelled url= or url~ field out of the optional tail and
// returns it along with the fields that remain positional.
func takeURLScope(optional []string) (filter, match string, rest []string, err error) {
	for _, field := range optional {
		value, isRegexp, ok := parseURLScope(field)
		if !ok {
			rest = append(rest, field)
			continue
		}
		if filter != "" {
			return "", "", nil, fmt.Errorf("фильтр по URL указан дважды")
		}
		if value == "" {
			return "", "", nil, fmt.Errorf("после `url=` нужен непустой шаблон")
		}
		filter = value
		match = engine.URLMatchContains
		if isRegexp {
			match = engine.URLMatchRegexp
		}
	}

	if filter != "" {
		// Reject a bad pattern here, where the message can name the field,
		// rather than at the first check hours later.
		if _, err := engine.URLFilter(filter, match); err != nil {
			return "", "", nil, err
		}
	}
	return filter, match, rest, nil
}

// parseURLScope recognises "url=<substring>" and "url~<regexp>".
func parseURLScope(field string) (value string, isRegexp, ok bool) {
	if v, found := strings.CutPrefix(field, "url~"); found {
		return strings.TrimSpace(v), true, true
	}
	if v, found := strings.CutPrefix(field, "url="); found {
		return strings.TrimSpace(v), false, true
	}
	return "", false, false
}

// ---- add report ----

func (b *Bot) promptAddReport(ctx context.Context, msg Message, args string) {
	counterID, ok := b.parseID(ctx, msg.ChatID, args, "/addreport 1")
	if !ok {
		return
	}
	if _, err := b.db.GetCounter(ctx, counterID); err != nil {
		b.reply(ctx, msg.ChatID, "❌ Счётчик не найден. Список: /counters")
		return
	}

	b.setPending(msg.UserID, &pendingAction{kind: actionAddReport, chatID: msg.ChatID, counterID: counterID})
	b.reply(ctx, msg.ChatID, `Пришли описание отчёта одним сообщением:

`+"`имя [| url=...] [| goals=42,77] [| group=url]`"+`

Примеры:
`+"`Чекаут | url=/checkout`"+`
`+"`Заказы | goals=42,77`"+`
`+"`Чекаут и заказы | url=/checkout | goals=42`"+`
`+"`Каталог | url~^/catalog/`"+`
`+"`Топ страниц | group=url`"+`

`+"`url=`"+` — URL содержит подстроку, `+"`url~`"+` — регулярное выражение.
Считаются сессии, в которых была хотя бы одна такая страница.
`+"`goals=`"+` — какие цели показывать; без него показываются все.
`+"`group=url`"+` — сгруппировать по страницам входа (топ URL).
ID целей: /goals `+strconv.FormatInt(counterID, 10)+`

Как только появится хотя бы один отчёт, сводка по всему счётчику присылаться
перестанет — заведите её отдельным отчётом без фильтров, если она нужна.`)
}

func (b *Bot) saveReport(ctx context.Context, p *pendingAction, text string) bool {
	fields := strings.Split(text, "|")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}

	name := fields[0]
	if name == "" {
		b.reply(ctx, p.chatID, "Имя отчёта не должно быть пустым.")
		return false
	}

	urlFilter, urlMatch, rest, err := takeURLScope(fields[1:])
	if err != nil {
		b.reply(ctx, p.chatID, "❌ "+err.Error())
		return false
	}

	var goalIDs []int64
	var groupBy string
	for _, field := range rest {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if value, found := strings.CutPrefix(field, "goals="); found {
			goalIDs, err = parseGoalList(value)
			if err != nil {
				b.reply(ctx, p.chatID, "❌ "+err.Error())
				return false
			}
			continue
		}
		if value, found := strings.CutPrefix(field, "group="); found {
			val := strings.ToLower(strings.TrimSpace(value))
			if val != "url" {
				b.reply(ctx, p.chatID, "❌ Неизвестная группировка `"+val+"`. Доступно: `group=url`.")
				return false
			}
			groupBy = "url"
			continue
		}
		if value, found := strings.CutPrefix(field, "group_by="); found {
			val := strings.ToLower(strings.TrimSpace(value))
			if val != "url" {
				b.reply(ctx, p.chatID, "❌ Неизвестная группировка `"+val+"`. Доступно: `group=url`.")
				return false
			}
			groupBy = "url"
			continue
		}
		if value, found := strings.CutPrefix(field, "by="); found {
			val := strings.ToLower(strings.TrimSpace(value))
			if val != "url" {
				b.reply(ctx, p.chatID, "❌ Неизвестная группировка `"+val+"`. Доступно: `group=url`.")
				return false
			}
			groupBy = "url"
			continue
		}
		b.reply(ctx, p.chatID, "Не понял часть `"+field+"`. Доступны `url=`, `url~`, `goals=` и `group=url`.")
		return false
	}

	report := &model.Report{
		CounterID: p.counterID,
		Name:      name,
		URLFilter: urlFilter,
		URLMatch:  urlMatch,
		GoalIDs:   goalIDs,
		GroupBy:   groupBy,
		Enabled:   true,
	}
	if err := b.db.CreateReport(ctx, report); err != nil {
		b.replyErr(ctx, p.chatID, err)
		return false
	}

	scope := engine.URLFilterLabel(urlFilter, urlMatch)
	if groupBy == "url" {
		scope += ", группировка по URL"
	}
	b.reply(ctx, p.chatID, fmt.Sprintf(
		"✅ Отчёт #%d *%s* создан\nОбласть: %s\n%s\n\nРасписание — /schedule",
		report.ID, name, scope, describeGoals(goalIDs)))
	return true
}

// parseGoalList reads "42,77" into goal IDs.
func parseGoalList(s string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "goal:"))
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("«%s» не похоже на ID цели — нужны числа через запятую, например `goals=42,77`", part)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("после `goals=` нужен хотя бы один ID цели")
	}
	return ids, nil
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
