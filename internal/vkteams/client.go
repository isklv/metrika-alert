// Package vkteams implements the VK Teams (Mail.ru Myteam) Bot API client.
//
// The API is a flat HTTP/GET interface documented at https://teams.vk.com/botapi/:
// every call carries the bot token as a query parameter and answers with a JSON
// envelope {"ok": bool, "description": string, ...}. Incoming messages arrive by
// long polling /events/get rather than by webhook, which keeps the service
// outbound-only and deployable behind NAT.
package vkteams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one VK Teams bot.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

// Options configure a Client. BaseURL defaults to the VK Teams cloud endpoint.
type Options struct {
	BaseURL   string
	ProxyURL  string
	Transport http.RoundTripper // overrides ProxyURL; used by tests
}

// DefaultBaseURL is the VK Teams cloud bot API.
const DefaultBaseURL = "https://myteam.mail.ru/bot/v1"

// New builds a client. The long-poll timeout is deliberately generous: the
// /events/get call is expected to block for pollTime seconds before answering.
func New(token string, opts Options) (*Client, error) {
	if token == "" {
		return nil, fmt.Errorf("vkteams: empty bot token")
	}
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("vkteams: bad base_url %q: %w", base, err)
	}

	transport := opts.Transport
	if transport == nil && opts.ProxyURL != "" {
		proxy, err := url.Parse(opts.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("vkteams: bad proxy_url %q: %w", opts.ProxyURL, err)
		}
		transport = &http.Transport{Proxy: http.ProxyURL(proxy)}
	}

	return &Client{
		token:   token,
		baseURL: base,
		http:    &http.Client{Timeout: 90 * time.Second, Transport: transport},
	}, nil
}

// BaseURL reports the endpoint the client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// response is the envelope every VK Teams endpoint wraps its payload in.
type response struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

func (c *Client) call(ctx context.Context, path string, params url.Values, dst any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("token", c.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("vkteams %s: %w", path, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("vkteams %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("vkteams %s: read body: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("vkteams %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}

	// The envelope is checked before the payload so that ok:false surfaces the
	// API's own description instead of a confusing decode error.
	var env response
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("vkteams %s: decode response: %w", path, err)
	}
	if !env.OK {
		desc := env.Description
		if desc == "" {
			desc = "request rejected"
		}
		return fmt.Errorf("vkteams %s: %s", path, desc)
	}

	if dst != nil {
		if err := json.Unmarshal(body, dst); err != nil {
			return fmt.Errorf("vkteams %s: decode payload: %w", path, err)
		}
	}
	return nil
}

// Self describes the bot account.
type Self struct {
	UserID    string `json:"userId"`
	Nick      string `json:"nick"`
	FirstName string `json:"firstName"`
}

// GetSelf verifies the token and returns the bot's own account.
func (c *Client) GetSelf(ctx context.Context) (*Self, error) {
	var self Self
	if err := c.call(ctx, "/self/get", nil, &self); err != nil {
		return nil, err
	}
	return &self, nil
}

// SendText delivers a message to a chat. chatID is a VK Teams chat or user ID
// (for example "user@corp.example" or "689123456"). Text must already be valid
// HTML for the parse mode the API is told to use.
func (c *Client) SendText(ctx context.Context, chatID, text string) error {
	if chatID == "" {
		return fmt.Errorf("vkteams: empty chatId")
	}
	text = TruncateMessage(text)

	params := url.Values{}
	params.Set("chatId", chatID)
	params.Set("text", text)
	params.Set("parseMode", "HTML")

	var resp struct {
		MsgID string `json:"msgId"`
	}
	return c.call(ctx, "/messages/sendText", params, &resp)
}

// Event is one update from the long-poll stream.
type Event struct {
	EventID int64           `json:"eventId"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// NewMessagePayload is the payload of a "newMessage" event.
type NewMessagePayload struct {
	MsgID string `json:"msgId"`
	Text  string `json:"text"`
	Chat  struct {
		ChatID string `json:"chatId"`
		Type   string `json:"type"`
		Title  string `json:"title"`
	} `json:"chat"`
	From struct {
		UserID    string `json:"userId"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	} `json:"from"`
	Timestamp int64 `json:"timestamp"`
}

// GetEvents long-polls for updates newer than lastEventID. It blocks for up to
// pollTime before returning an empty slice.
func (c *Client) GetEvents(ctx context.Context, lastEventID int64, pollTime time.Duration) ([]Event, error) {
	seconds := int(pollTime.Seconds())
	if seconds <= 0 {
		seconds = 30
	}

	params := url.Values{}
	params.Set("lastEventId", strconv.FormatInt(lastEventID, 10))
	params.Set("pollTime", strconv.Itoa(seconds))

	var resp struct {
		Events []Event `json:"events"`
	}
	if err := c.call(ctx, "/events/get", params, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ParseNewMessage decodes the payload of a "newMessage" event.
func ParseNewMessage(ev Event) (*NewMessagePayload, error) {
	if ev.Type != "newMessage" {
		return nil, fmt.Errorf("vkteams: event %d is %q, not newMessage", ev.EventID, ev.Type)
	}
	var payload NewMessagePayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil, fmt.Errorf("vkteams: decode newMessage payload: %w", err)
	}
	return &payload, nil
}
