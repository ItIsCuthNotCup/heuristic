package responsesapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

func TestRetryableResponseErrorCodes(t *testing.T) {
	policy := primitives.DefaultRemoteRequest("test", "test", "http://example.invalid").RetryPolicy
	for _, test := range []struct {
		name string
		err  *APIError
		want bool
	}{
		{name: "no error"},
		{name: "top-level server error", err: &APIError{StatusCode: 200, Type: "error", Code: "server_error"}, want: true},
		{name: "nested rate limit", err: &APIError{StatusCode: 200, Code: "rate_limit_exceeded"}, want: true},
		{name: "nested overload", err: &APIError{StatusCode: 200, Code: "server_is_overloaded"}, want: true},
		{name: "nested slow down", err: &APIError{StatusCode: 200, Code: "slow_down"}, want: true},
		{name: "HTTP overload", err: &APIError{StatusCode: 503, Code: "server_is_overloaded"}, want: true},
		{name: "HTTP transient status", err: &APIError{StatusCode: 429, Code: "unknown"}, want: true},
		{name: "HTTP nonstandard status", err: &APIError{StatusCode: 529}, want: true},
		{name: "unknown stream code", err: &APIError{StatusCode: 200, Code: "unknown"}, want: true},
		{name: "numeric stream code", err: &APIError{StatusCode: 200, Code: "429"}, want: true},
		{name: "in-band rate limit labelled invalid request (Fireworks)", err: &APIError{StatusCode: 200, Type: "invalid_request_error", Code: "invalid_request_error", Message: "rate limit exceeded, please try again later"}, want: true},
		{name: "in-band final code keeps its label", err: &APIError{StatusCode: 200, Type: "invalid_request_error", Code: "context_length_exceeded"}},
		{name: "HTTP bad request stays final", err: &APIError{StatusCode: 400, Type: "invalid_request_error", Code: "invalid_request_error"}},
		{name: "quota overrides HTTP status", err: &APIError{StatusCode: 429, Code: "insufficient_quota"}},
		{name: "policy overrides HTTP status", err: &APIError{StatusCode: 503, Code: "bio_policy"}},
		{name: "bad request type is not trusted in-band", err: &APIError{StatusCode: 200, Code: "server_error", Type: "invalid_request_error"}, want: true},
		{name: "authentication type", err: &APIError{StatusCode: 503, Type: "authentication_error"}},
		{name: "permission type", err: &APIError{StatusCode: 429, Type: "permission_error"}},
		{name: "unauthorized status", err: &APIError{StatusCode: 401, Code: "rate_limit_exceeded"}},
		{name: "redirect", err: &APIError{StatusCode: 307}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableResponseError(test.err, policy.RetryableStatusCodes); got != test.want {
				t.Fatalf("retryable = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRetryAfterMessage(t *testing.T) {
	for _, test := range []struct {
		message string
		want    time.Duration
	}{
		{"Please try again in 11.054s. Visit the docs.", 11054 * time.Millisecond},
		{"Please try again in 28ms.", 28 * time.Millisecond},
		{"Please try again in 1.5 milliseconds.", 1500 * time.Microsecond},
		{"TRY AGAIN IN 35 SECONDS.", 35 * time.Second},
		{"try again in 1 second", time.Second},
		{"try again in 0s", 0},
		{"try again in -1s", 0},
		{"try again in NaNs", 0},
		{"try again in 999999999999999999999s", 0},
		{"try again in 1 minute", 0},
		{"try again in 12snowflakes", 0},
		{"try again in 2", 0},
		{"try again in tomorrow", 0},
		{"quota exhausted", 0},
	} {
		t.Run(test.message, func(t *testing.T) {
			if got := retryAfterMessage(test.message); got != test.want {
				t.Fatalf("delay = %s, want %s", got, test.want)
			}
		})
	}
}

func TestRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{" 120 ", 2 * time.Minute},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"0", 0},
		{"-1", 0},
		{"1.5", 0},
		{"9223372036", 9223372036 * time.Second},
		{"9223372037", 0},
		{"18446744073709551615", 0},
		{"999999999999999999999", 0},
		{"garbage", 0},
		{"", 0},
	} {
		t.Run(test.value, func(t *testing.T) {
			if got := retryAfterHeader(test.value, now); got != test.want {
				t.Fatalf("delay = %s, want %s", got, test.want)
			}
		})
	}
}

func TestResponseRetryDelay(t *testing.T) {
	policy := primitives.DefaultRemoteRequest("test", "test", "http://example.invalid").RetryPolicy
	for _, test := range []struct {
		name    string
		attempt int
		code    string
		message string
		header  string
		want    time.Duration
	}{
		{name: "initial", attempt: 1, want: 2 * time.Second},
		{name: "exponential", attempt: 3, want: 8 * time.Second},
		{name: "capped", attempt: 100, want: 30 * time.Second},
		{name: "overloaded", attempt: 1, code: "server_is_overloaded", want: 10 * time.Second},
		{name: "slow down", attempt: 2, code: "slow_down", want: 20 * time.Second},
		{name: "overload capped", attempt: 100, code: "slow_down", want: time.Minute},
		{name: "message hint capped", attempt: 1, code: "rate_limit_exceeded", message: "try again in 3600s", want: 30 * time.Second},
		{name: "header hint capped", attempt: 1, header: "86400", want: 30 * time.Second},
		{name: "short hint", attempt: 1, code: "rate_limit_exceeded", message: "try again in 28ms", want: 28 * time.Millisecond},
		{name: "longer hint wins", attempt: 1, code: "rate_limit_exceeded", message: "try again in 20s", header: "10", want: 20 * time.Second},
		{name: "header beats message", attempt: 1, code: "rate_limit_exceeded", message: "try again in 10s", header: "20", want: 20 * time.Second},
		{name: "invalid header uses message", attempt: 1, code: "rate_limit_exceeded", message: "try again in 5s", header: "invalid", want: 5 * time.Second},
		{name: "ignore non-rate-limit message", attempt: 1, code: "server_error", message: "try again in 120s", want: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &APIError{Code: test.code, Message: test.message}
			headers := http.Header{"Retry-After": {test.header}}
			got := responseRetryDelay(policy, test.attempt, err, headers, time.Now(), 0)
			if got != test.want {
				t.Fatalf("delay = %s, want %s", got, test.want)
			}
			for _, jitter := range []float64{0.25, 0.5, 0.99} {
				delay := responseRetryDelay(policy, test.attempt, err, headers, time.Now(), jitter)
				if delay < test.want*4/5 || delay > test.want {
					t.Fatalf("jittered delay = %s", delay)
				}
				if (test.header != "" || test.code == "rate_limit_exceeded") && delay != got {
					t.Fatal("jitter shortened the server hint")
				}
			}
		})
	}
}
