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

	"github.com/isklv/metrika-alert/internal/model"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Router delivers alerts to configured destinations.
type Router struct {
	bot     *tgbotapi.BotAPI
	db      *model.DB
	webhook *http.Client
	mu      sync.Mutex
	chatIDs map[string]int64 // username -> chat ID cache
}

func NewRouter(botToken string, db *model.DB) (*Router, error) {
	if botToken == "" {
		return &Router{db: db, bot: nil, webhook: &http.Client{Timeout: 30 * time.Second}, chatIDs: make(map[string]int64)}, nil
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return nil, fmt.Errorf("init telegram bot: %w", err)
	}

	log.Printf("telegram bot ready: @%s", bot.Self.UserName)
	return &Router{
		bot:     bot,
		db:      db,
		webhook: &http.Client{Timeout: 30 * time.Second},
		chatIDs: make(map[string]int64),
	}, nil
}

// Alert delivers a notification to all configured actions.
func (r *Router) Alert(ctx context.Context, counterID int64, title, message string) error {
	if r.bot == nil {
		log.Printf("alert skipped: no telegram bot configured — %s", title)
		return nil
	}

	actions, err := r.db.ListAlertActions(ctx)
	if err != nil {
		return fmt.Errorf("list alert actions: %w", err)
	}
	if len(actions) == 0 {
		// No explicit actions: deliver to admin users from config via broadcast.
		return r.broadcastToAdmins(title, message)
	}

	for _, a := range actions {
		switch a.Type {
		case "telegram":
			if a.ChatID != nil {
				if err := r.sendToChat(*a.ChatID, title+message); err != nil {
					log.Printf("send to chat %d: %v", *a.ChatID, err)
				}
			}
		case "webhook":
			if err := r.sendWebhook(a.URL, counterID, title, message); err != nil {
				log.Printf("send webhook %s: %v", a.URL, err)
			}
		}
	}

	return nil
}

// broadcastToAdmins sends to all known admin chats. Called when no alert actions are configured yet.
func (r *Router) broadcastToAdmins(title, message string) error {
	if r.bot == nil {
		return nil
	}
	// Nothing to broadcast to without explicit actions — just log.
	log.Printf("alert (no actions configured): %s", title)
	return nil
}

// NotifyAdmin sends a one-off message to all admin users.
func (r *Router) NotifyAdmin(message string) error {
	if r.bot == nil {
		return fmt.Errorf("no telegram bot")
	}
	for _, chatID := range r.chatIDs {
		if err := r.sendToChat(chatID, message); err != nil {
			log.Printf("notify admin %d: %v", chatID, err)
		}
	}
	return nil
}

func (r *Router) sendToChat(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	// Truncate very long messages for Telegram's 4096 char limit.
	if len(text) > 3800 {
		msg.Text = text[:3750] + "\n\n…_сообщение обрезано_"
	}

	_, err := r.bot.Send(msg)
	return err
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

// RecordDeliveryChat associates a chat ID with the router for admin delivery.
func (r *Router) RecordDeliveryChat(chatID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.chatIDs == nil {
		r.chatIDs = make(map[string]int64)
	}
	r.chatIDs[fmt.Sprintf("chat-%d", chatID)] = chatID
}

// sendWebhook posts alert JSON to a webhook URL.
func (r *Router) sendWebhook(url string, counterID int64, title, message string) error {
	payload := map[string]any{
		"counter_id": counterID,
		"title":      title,
		"message":    strings.ReplaceAll(message, "*", ""),
		"timestamp":  time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
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
