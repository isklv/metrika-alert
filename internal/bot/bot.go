// Package bot implements the chat command interface for managing counters,
// monitors, triggers and alert destinations.
//
// The command logic is transport-agnostic: Telegram and VK Teams differ only in
// how a message is received and rendered, so both drive the same Bot through the
// Transport interface. IDs are handled as strings because VK Teams identifies
// users by address-like strings while Telegram uses numbers.
package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/isklv/metrika-alert/internal/model"
)

// Transport sends a rendered message over one chat platform.
type Transport interface {
	// Send delivers text, written in Telegram-flavoured Markdown, to a chat.
	// Implementations convert it to whatever markup they support.
	Send(ctx context.Context, chatID, text string) error
	// Name identifies the platform in logs and in the alert-action type column.
	Name() string
}

// Message is one inbound chat message, normalised across transports.
type Message struct {
	UserID string
	ChatID string
	Text   string
}

// Tasks runs the background jobs the bot can trigger on demand.
type Tasks interface {
	// RunReport builds and delivers a report; counterID 0 means every counter.
	RunReport(ctx context.Context, counterID int64) error
	// PollOnce fetches events for every counter once.
	PollOnce(ctx context.Context) error
}

// Bot handles chat commands for one transport.
type Bot struct {
	db        *model.DB
	transport Transport
	tasks     Tasks
	admins    map[string]bool

	mu      sync.Mutex
	pending map[string]*pendingAction // userID -> in-progress setup flow
}

// New builds a bot. adminIDs are the users allowed to configure the service;
// an empty list locks the bot down rather than opening it to everyone.
func New(transport Transport, db *model.DB, tasks Tasks, adminIDs []string) *Bot {
	admins := make(map[string]bool, len(adminIDs))
	for _, id := range adminIDs {
		if id = strings.TrimSpace(id); id != "" {
			admins[id] = true
		}
	}
	return &Bot{
		db:        db,
		transport: transport,
		tasks:     tasks,
		admins:    admins,
		pending:   make(map[string]*pendingAction),
	}
}

// IsAdmin reports whether the user may configure the service.
//
// An empty admin list denies everyone: the bot token alone is not an
// authorisation boundary, and counters hold Metrika OAuth tokens.
func (b *Bot) IsAdmin(userID string) bool {
	return b.admins[userID]
}

// Handle processes one inbound message. It reports whether the message was
// consumed, so a transport can stay quiet on chatter it does not own.
func (b *Bot) Handle(ctx context.Context, msg Message) bool {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return false
	}

	command, args := parseCommand(text)
	if command == "" {
		// Plain text only means something as the answer to a prompt this user
		// started. Anything else is chatter — in a group chat the bot sees every
		// message, and replying to all of them would make it unusable there.
		// Prompts are only ever opened for admins, so this path needs no further
		// authorisation check.
		return b.handlePending(ctx, msg.UserID, text)
	}

	if !b.IsAdmin(msg.UserID) {
		b.reply(ctx, msg.ChatID, fmt.Sprintf(
			"⛔ Доступ запрещён.\n\nТвой ID: `%s`\nДобавь его в config.yaml → `%s.admin_ids` и перезапусти сервис.",
			msg.UserID, b.transport.Name()))
		return true
	}

	// A command always cancels a half-finished flow, so a user who mistypes is
	// never stuck answering prompts for something they abandoned.
	b.clearPending(msg.UserID)

	switch command {
	case "start", "help", "menu":
		b.sendMenu(ctx, msg.ChatID)
	case "counters":
		b.listCounters(ctx, msg.ChatID)
	case "addcounter":
		b.promptAddCounter(ctx, msg)
	case "deletecounter":
		b.deleteCounter(ctx, msg.ChatID, args)
	case "monitors":
		b.listMonitors(ctx, msg.ChatID, args)
	case "addmonitor":
		b.promptAddMonitor(ctx, msg, args)
	case "deletemonitor":
		b.deleteMonitor(ctx, msg.ChatID, args)
	case "triggers":
		b.listTriggers(ctx, msg.ChatID, args)
	case "addtrigger":
		b.promptAddTrigger(ctx, msg, args)
	case "deletetrigger":
		b.deleteTrigger(ctx, msg.ChatID, args)
	case "actions":
		b.listAlertActions(ctx, msg.ChatID)
	case "addaction":
		b.promptAddAction(ctx, msg)
	case "deleteaction":
		b.deleteAlertAction(ctx, msg.ChatID, args)
	case "alerts":
		b.listAlerts(ctx, msg.ChatID, args)
	case "report":
		b.runReportNow(ctx, msg.ChatID, args)
	case "poll":
		b.runPollOnce(ctx, msg.ChatID)
	case "whoami":
		b.reply(ctx, msg.ChatID, fmt.Sprintf("Твой ID: `%s`\nЧат: `%s`\nПлатформа: %s", msg.UserID, msg.ChatID, b.transport.Name()))
	default:
		b.reply(ctx, msg.ChatID, "Неизвестная команда. /help — список команд.")
	}
	return true
}

// parseCommand splits "/addtrigger 3" into ("addtrigger", "3"). Telegram
// addresses commands in group chats as "/cmd@botname", so the suffix is dropped.
func parseCommand(text string) (command, args string) {
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	text = strings.TrimPrefix(text, "/")

	command = text
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		command = text[:i]
		args = strings.TrimSpace(text[i+1:])
	}
	if i := strings.IndexByte(command, '@'); i >= 0 {
		command = command[:i]
	}
	return strings.ToLower(command), args
}

func (b *Bot) reply(ctx context.Context, chatID, text string) {
	if err := b.transport.Send(ctx, chatID, text); err != nil {
		log.Printf("bot %s: send to %s: %v", b.transport.Name(), chatID, err)
	}
}

// stripMarkdown removes the formatting markers for consumers that do not
// render Markdown.
//
// Underscores are deliberately left alone: this service's text is full of
// Metrika field names — status_code, page_url, order_id — and stripping them
// would corrupt the very condition an alert is reporting on. The rare literal
// _italic_ showing its underscores is a far smaller flaw.
func stripMarkdown(s string) string {
	return strings.NewReplacer("*", "", "`", "").Replace(s)
}
