package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

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

// MaxUnits is Telegram's per-message ceiling, with room for the part counter.
func (t *TelegramTransport) MaxUnits() int { return telegramMaxUnits }

// Measure counts UTF-16 code units, as Telegram does. Markdown is sent through
// unchanged, so the text is measured as written.
func (t *TelegramTransport) Measure(text string) int { return utf16Len(text) }

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

// SendDocument delivers a file with an optional caption.
func (t *TelegramTransport) SendDocument(_ context.Context, chatID, filename string, content []byte, caption string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return err
	}

	doc := tgbotapi.NewDocument(id, tgbotapi.FileBytes{
		Name:  filename,
		Bytes: content,
	})
	if caption != "" {
		doc.Caption = caption
		doc.ParseMode = tgbotapi.ModeMarkdown
	}

	if _, err := t.api.Send(doc); err == nil {
		return nil
	}

	if caption != "" {
		doc.Caption = stripMarkdown(caption)
		doc.ParseMode = ""
		if _, err := t.api.Send(doc); err == nil {
			return nil
		}
	}

	doc.Caption = ""
	doc.ParseMode = ""
	_, err = t.api.Send(doc)
	return err
}

func downloadTelegramDocument(ctx context.Context, api *tgbotapi.BotAPI, fileID string) ([]byte, error) {
	fileURL, err := api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, fmt.Errorf("get file url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	resp, err := api.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
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
		if msg == nil || msg.From == nil {
			continue
		}

		var fileData []byte
		if msg.Document != nil && msg.Document.FileSize <= 10<<20 {
			data, err := downloadTelegramDocument(ctx, api, msg.Document.FileID)
			if err != nil {
				log.Printf("telegram: download document %s: %v", msg.Document.FileName, err)
			} else {
				fileData = data
			}
		}

		text := strings.TrimSpace(msg.Text)
		if text == "" {
			text = strings.TrimSpace(msg.Caption)
		}
		if text == "" && len(fileData) > 0 {
			text = string(fileData)
		}
		if text == "" && len(fileData) == 0 {
			continue
		}

		b.Handle(ctx, Message{
			UserID: strconv.FormatInt(msg.From.ID, 10),
			ChatID: strconv.FormatInt(msg.Chat.ID, 10),
			Text:   text,
			Data:   fileData,
		})
	}
}
