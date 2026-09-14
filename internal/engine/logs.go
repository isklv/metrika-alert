package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// LogsClient speaks the Yandex.Metrika Logs API.
//
// The API is asynchronous by design: a caller orders an export, Metrika
// prepares it over minutes, and only then can the caller download it as TSV.
// The full lifecycle is
//
//	Evaluate → Create → (poll) Get → Download each part → Clean
//
// Two constraints shape everything above this client. Metrika refuses a
// date2 of the current day, so Logs API data is never fresher than yesterday;
// and each counter may hold only a few prepared requests at once, so a request
// that is created must eventually be cleaned or cancelled.
type LogsClient struct {
	counterID string
	tr        *transport
}

// NewLogsClient builds a Logs API client for one counter.
func NewLogsClient(c *model.Counter, baseURL string) *LogsClient {
	return &LogsClient{counterID: c.CounterID, tr: newTransport(c.OAuthToken, baseURL)}
}

// Source selects which log Metrika exports.
type Source string

const (
	// SourceHits exports individual page views; its fields use the ym:pv: prefix.
	SourceHits Source = "hits"
	// SourceVisits exports sessions; its fields use the ym:s: prefix.
	SourceVisits Source = "visits"
)

// Status values a log request moves through.
const (
	StatusCreated       = "created"
	StatusProcessed     = "processed"
	StatusCanceled      = "canceled"
	StatusAwaitingRetry = "awaiting_retry"
	StatusFailed        = "processing_failed"
	StatusCleanedByUser = "cleaned_by_user"
	StatusCleanedTooOld = "cleaned_automatically_as_too_old"
)

// LogRequest is the state of one export as Metrika reports it.
type LogRequest struct {
	RequestID int64    `json:"request_id"`
	CounterID int64    `json:"counter_id"`
	Source    string   `json:"source"`
	Date1     string   `json:"date1"`
	Date2     string   `json:"date2"`
	Status    string   `json:"status"`
	Size      int64    `json:"size"`
	Fields    []string `json:"fields"`
	Parts     []struct {
		PartNumber int   `json:"part_number"`
		Size       int64 `json:"size"`
	} `json:"parts"`
}

// Ready reports whether the export can be downloaded.
func (r *LogRequest) Ready() bool { return r.Status == StatusProcessed }

// Terminal reports whether the request will never produce data, so the caller
// should stop waiting and order a new one.
func (r *LogRequest) Terminal() bool {
	switch r.Status {
	case StatusCanceled, StatusFailed, StatusCleanedByUser, StatusCleanedTooOld:
		return true
	}
	return false
}

// HitFields are the hits-source fields this service exports. They map onto
// model.MetrikaEvent, which is what trigger conditions are written against.
var HitFields = []string{
	"ym:pv:watchID",
	"ym:pv:dateTime",
	"ym:pv:URL",
	"ym:pv:title",
	"ym:pv:goalsID",
	"ym:pv:httpError",
	"ym:pv:purchaseRevenue",
	"ym:pv:purchaseID",
	"ym:pv:clientID",
	"ym:pv:counterUserIDHash",
}

// logsDateLayout is the ISO 8601 form the Logs API expects for date1/date2.
const logsDateLayout = "2006-01-02T15:04:05Z"

// Window is the period an export covers.
type Window struct {
	From time.Time
	To   time.Time
}

func (w Window) params(fields []string, source Source) url.Values {
	return url.Values{
		"date1":  {w.From.UTC().Format(logsDateLayout)},
		"date2":  {w.To.UTC().Format(logsDateLayout)},
		"fields": {strings.Join(fields, ",")},
		"source": {string(source)},
	}
}

// Evaluation is Metrika's verdict on whether an export can be ordered.
type Evaluation struct {
	Possible        bool `json:"possible"`
	MaxPossibleDays int  `json:"max_possible_day_quantity"`
}

// Evaluate asks whether an export for this window is within quota. Calling it
// before Create turns a rejected order into a clear log line instead of a
// request that is created and then fails.
func (c *LogsClient) Evaluate(ctx context.Context, w Window) (*Evaluation, error) {
	var resp struct {
		Evaluation Evaluation `json:"log_request_evaluation"`
	}
	path := counterPath(c.counterID, "/logrequests/evaluate")
	if err := c.tr.doJSON(ctx, http.MethodGet, path, w.params(HitFields, SourceHits), &resp); err != nil {
		return nil, fmt.Errorf("evaluate log request: %w", err)
	}
	return &resp.Evaluation, nil
}

// Create orders an export and returns it in its initial state.
func (c *LogsClient) Create(ctx context.Context, w Window) (*LogRequest, error) {
	var resp struct {
		LogRequest LogRequest `json:"log_request"`
	}
	path := counterPath(c.counterID, "/logrequests")
	if err := c.tr.doJSON(ctx, http.MethodPost, path, w.params(HitFields, SourceHits), &resp); err != nil {
		return nil, fmt.Errorf("create log request: %w", err)
	}
	return &resp.LogRequest, nil
}

// Get reports the current state of an export.
func (c *LogsClient) Get(ctx context.Context, requestID int64) (*LogRequest, error) {
	var resp struct {
		LogRequest LogRequest `json:"log_request"`
	}
	path := counterPath(c.counterID, "/logrequest/"+strconv.FormatInt(requestID, 10))
	if err := c.tr.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("get log request %d: %w", requestID, err)
	}
	return &resp.LogRequest, nil
}

// Clean releases a processed export. Metrika caps how many prepared requests a
// counter may hold, so skipping this eventually blocks every new export.
func (c *LogsClient) Clean(ctx context.Context, requestID int64) error {
	path := counterPath(c.counterID, "/logrequest/"+strconv.FormatInt(requestID, 10)+"/clean")
	if err := c.tr.doJSON(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("clean log request %d: %w", requestID, err)
	}
	return nil
}

// Cancel stops an export that has not finished processing.
func (c *LogsClient) Cancel(ctx context.Context, requestID int64) error {
	path := counterPath(c.counterID, "/logrequest/"+strconv.FormatInt(requestID, 10)+"/cancel")
	if err := c.tr.doJSON(ctx, http.MethodPost, path, nil, nil); err != nil {
		return fmt.Errorf("cancel log request %d: %w", requestID, err)
	}
	return nil
}

// DownloadPart streams one part of a processed export as TSV. The caller must
// close the returned reader.
func (c *LogsClient) DownloadPart(ctx context.Context, requestID int64, part int) (io.ReadCloser, error) {
	path := counterPath(c.counterID,
		"/logrequest/"+strconv.FormatInt(requestID, 10)+"/part/"+formatInt(part)+"/download")

	resp, err := c.tr.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("download log request %d part %d: %w", requestID, part, err)
	}
	return resp.Body, nil
}

// DownloadEvents downloads every part of a processed export and parses it into
// events. onEvent is called per row so a large export never has to be held in
// memory all at once.
func (c *LogsClient) DownloadEvents(ctx context.Context, req *LogRequest, onEvent func(*model.MetrikaEvent) error) error {
	for _, part := range req.Parts {
		body, err := c.DownloadPart(ctx, req.RequestID, part.PartNumber)
		if err != nil {
			return err
		}
		err = ParseTSV(body, onEvent)
		body.Close()
		if err != nil {
			return fmt.Errorf("part %d: %w", part.PartNumber, err)
		}
	}
	return nil
}

// maxTSVLine bounds one row; a page URL with a long query string is the
// realistic worst case.
const maxTSVLine = 1 << 20

// ParseTSV reads a Logs API export. The first line names the fields, so the
// column order is taken from the payload rather than assumed from the request.
func ParseTSV(r io.Reader, onEvent func(*model.MetrikaEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxTSVLine)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read header: %w", err)
		}
		// An empty export is a legitimate answer: no hits in the window.
		return nil
	}
	header := strings.Split(scanner.Text(), "\t")

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		ev := parseTSVRow(header, strings.Split(line, "\t"))
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// parseTSVRow maps one export row onto the event model.
func parseTSVRow(header, cols []string) *model.MetrikaEvent {
	ev := &model.MetrikaEvent{Params: make(map[string]string, len(header))}

	for i, name := range header {
		if i >= len(cols) {
			break
		}
		value := unescapeTSV(cols[i])
		// Metrika writes an absent value as \N.
		if value == `\N` {
			continue
		}

		switch name {
		case "ym:pv:dateTime":
			ev.EventTime = parseMetrikaTime(value)
		case "ym:pv:URL":
			ev.PageURL = value
		case "ym:pv:title":
			ev.Title = value
		case "ym:pv:httpError":
			ev.Status = value
		case "ym:pv:goalsID":
			ev.GoalsID = parseArray(value)
		case "ym:pv:purchaseRevenue":
			// An event may carry several purchases; a trigger on revenue asks
			// about the size of the event, so they are summed.
			for _, v := range parseArray(value) {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					ev.Revenue += f
				}
			}
		case "ym:pv:purchaseID":
			if ids := parseArray(value); len(ids) > 0 {
				ev.OrderID = ids[0]
			}
		case "ym:pv:clientID":
			ev.ClientID = value
		case "ym:pv:counterUserIDHash":
			ev.UserID = value
		default:
			// Anything else stays addressable by its field name, so a trigger
			// can reference a field this switch does not know about.
			ev.Params[name] = value
		}
	}
	return ev
}

// metrikaTimeLayouts are the forms ym:pv:dateTime is observed in. The value is
// in the counter's own time zone and carries no offset, so it is read as local
// time — the same zone the service compares trigger windows against.
var metrikaTimeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	time.RFC3339,
}

func parseMetrikaTime(s string) time.Time {
	for _, layout := range metrikaTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseArray reads an array column. Metrika writes them as [1,2,3], and a
// single scalar is also accepted so the parser survives a field that changes
// arity.
func parseArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" {
		return nil
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")

	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, `'"`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// unescapeTSV reverses the escaping Metrika applies to tab-separated values, so
// a page title containing a tab or newline survives the round trip intact.
func unescapeTSV(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case '\'':
			b.WriteByte('\'')
		case '0':
			b.WriteByte(0)
		default:
			// Not an escape this format defines — keep both bytes verbatim
			// rather than silently dropping the backslash.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
