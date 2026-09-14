package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/isklv/metrika-alert/internal/model"
)

// ReportClient speaks the Yandex.Metrika Reporting API (GET /stat/v1/data).
//
// Unlike the Logs API this one is aggregated and near real time: it accepts
// date2=today and answers in a single call, which is why reports use it and
// raw-event triggers cannot.
type ReportClient struct {
	counterID string
	tr        *transport
}

func NewReportClient(c *model.Counter, baseURL string) *ReportClient {
	return &ReportClient{counterID: c.CounterID, tr: newTransport(c.OAuthToken, baseURL)}
}

// Standard session metrics. The API names them with a ym:s: prefix; the bare
// names used in config and in reports are mapped here so neither the report
// builder nor the user has to spell them out.
const (
	MetricVisits      = "ym:s:visits"
	MetricUsers       = "ym:s:users"
	MetricPageviews   = "ym:s:pageviews"
	MetricBounceRate  = "ym:s:bounceRate"
	MetricPageDepth   = "ym:s:pageDepth"
	MetricAvgDuration = "ym:s:avgVisitDurationSeconds"
	MetricGoalReaches = "ym:s:sumGoalReachesAny"
)

// SummaryMetrics is the metric set a periodic report is built from. Order is
// significant: the API answers with one number per metric, in request order.
var SummaryMetrics = []string{
	MetricVisits,
	MetricUsers,
	MetricPageviews,
	MetricBounceRate,
	MetricPageDepth,
	MetricAvgDuration,
	MetricGoalReaches,
}

// GoalReachesMetric builds the per-goal reach metric for a goal ID.
func GoalReachesMetric(goalID int64) string {
	return "ym:s:goal" + strconv.FormatInt(goalID, 10) + "reaches"
}

// Query is one Reporting API request.
//
// Date1 and Date2 accept either YYYY-MM-DD or the API's relative keywords
// ("today", "yesterday", "7daysAgo"), which is what makes a like-for-like
// period comparison expressible without any date arithmetic here.
type Query struct {
	Metrics    []string
	Dimensions []string
	Date1      string
	Date2      string
	Filters    string
	Limit      int
	Accuracy   string
}

func (q Query) params(counterID string) url.Values {
	p := url.Values{}
	p.Set("ids", counterID)
	p.Set("metrics", strings.Join(q.Metrics, ","))
	if len(q.Dimensions) > 0 {
		p.Set("dimensions", strings.Join(q.Dimensions, ","))
	}
	if q.Date1 != "" {
		p.Set("date1", q.Date1)
	}
	if q.Date2 != "" {
		p.Set("date2", q.Date2)
	}
	if q.Filters != "" {
		p.Set("filters", q.Filters)
	}
	if q.Limit > 0 {
		p.Set("limit", formatInt(q.Limit))
	}
	accuracy := q.Accuracy
	if accuracy == "" {
		// Ask for exact numbers: an alert threshold compared against a sampled
		// estimate is not a threshold.
		accuracy = "full"
	}
	p.Set("accuracy", accuracy)
	return p
}

// Row is one grouped result. Metrics are positional, matching Query.Metrics.
type Row struct {
	Dimensions []map[string]any `json:"dimensions"`
	Metrics    []float64        `json:"metrics"`
}

// Dimension returns the named attribute of the row's i-th grouping key.
func (r Row) Dimension(i int, key string) string {
	if i >= len(r.Dimensions) {
		return ""
	}
	if v, ok := r.Dimensions[i][key].(string); ok {
		return v
	}
	return ""
}

// Result is a Reporting API response.
type Result struct {
	Data      []Row     `json:"data"`
	Totals    []float64 `json:"totals"`
	TotalRows int       `json:"total_rows"`
	Sampled   bool      `json:"sampled"`
}

// Total returns the i-th total, or 0 when the response is shorter than asked.
func (r *Result) Total(i int) float64 {
	if i < 0 || i >= len(r.Totals) {
		return 0
	}
	return r.Totals[i]
}

// Empty reports whether the period produced no data at all.
func (r *Result) Empty() bool { return len(r.Data) == 0 && len(r.Totals) == 0 }

// Fetch runs a report.
func (c *ReportClient) Fetch(ctx context.Context, q Query) (*Result, error) {
	if len(q.Metrics) == 0 {
		return nil, fmt.Errorf("report query needs at least one metric")
	}
	// The API caps a request at 20 metrics.
	if len(q.Metrics) > 20 {
		return nil, fmt.Errorf("report query has %d metrics, the API allows 20", len(q.Metrics))
	}

	var result Result
	if err := c.tr.doJSON(ctx, http.MethodGet, statPath, q.params(c.counterID), &result); err != nil {
		return nil, fmt.Errorf("fetch report: %w", err)
	}
	return &result, nil
}

// Goal is a conversion goal configured on the counter.
type Goal struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	IsFavorite bool   `json:"is_favorite"`
}

// Active reports whether the goal is still in use. The API does not document
// the status values, so anything it does not explicitly mark as deleted counts
// as active — guessing the other way would hide real goals.
func (g Goal) Active() bool {
	return !strings.EqualFold(g.Status, "Deleted")
}

// goalTypeLabels renders the documented goal types in Russian. An unknown type
// falls through to its own name rather than being dropped.
var goalTypeLabels = map[string]string{
	"action":         "JS-событие",
	"chat":           "чат",
	"email":          "клик по email",
	"file":           "скачивание файла",
	"messenger":      "мессенджер",
	"number":         "глубина просмотра",
	"payment_system": "платёжная система",
	"phone":          "клик по телефону",
	"search":         "поиск по сайту",
	"social":         "соцсеть",
	"step":           "составная цель",
	"url":            "посещение страницы",
	"visit_duration": "время на сайте",
}

// TypeLabel renders the goal's type for a human.
func (g Goal) TypeLabel() string {
	if label, ok := goalTypeLabels[g.Type]; ok {
		return label
	}
	if g.Type == "" {
		return "цель"
	}
	return g.Type
}

// Goals lists the counter's goals so reports can break conversions down by
// goal. Best effort: a counter whose token lacks management access still gets
// a report, just with the aggregate goal count instead of a per-goal list.
func (c *ReportClient) Goals(ctx context.Context) ([]Goal, error) {
	var resp struct {
		Goals []Goal `json:"goals"`
	}
	if err := c.tr.doJSON(ctx, http.MethodGet, counterPath(c.counterID, "/goals"), nil, &resp); err != nil {
		return nil, fmt.Errorf("list goals: %w", err)
	}
	return resp.Goals, nil
}

// ---- Time series ----

// The alerting engine works on hourly series: a counter's traffic has a strong
// daily and weekly rhythm, so "now versus the same hour last Tuesday" is the
// only comparison that distinguishes a real drop from the normal evening lull.
const byTimePath = "/stat/v1/data/bytime"

// Time groupings this service requests. Both are plain values with no
// documented point cap; the "minutes"/"hours" modes do carry one, which is why
// they are avoided.
const (
	GroupHour       = "hour"
	GroupTenMinutes = "dekaminute"
)

// TimeSeries is a metric-by-interval result from the bytime endpoint.
//
// Values is indexed [metric][interval], matching the request's metric order and
// the Intervals slice, so a caller reads one metric's history as Values[i].
type TimeSeries struct {
	Intervals []time.Time
	Values    [][]float64
	Sampled   bool

	// index maps an interval start to its position. Built on first lookup:
	// a four-week series holds thousands of intervals and the evaluator
	// addresses it once per window it judges.
	index map[int64]int
}

// At returns one metric's value for one interval.
func (s *TimeSeries) At(metric, interval int) (float64, bool) {
	if metric < 0 || metric >= len(s.Values) {
		return 0, false
	}
	if interval < 0 || interval >= len(s.Values[metric]) {
		return 0, false
	}
	return s.Values[metric][interval], true
}

// IndexOf returns the position of the interval starting at t, or -1.
func (s *TimeSeries) IndexOf(t time.Time) int {
	if s.index == nil {
		s.index = make(map[int64]int, len(s.Intervals))
		for i, iv := range s.Intervals {
			s.index[iv.Unix()] = i
		}
	}
	if i, ok := s.index[t.Unix()]; ok {
		return i
	}
	return -1
}

// Step reports the spacing between intervals, or 0 when the series is too short
// to tell. The API may coarsen a grouping it considers too fine for the range,
// and a series read at the wrong resolution would silently produce nonsense.
func (s *TimeSeries) Step() time.Duration {
	if len(s.Intervals) < 2 {
		return 0
	}
	return s.Intervals[1].Sub(s.Intervals[0])
}

// byTimeResponse mirrors the endpoint's payload. Unlike the table endpoint,
// bytime nests metrics one level deeper: each metric carries a slice of
// per-interval values rather than a single number.
type byTimeResponse struct {
	Totals        [][]float64 `json:"totals"`
	TimeIntervals [][]string  `json:"time_intervals"`
	Sampled       bool        `json:"sampled"`
	Data          []struct {
		Metrics [][]float64 `json:"metrics"`
	} `json:"data"`
}

// FetchByTime requests a time series at the given grouping.
//
// Query.Date1/Date2 accept the API's own formats, so a caller can pass either
// YYYY-MM-DD or a relative keyword, and Query.Filters narrows the report.
func (c *ReportClient) FetchByTime(ctx context.Context, q Query, group string) (*TimeSeries, error) {
	if len(q.Metrics) == 0 {
		return nil, fmt.Errorf("time series needs at least one metric")
	}
	if len(q.Metrics) > 20 {
		return nil, fmt.Errorf("time series has %d metrics, the API allows 20", len(q.Metrics))
	}

	params := q.params(c.counterID)
	if group == "" {
		group = GroupHour
	}
	params.Set("group", group)

	var resp byTimeResponse
	if err := c.tr.doJSON(ctx, http.MethodGet, byTimePath, params, &resp); err != nil {
		return nil, fmt.Errorf("fetch time series: %w", err)
	}

	series := &TimeSeries{Sampled: resp.Sampled}

	// Prefer totals: with no dimensions requested it carries the whole series,
	// and it is present whether or not the period produced grouped rows.
	series.Values = resp.Totals
	if len(series.Values) == 0 && len(resp.Data) > 0 {
		series.Values = resp.Data[0].Metrics
	}

	series.Intervals = parseTimeIntervals(resp.TimeIntervals)
	if len(series.Intervals) == 0 {
		// The field is not in the published schema, so the intervals are
		// reconstructed from the request when the API omits them.
		series.Intervals = deriveIntervals(q.Date1, q.Date2, groupStep(group), seriesLength(series.Values))
	}
	return series, nil
}

// groupStep is the interval width a grouping produces.
func groupStep(group string) time.Duration {
	if group == GroupTenMinutes {
		return 10 * time.Minute
	}
	return time.Hour
}

// seriesLength reports the longest metric series in the response.
func seriesLength(values [][]float64) int {
	longest := 0
	for _, v := range values {
		if len(v) > longest {
			longest = len(v)
		}
	}
	return longest
}

// intervalLayouts are the forms a time_intervals boundary is observed in.
var intervalLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseTimeIntervals reads the start of each interval. Values are in the
// counter's own time zone and carry no offset, so they are read as local time —
// the same zone the engine buckets hours and weekdays in.
func parseTimeIntervals(raw [][]string) []time.Time {
	out := make([]time.Time, 0, len(raw))
	for _, pair := range raw {
		if len(pair) == 0 {
			continue
		}
		t, ok := parseInterval(pair[0])
		if !ok {
			return nil
		}
		out = append(out, t)
	}
	return out
}

func parseInterval(s string) (time.Time, bool) {
	for _, layout := range intervalLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// deriveIntervals reconstructs interval starts for a period the caller
// expressed as concrete dates. Relative keywords cannot be reconstructed, so
// they yield nothing and the caller must rely on the API's own intervals.
func deriveIntervals(date1, date2 string, step time.Duration, count int) []time.Time {
	if count == 0 {
		return nil
	}
	start, err := time.ParseInLocation("2006-01-02", date1, time.Local)
	if err != nil {
		return nil
	}
	if _, err := time.ParseInLocation("2006-01-02", date2, time.Local); err != nil {
		return nil
	}

	out := make([]time.Time, count)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * step)
	}
	return out
}

// Lookup answers read-only questions about a counter's configuration in
// Metrika, building a client per counter from its own stored token.
type Lookup struct {
	cfg *MetrikaConfig
}

func NewLookup(cfg *MetrikaConfig) *Lookup { return &Lookup{cfg: cfg} }

// Goals lists the counter's conversion goals.
func (l *Lookup) Goals(ctx context.Context, counter *model.Counter) ([]Goal, error) {
	return NewReportClient(counter, l.cfg.BaseURL).Goals(ctx)
}
