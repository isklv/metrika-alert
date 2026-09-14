package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Yandex.Metrika serves both APIs this package uses from one host:
//
//	Reporting API  GET  /stat/v1/data                       — aggregated, near real time
//	Logs API       POST /management/v1/counter/{id}/logrequests — raw events, asynchronous
//
// Note the .net domain: api-metrika.yandex.ru is not the documented API host.
const (
	DefaultAPIHost = "https://api-metrika.yandex.net"

	statPath       = "/stat/v1/data"
	managementPath = "/management/v1/counter/"
)

// transport performs authenticated Metrika calls and decodes their responses.
// Both API clients share it so that authentication, error envelopes, rate-limit
// handling and retries are implemented once.
type transport struct {
	token   string
	baseURL string
	http    *http.Client
}

func newTransport(token, baseURL string) *transport {
	if baseURL == "" {
		baseURL = DefaultAPIHost
	}
	return &transport{
		token:   token,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		// Log request parts can be large; the download path streams, but a slow
		// counter still needs a generous ceiling.
		http: &http.Client{Timeout: 5 * time.Minute},
	}
}

// APIError is a structured Metrika error response.
type APIError struct {
	StatusCode int
	Code       int
	Message    string
	Errors     []string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if len(e.Errors) > 0 {
		msg += ": " + strings.Join(e.Errors, "; ")
	}
	return fmt.Sprintf("metrika API %d: %s", e.StatusCode, msg)
}

// AccessDenied reports whether the API refused on credentials rather than on
// the request itself.
func (e *APIError) AccessDenied() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsAccessDenied reports whether err is the API refusing on credentials.
// Callers use it to tell "your token lacks this permission" apart from every
// other failure — blaming the token for, say, an unreadable response sends
// people to fix something that was never wrong.
func IsAccessDenied(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.AccessDenied()
}

// Retryable reports whether repeating the request could succeed. Quota and
// server-side failures are worth another attempt; a bad token or a malformed
// field list never is.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// errorEnvelope is the JSON body Metrika returns for a failed call.
type errorEnvelope struct {
	Errors []struct {
		ErrorType string `json:"error_type"`
		Message   string `json:"message"`
	} `json:"errors"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// maxAttempts bounds retries of a single call.
const maxAttempts = 3

// do performs one authenticated request, retrying transient failures. The
// caller receives the response body still open and must close it.
func (t *transport) do(ctx context.Context, method, path string, params url.Values) (*http.Response, error) {
	endpoint := t.baseURL + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// Back off before retrying, but never past the caller's deadline.
			delay := time.Duration(attempt-1) * 2 * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("build request %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "OAuth "+t.token)
		req.Header.Set("Accept", "application/json")

		resp, err := t.http.Do(req)
		if err != nil {
			// A cancelled context is the caller's decision, not a transient fault.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("request %s %s: %w", method, path, err)
			continue
		}
		if resp.StatusCode < 400 {
			return resp, nil
		}

		apiErr := parseAPIError(resp)
		resp.Body.Close()
		if !apiErr.Retryable() {
			return nil, apiErr
		}
		lastErr = apiErr
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// doJSON performs a request and decodes a JSON response into dst.
func (t *transport) doJSON(ctx context.Context, method, path string, params url.Values, dst any) error {
	resp, err := t.do(ctx, method, path, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if dst == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

// parseAPIError turns a failed response into an APIError, preserving whatever
// detail Metrika supplied — "field ym:pv:foo does not exist" is far more useful
// than a bare 400.
func parseAPIError(resp *http.Response) *APIError {
	apiErr := &APIError{StatusCode: resp.StatusCode}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(body) == 0 {
		return apiErr
	}

	var env errorEnvelope
	if json.Unmarshal(body, &env) == nil && (env.Message != "" || len(env.Errors) > 0) {
		apiErr.Code = env.Code
		apiErr.Message = env.Message
		for _, e := range env.Errors {
			detail := e.Message
			if e.ErrorType != "" {
				detail = e.ErrorType + " — " + detail
			}
			apiErr.Errors = append(apiErr.Errors, detail)
		}
		return apiErr
	}

	apiErr.Message = strings.TrimSpace(string(body))
	if len(apiErr.Message) > 300 {
		apiErr.Message = apiErr.Message[:300] + "…"
	}
	return apiErr
}

// counterPath builds a management API path for one counter.
func counterPath(counterID string, suffix string) string {
	return managementPath + counterID + suffix
}

func formatInt(n int) string { return strconv.Itoa(n) }
