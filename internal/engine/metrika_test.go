package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A 403 from a bad token should name the reason, not surface as a bare status.
func TestAPIErrorCarriesMetrikaDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errors":[{"error_type":"access_denied","message":"No access to counter"}],"code":403,"message":"Access denied"}`)
	}))
	defer srv.Close()

	c := NewReportClient(testCounter(), srv.URL)
	_, err := c.Goals(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not an *APIError: %v", err)
	}
	if apiErr.StatusCode != 403 {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	for _, want := range []string{"Access denied", "access_denied", "No access to counter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err.Error(), want)
		}
	}
}

// Quota and server faults are worth another attempt; a malformed field list is not.
func TestRetriesQuotaAndServerFaults(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"Quota exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"goals":[{"id":42,"name":"Покупка"}]}`)
	}))
	defer srv.Close()

	c := NewReportClient(testCounter(), srv.URL)
	goals, err := c.Goals(context.Background())
	if err != nil {
		t.Fatalf("Goals after a 429: %v", err)
	}
	if len(goals) != 1 {
		t.Errorf("goals = %+v", goals)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("made %d calls, want 2 (one retry)", n)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"Field ym:pv:nope does not exist"}`)
	}))
	defer srv.Close()

	c := NewReportClient(testCounter(), srv.URL)
	if _, err := c.Goals(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	// Retrying a request the API will never accept only wastes quota.
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d calls, want 1 — a 400 must not be retried", n)
	}
}

func TestCancelledContextStopsImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewReportClient(testCounter(), srv.URL)
	if _, err := c.Goals(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestDefaultHostIsTheDocumentedOne(t *testing.T) {
	tr := newTransport("token", "")
	if tr.baseURL != DefaultAPIHost {
		t.Errorf("baseURL = %q, want %q", tr.baseURL, DefaultAPIHost)
	}
	if !strings.HasSuffix(DefaultAPIHost, ".net") {
		t.Errorf("the documented API host is api-metrika.yandex.net, got %q", DefaultAPIHost)
	}
}
