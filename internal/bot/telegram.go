package bot

import (
	"context"
	"log"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// TelegramTransport adapts the Telegram Bot API to the Transport interface.
type TelegramTransport struct {
	api *tgbotapi.BotAPI
}

// NewTelegramTransport wraps an already-connected bot API client.
func NewTelegramTransport(api *tgbotapi.BotAPI) *TelegramTransport {
	return &TelegramTransport{api: api}
}

func (t *TelegramTransport) Name() string { return "telegram" }

// Send delivers Markdown text. Counter names and Metrika page titles routinely
// contain characters Telegram treats as markup, so a rejected message is retried
// without formatting instead of being lost.
func (t *TelegramTransport) Send(_ context.Context, chatID, text string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return err
	}

	msg := tgbotapi.NewMessage(id, text)
	msg.ParseMode = tgbotapi.ModeMarkdown
	if _, err := t.api.Send(msg); err == nil {
		return nil
	}

	plain := tgbotapi.NewMessage(id, stripMarkdown(text))
	_, err = t.api.Send(plain)
	return err
}

// RunTelegram pumps Telegram updates into the bot until ctx is cancelled.
func RunTelegram(ctx context.Context, api *tgbotapi.BotAPI, b *Bot) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := api.GetUpdatesChan(u)

	// StopReceivingUpdates unblocks the range below on shutdown.
	go func() {
		<-ctx.Done()
		api.StopReceivingUpdates()
	}()

	log.Printf("telegram bot running: @%s", api.Self.UserName)

	for update := range updates {
		msg := update.Message
		if msg == nil || msg.From == nil || msg.Text == "" {
			continue
		}
		b.Handle(ctx, Message{
			UserID: strconv.FormatInt(msg.From.ID, 10),
			ChatID: strconv.FormatInt(msg.Chat.ID, 10),
			Text:   msg.Text,
		})
	}
}
