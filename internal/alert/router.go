// Package alert delivers notifications to every configured destination.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/isklv/metrika-alert/internal/model"
	"github.com/isklv/metrika-alert/internal/vkteams"
)

// TelegramAPI is the slice of tgbotapi the router uses, so tests can stub it.
type TelegramAPI interface {
	Send(c tgbotapi.Chattable) (tgbotapi.Message, error)
}

// VKTeamsAPI is the slice of the VK Teams client the router uses.
type VKTeamsAPI interface {
	SendText(ctx context.Context, chatID, text string) error
}

// Options wires already-initialised bot clients into the router. Both channels
// are optional; a nil client simply means that channel is not configured.
//
// The clients are built by the caller rather than here so that a single bot
// connection (and its proxy settings) is shared with the command handlers.
type Options struct {
	Telegram       TelegramAPI
	TelegramAdmins []int64
	VKTeams        VKTeamsAPI
	VKTeamsAdmins  []string
}

// Router delivers alerts to configured destinations.
type Router struct {
	db      *model.DB
	tg      TelegramAPI
	vk      VKTeamsAPI
	webhook *http.Client

	tgAdmins []int64
	vkAdmins []string

	mu      sync.Mutex
	chatIDs map[string]int64 // username -> chat ID cache
}

// NewRouter builds a router over the configured channels.
func NewRouter(db *model.DB, opts Options) *Router {
	return &Router{
		db:       db,
		tg:       opts.Telegram,
		vk:       opts.VKTeams,
		webhook:  &http.Client{Timeout: 30 * time.Second},
		tgAdmins: opts.TelegramAdmins,
		vkAdmins: opts.VKTeamsAdmins,
		chatIDs:  make(map[string]int64),
	}
}

// Channels lists the delivery channels that are configured, for startup logging.
func (r *Router) Channels() []string {
	var out []string
	if r.tg != nil {
		out = append(out, "telegram")
	}
	if r.vk != nil {
		out = append(out, "vkteams")
	}
	if len(out) == 0 {
		out = append(out, "none (webhooks only)")
	}
	return out
}

// Alert delivers a notification to every configured action. Delivery failures
// on one destination never stop the others: an alert that reaches three of four
// recipients is far better than one that stops at the first broken webhook.
func (r *Router) Alert(ctx context.Context, counterID int64, title, message string) error {
	text := title
	if message != "" {
		text += "\n\n" + message
	}

	actions, err := r.db.ListAlertActions(ctx)
	if err != nil {
		return fmt.Errorf("list alert actions: %w", err)
	}
	if len(actions) == 0 {
		// Nothing configured yet — fall back to the admins from config so that a
		// fresh install still delivers instead of silently dropping alerts.
		return r.broadcastToAdmins(ctx, text)
	}

	var delivered int
	for _, a := range actions {
		if err := r.deliver(ctx, a, counterID, title, message, text); err != nil {
			log.Printf("alert: %s → %s: %v", a.Type, a.Destination(), err)
			continue
		}
		delivered++
	}
	if delivered == 0 {
		return fmt.Errorf("alert %q reached none of %d destinations", title, len(actions))
	}
	return nil
}

func (r *Router) deliver(ctx context.Context, a model.AlertAction, counterID int64, title, message, text string) error {
	switch a.Type {
	case "telegram":
		if r.tg == nil {
			return fmt.Errorf("telegram bot not configured")
		}
		if a.ChatID == nil {
			return fmt.Errorf("telegram action %d has no chat_id", a.ID)
		}
		return r.sendTelegram(*a.ChatID, text)
	case "vkteams":
		if r.vk == nil {
			return fmt.Errorf("vkteams bot not configured")
		}
		if a.Target == "" {
			return fmt.Errorf("vkteams action %d has no chat id", a.ID)
		}
		return r.vk.SendText(ctx, a.Target, vkteams.FromMarkdown(text))
	case "webhook":
		return r.sendWebhook(ctx, a.URL, counterID, title, message)
	default:
		return fmt.Errorf("unknown action type %q", a.Type)
	}
}

// broadcastToAdmins sends to the admin accounts named in config. Used until
// explicit alert actions exist.
func (r *Router) broadcastToAdmins(ctx context.Context, text string) error {
	var delivered, attempted int

	if r.tg != nil {
		for _, chatID := range r.tgAdmins {
			attempted++
			if err := r.sendTelegram(chatID, text); err != nil {
				log.Printf("alert: telegram admin %d: %v", chatID, err)
				continue
			}
			delivered++
		}
	}
	if r.vk != nil {
		html := vkteams.FromMarkdown(text)
		for _, userID := range r.vkAdmins {
			attempted++
			if err := r.vk.SendText(ctx, userID, html); err != nil {
				log.Printf("alert: vkteams admin %s: %v", userID, err)
				continue
			}
			delivered++
		}
	}

	if attempted == 0 {
		log.Printf("alert dropped — no destinations and no admins configured: %s", firstLine(text))
		return nil
	}
	if delivered == 0 {
		return fmt.Errorf("alert reached none of %d admin(s)", attempted)
	}
	return nil
}

// NotifyAdmin sends a one-off operational message to the configured admins.
func (r *Router) NotifyAdmin(ctx context.Context, message string) error {
	return r.broadcastToAdmins(ctx, message)
}

func (r *Router) sendTelegram(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, truncateRunes(text, telegramLimit))
	msg.ParseMode = tgbotapi.ModeMarkdown
	_, err := r.tg.Send(msg)
	if err == nil {
		return nil
	}
	// Metrika data (page titles, URLs) can contain stray Markdown markers that
	// make Telegram reject the whole message. Retry once as plain text so the
	// alert still arrives.
	plain := tgbotapi.NewMessage(chatID, truncateRunes(stripMarkdown(text), telegramLimit))
	if _, retryErr := r.tg.Send(plain); retryErr != nil {
		return fmt.Errorf("%w (plain-text retry: %v)", err, retryErr)
	}
	return nil
}

// AddChatID registers a user's chat ID for future delivery.
func (r *Router) AddChatID(username string, chatID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chatIDs[username] = chatID
}

// ChatIDFor returns a cached chat ID or 0.
func (r *Router) ChatIDFor(username string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chatIDs[username]
}

// sendWebhook posts alert JSON to a webhook URL.
func (r *Router) sendWebhook(ctx context.Context, url string, counterID int64, title, message string) error {
	payload := map[string]any{
		"counter_id": counterID,
		"title":      stripMarkdown(title),
		"message":    stripMarkdown(message),
		"timestamp":  time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.webhook.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

// telegramLimit is Telegram's per-message ceiling, with room for the notice.
const telegramLimit = 4000

const truncationNotice = "\n\n…сообщение обрезано"

// truncateRunes cuts on a rune boundary. Cutting by bytes would split a
// multi-byte character and produce invalid UTF-8 that Telegram rejects.
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	keep := limit - utf8.RuneCountInString(truncationNotice)
	count := 0
	for i := range s {
		if count == keep {
			return s[:i] + truncationNotice
		}
		count++
	}
	return s
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
