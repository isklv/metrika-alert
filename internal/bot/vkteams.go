package bot

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/isklv/metrika-alert/internal/vkteams"
)

// VKTeamsTransport adapts the VK Teams bot API to the Transport interface.
type VKTeamsTransport struct {
	client *vkteams.Client
}

// NewVKTeamsTransport wraps a VK Teams client.
func NewVKTeamsTransport(client *vkteams.Client) *VKTeamsTransport {
	return &VKTeamsTransport{client: client}
}

func (t *VKTeamsTransport) Name() string { return "vkteams" }

// MaxUnits keeps VK Teams replies within what its API accepts.
func (t *VKTeamsTransport) MaxUnits() int { return vkteams.MaxMessageRunes - 200 }

// Measure counts the message after conversion to HTML, which is what actually
// goes over the wire. The tags are not free: a list of `goal:N` entries gains
// thirteen characters per line, and measuring the Markdown instead let a reply
// pass the check and then be truncated.
func (t *VKTeamsTransport) Measure(text string) int {
	return utf8.RuneCountInString(vkteams.FromMarkdown(text))
}

// Send converts the bot's Markdown into the HTML subset VK Teams renders.
func (t *VKTeamsTransport) Send(ctx context.Context, chatID, text string) error {
	return t.client.SendText(ctx, chatID, vkteams.FromMarkdown(text))
}

// vkPollTime is how long /events/get blocks before returning empty.
const vkPollTime = 30 * time.Second

// RunVKTeams long-polls VK Teams for updates until ctx is cancelled.
//
// lastEventID is the API's cursor: the server only replays events newer than
// it, so it must advance past every event handled — including ones this bot
// ignores — or the same updates arrive forever.
func RunVKTeams(ctx context.Context, client *vkteams.Client, b *Bot) {
	self, err := client.GetSelf(ctx)
	if err != nil {
		log.Printf("vkteams bot: cannot verify token: %v", err)
		return
	}
	log.Printf("vkteams bot running: %s (%s)", self.Nick, self.UserID)

	// Start from 0: VK Teams then delivers only events that arrive from now on,
	// rather than replaying the backlog accumulated while the service was down.
	var lastEventID int64
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		events, err := client.GetEvents(ctx, lastEventID, vkPollTime)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("vkteams poll: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			// Cap the backoff so a long outage does not stall recovery.
			if backoff *= 2; backoff > time.Minute {
				backoff = time.Minute
			}
			continue
		}
		backoff = time.Second

		for _, ev := range events {
			if ev.EventID > lastEventID {
				lastEventID = ev.EventID
			}
			if ev.Type != "newMessage" {
				continue
			}

			payload, err := vkteams.ParseNewMessage(ev)
			if err != nil {
				log.Printf("vkteams event %d: %v", ev.EventID, err)
				continue
			}
			text := strings.TrimSpace(payload.Text)
			if text == "" || payload.From.UserID == "" {
				continue
			}

			b.Handle(ctx, Message{
				UserID: payload.From.UserID,
				ChatID: payload.Chat.ChatID,
				Text:   text,
			})
		}
	}
}
