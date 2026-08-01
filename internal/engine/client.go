package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// MetrikaClient wraps Yandex.Metrika HTTP API calls.
type MetrikaClient struct {
	counterID string
	token     string
	baseURL   string
	logsURL   string
	http      *http.Client
}

func NewMetrikaClient(c *model.Counter, baseURL, logsURL string) *MetrikaClient {
	return &MetrikaClient{
		counterID: c.CounterID,
		token:     c.OAuthToken,
		baseURL:   baseURL,
		logsURL:   logsURL,
		http:      &http.Client{Timeout: 60 * time.Second},
	}
}

type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *MetrikaClient) doJSON(ctx context.Context, method, path string, body any, dst any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "OAuth "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		// Rate limit — caller should back off.
		retryAfter := resp.Header.Get("Retry-After")
		if retryAfter != "" {
			return fmt.Errorf("rate limited: retry after %s", retryAfter)
		}
		return fmt.Errorf("rate limited (429)")
	}

	if resp.StatusCode >= 400 {
		var apiErr apiError
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Error.Message != "" {
			return fmt.Errorf("metrika API %d: %s", resp.StatusCode, apiErr.Error.Message)
		}
		return fmt.Errorf("metrika API error: %d", resp.StatusCode)
	}

	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}

// LogRequest is the payload for Logs API streaming.
type LogRequest struct {
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Limit     int    `json:"limit"`
	Fields    []string `json:"fields"`
	Filters   []LogFilter `json:"filters,omitempty"`
}

// LogFilter is a Logs API filter.
type LogFilter struct {
	Name  string `json:"name"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// Hit is one event from Logs API.
type Hit map[string]any

// FetchEvents queries the Logs API for events in [since, now).
// Returns at most limit events. The caller should pass since as the time of the last successfully processed event.
func (c *MetrikaClient) FetchEvents(ctx context.Context, since time.Time, limit int) ([]Hit, error) {
	if since.IsZero() {
		since = time.Now().Add(-time.Hour)
	}

	req := LogRequest{
		StartTime: since.Format(time.RFC3339),
		EndTime:   time.Now().Format(time.RFC3339),
		Limit:     limit,
		Fields: []string{
			"event_time", "page_url", "title", "goals_id", "status_code",
			"revenue", "order_id", "client_id", "user_id",
		},
	}

	var resp struct {
		TotalHits int64 `json:"total_hits"`
		Hits      []Hit `json:"hits"`
	}
	if err := c.doJSON(ctx, "POST", "/"+c.counterID+"/logs/hits", req, &resp); err != nil {
		return nil, fmt.Errorf("fetch events: %w", err)
	}

	return resp.Hits, nil
}

// ParseEvent converts a Logs API hit into our MetrikaEvent model.
func ParseEvent(hit Hit) *model.MetrikaEvent {
	ev := &model.MetrikaEvent{
		Params: make(map[string]string),
	}

	if t, ok := hit["event_time"].(string); ok && t != "" {
		if p, err := time.Parse(time.RFC3339, t); err == nil {
			ev.EventTime = p
		}
	}
	if u, ok := hit["page_url"].(string); ok {
		ev.PageURL = u
	}
	if t, ok := hit["title"].(string); ok {
		ev.Title = t
	}
	if sc, ok := hit["status_code"].(float64); ok && sc > 0 {
		ev.Status = strconv.Itoa(int(sc))
	}
	if r, ok := hit["revenue"].(float64); ok && r > 0 {
		ev.Revenue = r
	}
	if o, ok := hit["order_id"].(string); ok {
		ev.OrderID = o
	}
	if c, ok := hit["client_id"].(string); ok {
		ev.ClientID = c
	}
	if u, ok := hit["user_id"].(string); ok {
		ev.UserID = u
	}

	// goals_id comes as a string "123,456" or single "123".
	if g, ok := hit["goals_id"].(string); ok && g != "" {
		for _, id := range splitComma(g) {
			ev.GoalsID = append(ev.GoalsID, id)
		}
	}

	return ev
}

// FetchReport queries the standard Metrika API for a report with the given dimensions and metrics.
func (c *MetrikaClient) FetchReport(ctx context.Context, date1, date2 string, metrics []string, limit int) ([]map[string]any, error) {
	type reportReq struct {
		Date1   string   `json:"date1"`
		Date2   string   `json:"date2"`
		Metrics []string `json:"metrics"`
		Limit   int      `json:"limit"`
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := c.doJSON(ctx, "POST", "/"+c.counterID+"/report", reportReq{
		Date1:   date1,
		Date2:   date2,
		Metrics: metrics,
		Limit:   limit,
	}, &resp); err != nil {
		return nil, fmt.Errorf("fetch report: %w", err)
	}

	return resp.Data, nil
}

func splitComma(s string) []string {
	var out []string
	for _, v := range bytes.Split([]byte(s), []byte(",")) {
		if t := string(bytes.TrimSpace(v)); t != "" {
			out = append(out, t)
		}
	}
	return out
}
