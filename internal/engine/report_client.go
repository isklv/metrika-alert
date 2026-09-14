package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
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
