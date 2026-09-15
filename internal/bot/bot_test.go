package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isklv/metrika-alert/internal/engine"
	"github.com/isklv/metrika-alert/internal/model"
)

// fakeTransport records what the bot sends instead of talking to a platform.
type fakeTransport struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeTransport) Name() string { return "vkteams" }

// MaxUnits mirrors Telegram's ceiling, the tighter of the two platforms.
func (f *fakeTransport) MaxUnits() int { return telegramMaxUnits }

func (f *fakeTransport) Measure(text string) int { return utf16Len(text) }

func (f *fakeTransport) Send(_ context.Context, chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeTransport) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

// fakeMetrika stands in for the counter-configuration lookup.
type fakeMetrika struct {
	goals []engine.Goal
	err   error
}

func (f *fakeMetrika) Goals(context.Context, *model.Counter) ([]engine.Goal, error) {
	return f.goals, f.err
}

func newTestBot(t *testing.T, admins ...string) (*Bot, *fakeTransport, *model.DB) {
	t.Helper()
	db, err := model.OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tr := &fakeTransport{}
	return New(tr, db, nil, &fakeMetrika{}, admins), tr, db
}

const admin = "admin@corp.ru"

func say(t *testing.T, b *Bot, text string) {
	t.Helper()
	b.Handle(context.Background(), Message{UserID: admin, ChatID: "chat1", Text: text})
}

// The interactive flows used to prompt and then discard the answer, because
// recording the pending state was a no-op. Every add-command must persist.
func TestAddCounterFlowPersists(t *testing.T) {
	b, tr, db := newTestBot(t, admin)

	say(t, b, "/addcounter")
	if !strings.Contains(tr.last(), "counterID") {
		t.Fatalf("expected a prompt, got %q", tr.last())
	}

	say(t, b, "Магазин 12345678 y0_token 15")

	counters, err := db.ListCounters(context.Background())
	if err != nil {
		t.Fatalf("ListCounters: %v", err)
	}
	if len(counters) != 1 {
		t.Fatalf("got %d counters, want 1 — the answer to the prompt was dropped", len(counters))
	}
	got := counters[0]
	if got.Name != "Магазин" || got.CounterID != "12345678" || got.OAuthToken != "y0_token" {
		t.Errorf("stored counter = %+v", got)
	}
	if got.PollInterval != 15 {
		t.Errorf("poll interval = %d, want 15", got.PollInterval)
	}
}

func TestAddTriggerFlowPersists(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/addtrigger 1")
	say(t, b, "Визиты упали | visits | drop | 40")

	triggers, err := db.ListTriggers(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("got %d triggers, want 1", len(triggers))
	}

	got := triggers[0]
	// The name contains a space, so the pipe form is what keeps it apart from
	// the metric. A positional parse would have mangled this.
	if got.Name != "Визиты упали" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Metric != "visits" || got.Direction != "drop" || got.DeviationPct != 40 {
		t.Errorf("rule = %+v", got)
	}
	if got.BaselineWeeks != 4 || got.MinBaseline != 10 {
		t.Errorf("defaults not applied: %+v", got)
	}
}

// A four-field answer used to index parts[4] and panic the update loop.
func TestAddTriggerRejectsMalformedInputWithoutPanicking(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")
	say(t, b, "/addtrigger 1")

	for _, bad := range []string{
		"Визиты visits drop 40",
		"Визиты | visits | drop",
		"Визиты | нетакой | drop | 40",
		"Визиты | visits | вбок | 40",
		"Визиты | visits | drop | сорок",
		"Визиты | visits | drop | 0",
		"Визиты | visits | drop | 140",
		"| visits | drop | 40",
		"Заказы | goal:abc | drop | 40",
	} {
		say(t, b, bad)
		last := tr.last()
		if !strings.ContainsAny(last, "❌") && !strings.Contains(last, "формат") &&
			!strings.Contains(last, "Направление") && !strings.Contains(last, "Порог") &&
			!strings.Contains(last, "пуст") {
			t.Errorf("input %q: expected a validation message, got %q", bad, last)
		}
	}

	triggers, _ := db.ListTriggers(context.Background(), 1)
	if len(triggers) != 0 {
		t.Fatalf("malformed input created %d trigger(s)", len(triggers))
	}

	// The flow stays open, so a correct retry still works.
	say(t, b, "Визиты | visits | drop | 40")
	if triggers, _ = db.ListTriggers(context.Background(), 1); len(triggers) != 1 {
		t.Fatal("retry after a malformed answer did not create the trigger")
	}
}

func TestAddActionSupportsVKTeamsTarget(t *testing.T) {
	b, _, db := newTestBot(t, admin)

	say(t, b, "/addaction")
	say(t, b, "vkteams team-chat@corp.ru")

	actions, err := db.ListAlertActions(context.Background())
	if err != nil {
		t.Fatalf("ListAlertActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	if actions[0].Type != "vkteams" {
		t.Errorf("type = %q, want vkteams", actions[0].Type)
	}
	if actions[0].Target != "team-chat@corp.ru" {
		t.Errorf("target = %q", actions[0].Target)
	}
}

func TestAddActionRejectsUnknownType(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	say(t, b, "/addaction")
	say(t, b, "slack #alerts")

	if !strings.Contains(tr.last(), "Неизвестный тип") {
		t.Errorf("got %q", tr.last())
	}
	if actions, _ := db.ListAlertActions(context.Background()); len(actions) != 0 {
		t.Error("an unsupported destination type was stored")
	}
}

// An empty admin list must lock the bot down: counters hold Metrika OAuth
// tokens, so the bot token alone is not an authorisation boundary.
func TestNoAdminsConfiguredDeniesEveryone(t *testing.T) {
	b, tr, db := newTestBot(t) // no admins

	say(t, b, "/addcounter")
	if !strings.Contains(tr.last(), "Доступ запрещён") {
		t.Errorf("expected a denial, got %q", tr.last())
	}
	say(t, b, "Магазин 12345678 y0_token")
	if counters, _ := db.ListCounters(context.Background()); len(counters) != 0 {
		t.Fatal("a non-admin created a counter")
	}
}

func TestNonAdminIsDeniedAndToldItsID(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	b.Handle(context.Background(), Message{UserID: "intruder@corp.ru", ChatID: "c", Text: "/counters"})

	if !strings.Contains(tr.last(), "intruder@corp.ru") {
		t.Errorf("denial should show the caller's own ID, got %q", tr.last())
	}
}

// A command must abandon a half-finished flow, or a user who mistypes stays
// stuck answering prompts for something they walked away from.
func TestCommandCancelsPendingFlow(t *testing.T) {
	b, tr, db := newTestBot(t, admin)

	say(t, b, "/addcounter")
	say(t, b, "/counters")
	say(t, b, "Магазин 12345678 y0_token")

	if counters, _ := db.ListCounters(context.Background()); len(counters) != 0 {
		t.Fatal("text after a cancelling command was still consumed by the flow")
	}
	_ = tr.last()
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in      string
		command string
		args    string
	}{
		{"/counters", "counters", ""},
		{"/addtrigger 3", "addtrigger", "3"},
		{"/AddTrigger 3", "addtrigger", "3"},
		{"/report@metrika_alert_bot 2", "report", "2"},
		{"/alerts   7  ", "alerts", "7"},
		{"не команда", "", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		command, args := parseCommand(tc.in)
		if command != tc.command || args != tc.args {
			t.Errorf("parseCommand(%q) = (%q, %q), want (%q, %q)", tc.in, command, args, tc.command, tc.args)
		}
	}
}

// In a group chat the bot sees every message. Replying to ordinary conversation
// with an access denial would make it unusable there, so non-commands from
// people with no open prompt are ignored silently.
func TestPlainChatterFromNonAdminIsIgnored(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	handled := b.Handle(context.Background(), Message{
		UserID: "colleague@corp.ru", ChatID: "team-chat", Text: "обсуждаем релиз",
	})

	if handled {
		t.Error("ordinary group chatter was treated as handled")
	}
	if tr.last() != "" {
		t.Errorf("bot answered ordinary chatter with %q", tr.last())
	}
}

// A command from a non-admin still gets an explicit refusal, so someone who
// actually tried to use the bot learns why it did not work.
func TestCommandFromNonAdminIsRefused(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	if !b.Handle(context.Background(), Message{UserID: "colleague@corp.ru", ChatID: "c", Text: "/counters"}) {
		t.Error("a command should be handled even when refused")
	}
	if !strings.Contains(tr.last(), "Доступ запрещён") {
		t.Errorf("got %q", tr.last())
	}
}

// ---- URL scoping in the rule syntax ----

func TestAddTriggerWithURLScope(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/addtrigger 1")
	say(t, b, "Чекаут просел | visits | drop | 40 | url=/checkout")

	triggers, err := db.ListTriggers(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("got %d triggers, want 1", len(triggers))
	}

	got := triggers[0]
	if got.URLFilter != "/checkout" {
		t.Errorf("url_filter = %q", got.URLFilter)
	}
	if got.URLMatch != "contains" {
		t.Errorf("url_match = %q, want contains", got.URLMatch)
	}
	// The labelled field must not be mistaken for the positional numbers.
	if got.MinBaseline != 10 || got.BaselineWeeks != 4 {
		t.Errorf("defaults were consumed by the url field: %+v", got)
	}
}

func TestAddTriggerWithRegexpScope(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/addtrigger 1")
	say(t, b, `Каталог | visits | drop | 35 | url~^/catalog/\d+`)

	triggers, _ := db.ListTriggers(context.Background(), 1)
	if len(triggers) != 1 {
		t.Fatalf("got %d triggers", len(triggers))
	}
	if triggers[0].URLMatch != "regexp" {
		t.Errorf("url_match = %q, want regexp", triggers[0].URLMatch)
	}
	if triggers[0].URLFilter != `^/catalog/\d+` {
		t.Errorf("url_filter = %q", triggers[0].URLFilter)
	}
}

// The scope may follow the optional numbers as well as replace them.
func TestAddTriggerWithScopeAfterOptionalNumbers(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/addtrigger 1")
	say(t, b, "Чекаут | goal:42 | drop | 50 | 5 | 6 | url=/checkout")

	triggers, _ := db.ListTriggers(context.Background(), 1)
	if len(triggers) != 1 {
		t.Fatalf("got %d triggers", len(triggers))
	}
	got := triggers[0]
	if got.MinBaseline != 5 || got.BaselineWeeks != 6 {
		t.Errorf("positional numbers were misread: %+v", got)
	}
	if got.URLFilter != "/checkout" {
		t.Errorf("url_filter = %q", got.URLFilter)
	}
}

// Rules written before URL scoping existed must keep working unchanged.
func TestAddTriggerWithoutScopeStillWorks(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/addtrigger 1")
	say(t, b, "Визиты | visits | drop | 40 | 20 | 8")

	triggers, _ := db.ListTriggers(context.Background(), 1)
	if len(triggers) != 1 {
		t.Fatalf("got %d triggers", len(triggers))
	}
	got := triggers[0]
	if got.URLFilter != "" || got.URLMatch != "" {
		t.Errorf("an unscoped rule gained a scope: %+v", got)
	}
	if got.MinBaseline != 20 || got.BaselineWeeks != 8 {
		t.Errorf("rule = %+v", got)
	}
}

func TestAddTriggerRejectsBadScope(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")
	say(t, b, "/addtrigger 1")

	for _, bad := range []string{
		"Каталог | visits | drop | 40 | url~^/catalog/[", // uncompilable regexp
		"Каталог | visits | drop | 40 | url=",            // empty pattern
		"Каталог | visits | drop | 40 | url=/a | url=/b", // two scopes
	} {
		say(t, b, bad)
		if last := tr.last(); !strings.Contains(last, "❌") && !strings.Contains(last, "url") {
			t.Errorf("input %q: expected a validation message, got %q", bad, last)
		}
	}

	if triggers, _ := db.ListTriggers(context.Background(), 1); len(triggers) != 0 {
		t.Fatalf("a malformed scope created %d rule(s)", len(triggers))
	}
}

// ---- Goal discovery ----

// Writing `goal:42` means knowing that 42 is the order confirmation, and that
// number lives only in Metrika — so the bot has to be able to show it.
func TestListGoalsShowsIDsAndUsage(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	b.metrika = &fakeMetrika{goals: []engine.Goal{
		{ID: 42, Name: "Покупка", Type: "action", IsFavorite: true},
		{ID: 77, Name: "Регистрация", Type: "url"},
	}}
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/goals 1")

	got := tr.last()
	for _, want := range []string{"Покупка", "goal:42", "Регистрация", "goal:77"} {
		if !strings.Contains(got, want) {
			t.Errorf("goal list is missing %q:\n%s", want, got)
		}
	}
	// Types are rendered for a person, not as API identifiers.
	if !strings.Contains(got, "JS-событие") || !strings.Contains(got, "посещение страницы") {
		t.Errorf("goal types were not translated:\n%s", got)
	}
	// A ready-to-paste rule removes the last step of guesswork.
	if !strings.Contains(got, "| goal:42 | drop |") {
		t.Errorf("no example rule to copy:\n%s", got)
	}
}

// The usual failure is a token without access to the counter's settings. That
// must not read as "the feature is broken".
func TestListGoalsExplainsAccessFailure(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	// The real client returns a wrapped *APIError, which is what the reply
	// classifies on — a bare string would not be recognised as a refusal.
	b.metrika = &fakeMetrika{err: fmt.Errorf("list goals: %w",
		&engine.APIError{StatusCode: 403, Message: "Access denied"})}
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/goals 1")

	got := tr.last()
	if !strings.Contains(got, "OAuth-токен") {
		t.Errorf("the reply does not point at the likely cause:\n%s", got)
	}
	// Goal alerts do not need management access; say so.
	if !strings.Contains(got, "работают независимо") {
		t.Errorf("the reply does not say alerts still work:\n%s", got)
	}
}

func TestListGoalsWithNoneConfigured(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	b.metrika = &fakeMetrika{}
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	say(t, b, "/goals 1")

	if got := tr.last(); !strings.Contains(got, "нет настроенных целей") {
		t.Errorf("got %q", got)
	}
}

func TestListGoalsNeedsAKnownCounter(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	say(t, b, "/goals 99")
	if got := tr.last(); !strings.Contains(got, "не найден") {
		t.Errorf("got %q", got)
	}

	say(t, b, "/goals")
	if got := tr.last(); !strings.Contains(got, "ID") {
		t.Errorf("got %q", got)
	}
}

// The question that needed guessing twice: which build is actually running.
func TestVersionCommand(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	b.SetVersion("43759de от 14.09.2026 21:42")

	say(t, b, "/version")

	got := tr.last()
	if !strings.Contains(got, "43759de") {
		t.Errorf("reply does not name the build:\n%s", got)
	}
	// The reply should point at the usual cause of a missing command.
	if !strings.Contains(got, "старый бинарь") {
		t.Errorf("reply does not explain a stale deployment:\n%s", got)
	}
}

func TestVersionCommandWithoutStamp(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	say(t, b, "/version")

	if got := tr.last(); !strings.Contains(got, "неизвестна") {
		t.Errorf("got %q", got)
	}
}

// Every command the menu advertises must actually be dispatched — the menu is
// how people discover them, and a gap here is exactly what looks like a bug.
func TestMenuCommandsAreAllDispatched(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	b.metrika = &fakeMetrika{}

	say(t, b, "/help")
	menu := tr.last()

	checked := 0
	for _, line := range strings.Split(menu, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		command := strings.TrimPrefix(strings.Fields(line)[0], "/")

		say(t, b, "/"+command+" 1")
		if got := tr.last(); strings.Contains(got, "Неизвестная команда") {
			t.Errorf("/%s is offered in the menu but not dispatched", command)
		}
		checked++
	}

	// Guard against the test passing because it parsed nothing.
	if checked < 10 {
		t.Fatalf("only %d menu commands were checked; the menu was not parsed", checked)
	}
	if !strings.Contains(menu, "/goals") {
		t.Error("/goals is missing from the menu")
	}
}

// Blaming the OAuth token for a decode failure sends people to fix something
// that was never wrong — the message has to match the actual cause.
func TestGoalsErrorBlamesTheTokenOnlyWhenRefused(t *testing.T) {
	t.Run("access denied names the token", func(t *testing.T) {
		b, tr, _ := newTestBot(t, admin)
		b.metrika = &fakeMetrika{err: &engine.APIError{StatusCode: 403, Message: "Access denied"}}
		say(t, b, "/addcounter")
		say(t, b, "Магазин 12345678 y0_token")

		say(t, b, "/goals 1")
		if got := tr.last(); !strings.Contains(got, "OAuth-токен") {
			t.Errorf("a refused request should name the token:\n%s", got)
		}
	})

	t.Run("other failures do not", func(t *testing.T) {
		b, tr, _ := newTestBot(t, admin)
		b.metrika = &fakeMetrika{err: fmt.Errorf("decode response: json: cannot unmarshal number into bool")}
		say(t, b, "/addcounter")
		say(t, b, "Магазин 12345678 y0_token")

		say(t, b, "/goals 1")
		got := tr.last()
		if strings.Contains(got, "OAuth-токен") {
			t.Errorf("a decode failure was blamed on the token:\n%s", got)
		}
		if !strings.Contains(got, "cannot unmarshal") {
			t.Errorf("the real error was not reported:\n%s", got)
		}
	})
}

// The reported failure: Telegram rejected the whole goal list as too long, so
// the reply was lost entirely instead of arriving in pieces.
func TestLongReplyArrivesInParts(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)

	var goals []engine.Goal
	for i := range 250 {
		goals = append(goals, engine.Goal{
			ID:   int64(200000000 + i),
			Name: fmt.Sprintf("Цель с довольно длинным названием номер %d", i),
			Type: "action",
		})
	}
	b.metrika = &fakeMetrika{goals: goals}
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")

	before := len(tr.sent)
	say(t, b, "/goals 1")
	parts := tr.sent[before:]

	if len(parts) < 2 {
		t.Fatalf("a 250-goal list went out as %d message(s); Telegram would reject it", len(parts))
	}
	for i, part := range parts {
		if n := utf16Len(part); n > b.transport.MaxUnits() {
			t.Errorf("part %d is %d units, over the %d limit", i, n, b.transport.MaxUnits())
		}
		// Each part says where it sits in the sequence.
		if !strings.Contains(part, fmt.Sprintf("(%d/%d)", i+1, len(parts))) {
			t.Errorf("part %d carries no position marker", i)
		}
	}

	// Every goal survives the split.
	all := strings.Join(parts, "\n")
	for _, id := range []int{200000000, 200000124, 200000249} {
		if !strings.Contains(all, fmt.Sprintf("goal:%d", id)) {
			t.Errorf("goal:%d was lost", id)
		}
	}
}

// ---- Report schedule ----

// fakeScheduler stands in for the report schedule.
type fakeScheduler struct {
	schedule engine.Schedule
	next     time.Time
	setErr   error
}

func (f *fakeScheduler) Schedule(context.Context) engine.Schedule { return f.schedule }

func (f *fakeScheduler) SetSchedule(_ context.Context, s engine.Schedule) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.schedule = s
	return nil
}

func (f *fakeScheduler) NextRun(context.Context) (time.Time, bool) {
	if f.schedule.Off() {
		return time.Time{}, false
	}
	return f.next, true
}

// The request that prompted this: hourly reports are too many, make it daily,
// from the chat rather than a config file.
func TestScheduleCommandSetsDailySchedule(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	sched := &fakeScheduler{
		schedule: engine.Schedule{Kind: engine.ScheduleEvery, Every: time.Hour},
		next:     time.Now().Add(3 * time.Hour),
	}
	b.SetScheduler(sched)

	say(t, b, "/schedule 10:00")

	if sched.schedule.Kind != engine.ScheduleDaily || sched.schedule.Hour != 10 {
		t.Fatalf("schedule = %+v, want daily at 10:00", sched.schedule)
	}
	got := tr.last()
	if !strings.Contains(got, "каждый день в 10:00") {
		t.Errorf("reply does not confirm the new schedule:\n%s", got)
	}
	// Changing it must not require a restart, and the reply should say so.
	if !strings.Contains(got, "перезапуск не нужен") {
		t.Errorf("reply does not say it takes effect on its own:\n%s", got)
	}
}

func TestScheduleCommandShowsCurrentSchedule(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	b.SetScheduler(&fakeScheduler{
		schedule: engine.Schedule{Kind: engine.ScheduleDaily, Hour: 9, Minute: 30},
		next:     time.Now().Add(90 * time.Minute),
	})

	say(t, b, "/schedule")

	got := tr.last()
	if !strings.Contains(got, "каждый день в 09:30") {
		t.Errorf("reply does not show the schedule:\n%s", got)
	}
	// Knowing when the next one lands is half the reason to ask.
	if !strings.Contains(got, "Ближайший") {
		t.Errorf("reply does not say when the next report lands:\n%s", got)
	}
	if !strings.Contains(got, "/schedule 10:00") {
		t.Errorf("reply does not show how to change it:\n%s", got)
	}
}

func TestScheduleCommandAcceptsIntervalAndOff(t *testing.T) {
	b, _, _ := newTestBot(t, admin)
	sched := &fakeScheduler{next: time.Now().Add(time.Hour)}
	b.SetScheduler(sched)

	say(t, b, "/schedule 6h")
	if sched.schedule.Kind != engine.ScheduleEvery || sched.schedule.Every != 6*time.Hour {
		t.Errorf("schedule = %+v, want every 6h", sched.schedule)
	}

	say(t, b, "/schedule off")
	if !sched.schedule.Off() {
		t.Errorf("schedule = %+v, want off", sched.schedule)
	}
}

func TestScheduleCommandRejectsNonsense(t *testing.T) {
	b, tr, _ := newTestBot(t, admin)
	sched := &fakeScheduler{schedule: engine.Schedule{Kind: engine.ScheduleDaily, Hour: 10}}
	b.SetScheduler(sched)

	for _, bad := range []string{"25:00", "по вторникам", "5m"} {
		say(t, b, "/schedule "+bad)
		if got := tr.last(); !strings.Contains(got, "❌") {
			t.Errorf("input %q was accepted: %s", bad, got)
		}
	}
	// The existing schedule must survive a rejected change.
	if sched.schedule.Hour != 10 {
		t.Errorf("a rejected input changed the schedule to %+v", sched.schedule)
	}
}

// ---- Report definitions ----

func seedCounter(t *testing.T, b *Bot) {
	t.Helper()
	say(t, b, "/addcounter")
	say(t, b, "Магазин 12345678 y0_token")
}

func TestAddReportWithPageAndGoals(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	seedCounter(t, b)

	say(t, b, "/addreport 1")
	say(t, b, "Чекаут и заказы | url=/checkout | goals=42,77")

	reports, err := db.ListReports(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("got %d reports, want 1", len(reports))
	}

	got := reports[0]
	if got.Name != "Чекаут и заказы" {
		t.Errorf("name = %q", got.Name)
	}
	if got.URLFilter != "/checkout" || got.URLMatch != "contains" {
		t.Errorf("scope = %q/%q", got.URLFilter, got.URLMatch)
	}
	if len(got.GoalIDs) != 2 || got.GoalIDs[0] != 42 || got.GoalIDs[1] != 77 {
		t.Errorf("goal_ids = %v", got.GoalIDs)
	}
}

func TestAddReportAcceptsEitherScopeAlone(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	seedCounter(t, b)
	ctx := context.Background()

	say(t, b, "/addreport 1")
	say(t, b, "Только страница | url=/cart")

	say(t, b, "/addreport 1")
	say(t, b, "Только цели | goals=42")

	say(t, b, "/addreport 1")
	say(t, b, "Весь счётчик")

	reports, _ := db.ListReports(ctx, 1)
	if len(reports) != 3 {
		t.Fatalf("got %d reports, want 3", len(reports))
	}
	if reports[0].URLFilter != "/cart" || len(reports[0].GoalIDs) != 0 {
		t.Errorf("page-only report = %+v", reports[0])
	}
	if reports[1].URLFilter != "" || len(reports[1].GoalIDs) != 1 {
		t.Errorf("goal-only report = %+v", reports[1])
	}
	// A report with neither is the whole counter, which is a legitimate choice
	// once other reports exist and the implicit one has stopped.
	if reports[2].Scoped() {
		t.Errorf("unscoped report reads as scoped: %+v", reports[2])
	}
}

func TestAddReportAcceptsRegexpScope(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	seedCounter(t, b)

	say(t, b, "/addreport 1")
	say(t, b, `Каталог | url~^/catalog/\d+`)

	reports, _ := db.ListReports(context.Background(), 1)
	if len(reports) != 1 || reports[0].URLMatch != "regexp" {
		t.Fatalf("reports = %+v", reports)
	}
}

func TestAddReportRejectsBadInput(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	seedCounter(t, b)
	say(t, b, "/addreport 1")

	for _, bad := range []string{
		"| url=/checkout",           // no name
		"Отчёт | goals=",            // empty goal list
		"Отчёт | goals=сорок два",   // not an ID
		"Отчёт | url~^/a[",          // uncompilable regexp
		"Отчёт | что-то непонятное", // unknown field
		"Отчёт | group=bad",         // unknown grouping
	} {
		say(t, b, bad)
		if got := tr.last(); !strings.ContainsAny(got, "❌") && !strings.Contains(got, "не должн") &&
			!strings.Contains(got, "Не понял") {
			t.Errorf("input %q was accepted: %s", bad, got)
		}
	}

	if reports, _ := db.ListReports(context.Background(), 1); len(reports) != 0 {
		t.Fatalf("malformed input created %d report(s)", len(reports))
	}
}

// goals= accepts the same goal:N spelling the /goals listing prints, so a value
// can be pasted straight across.
func TestAddReportAcceptsGoalPrefixedIDs(t *testing.T) {
	b, _, db := newTestBot(t, admin)
	seedCounter(t, b)

	say(t, b, "/addreport 1")
	say(t, b, "Заказы | goals=goal:42, goal:77")

	reports, _ := db.ListReports(context.Background(), 1)
	if len(reports) != 1 || len(reports[0].GoalIDs) != 2 {
		t.Fatalf("reports = %+v", reports)
	}
}

func TestListAndDeleteReports(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	seedCounter(t, b)
	ctx := context.Background()

	say(t, b, "/reports 1")
	if got := tr.last(); !strings.Contains(got, "сводка по всему счётчику") {
		t.Errorf("an unconfigured counter should say what it currently sends:\n%s", got)
	}

	say(t, b, "/addreport 1")
	say(t, b, "Чекаут | url=/checkout | goals=42")

	say(t, b, "/reports 1")
	got := tr.last()
	for _, want := range []string{"Чекаут", "/checkout", "goal:42"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing is missing %q:\n%s", want, got)
		}
	}

	reports, _ := db.ListReports(ctx, 1)
	say(t, b, fmt.Sprintf("/deletereport %d", reports[0].ID))
	if remaining, _ := db.ListReports(ctx, 1); len(remaining) != 0 {
		t.Errorf("report was not deleted: %+v", remaining)
	}
}

func TestAddReportWithGroupBy(t *testing.T) {
	b, tr, db := newTestBot(t, admin)
	seedCounter(t, b)
	ctx := context.Background()

	say(t, b, "/addreport 1")
	say(t, b, "Топ страниц | group=url")

	say(t, b, "/addreport 1")
	say(t, b, "Чекаут и цели | url=/checkout | goals=42 | group_by=url")

	reports, err := db.ListReports(ctx, 1)
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}

	if reports[0].GroupBy != "url" || reports[0].Name != "Топ страниц" {
		t.Errorf("report[0] = %+v", reports[0])
	}
	if !reports[0].Scoped() {
		t.Errorf("group=url report must be scoped")
	}

	if reports[1].GroupBy != "url" || reports[1].URLFilter != "/checkout" || len(reports[1].GoalIDs) != 1 {
		t.Errorf("report[1] = %+v", reports[1])
	}

	say(t, b, "/reports 1")
	got := tr.last()
	if !strings.Contains(got, "группировка по URL") {
		t.Errorf("listing does not mention grouping:\n%s", got)
	}
}
