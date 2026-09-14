package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reportStub(t *testing.T, body string, capture **http.Request) *ReportClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = r.Clone(r.Context())
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return NewReportClient(testCounter(), srv.URL)
}

// The Reporting API lives at /stat/v1/data, not the /{id}/report path the old
// client invented.
func TestReportClientUsesStatEndpoint(t *testing.T) {
	var got *http.Request
	c := reportStub(t, `{"data":[],"totals":[0]}`, &got)

	if _, err := c.Fetch(context.Background(), Query{
		Metrics: []string{MetricVisits, MetricUsers},
		Date1:   "today",
		Date2:   "today",
	}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got.URL.Path != "/stat/v1/data" {
		t.Errorf("path = %q, want /stat/v1/data", got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("ids") != "12345678" {
		t.Errorf("ids = %q", q.Get("ids"))
	}
	if q.Get("metrics") != "ym:s:visits,ym:s:users" {
		t.Errorf("metrics = %q", q.Get("metrics"))
	}
	if q.Get("date1") != "today" || q.Get("date2") != "today" {
		t.Errorf("dates = %q → %q", q.Get("date1"), q.Get("date2"))
	}
	// A threshold compared against a sampled estimate is not a threshold.
	if q.Get("accuracy") != "full" {
		t.Errorf("accuracy = %q, want full", q.Get("accuracy"))
	}
}

func TestReportClientParsesTotalsAndRows(t *testing.T) {
	body := `{
		"data":[
			{"dimensions":[{"name":"/checkout"}],"metrics":[120,95]},
			{"dimensions":[{"name":"/cart"}],"metrics":[80,61]}
		],
		"totals":[200,156],
		"total_rows":2,
		"sampled":true
	}`
	c := reportStub(t, body, nil)

	res, err := c.Fetch(context.Background(), Query{Metrics: []string{MetricVisits, MetricUsers}})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if res.Total(0) != 200 || res.Total(1) != 156 {
		t.Errorf("totals = %v", res.Totals)
	}
	// Out-of-range reads must not panic on a short response.
	if res.Total(9) != 0 {
		t.Errorf("Total(9) = %v, want 0", res.Total(9))
	}
	if len(res.Data) != 2 {
		t.Fatalf("got %d rows", len(res.Data))
	}
	if got := res.Data[0].Dimension(0, "name"); got != "/checkout" {
		t.Errorf("dimension = %q", got)
	}
	if res.Data[0].Metrics[0] != 120 {
		t.Errorf("row metric = %v", res.Data[0].Metrics[0])
	}
	if !res.Sampled {
		t.Error("sampled flag was lost")
	}
}

func TestReportClientRejectsOversizedQuery(t *testing.T) {
	c := reportStub(t, `{}`, nil)

	if _, err := c.Fetch(context.Background(), Query{}); err == nil {
		t.Error("expected an error for a query with no metrics")
	}

	metrics := make([]string, 21)
	for i := range metrics {
		metrics[i] = MetricVisits
	}
	// The API caps a request at 20 metrics; catching it here names the problem.
	if _, err := c.Fetch(context.Background(), Query{Metrics: metrics}); err == nil {
		t.Error("expected an error for 21 metrics")
	}
}

func TestGoalReachesMetric(t *testing.T) {
	if got := GoalReachesMetric(42); got != "ym:s:goal42reaches" {
		t.Errorf("GoalReachesMetric(42) = %q", got)
	}
}

func TestGoalsList(t *testing.T) {
	var got *http.Request
	c := reportStub(t, `{"goals":[{"id":42,"name":"Покупка","type":"action"},{"id":77,"name":"Регистрация"}]}`, &got)

	goals, err := c.Goals(context.Background())
	if err != nil {
		t.Fatalf("Goals: %v", err)
	}
	if got.URL.Path != "/management/v1/counter/12345678/goals" {
		t.Errorf("path = %q", got.URL.Path)
	}
	if len(goals) != 2 || goals[0].ID != 42 || goals[0].Name != "Покупка" {
		t.Errorf("goals = %+v", goals)
	}
}

func TestResultEmpty(t *testing.T) {
	c := reportStub(t, `{"data":[],"totals":[]}`, nil)
	res, err := c.Fetch(context.Background(), Query{Metrics: []string{MetricVisits}})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.Empty() {
		t.Error("a response with no data or totals should read as empty")
	}
}

func TestSummaryMetricsOrderMatchesAccessors(t *testing.T) {
	// summaryFrom reads totals positionally, so the request order is load-bearing.
	want := []string{
		MetricVisits, MetricUsers, MetricPageviews,
		MetricBounceRate, MetricPageDepth, MetricAvgDuration, MetricGoalReaches,
	}
	if strings.Join(SummaryMetrics, ",") != strings.Join(want, ",") {
		t.Errorf("SummaryMetrics = %v", SummaryMetrics)
	}
}
