package responsesapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestAdapterRetryConfiguration(t *testing.T) {
	for _, test := range []struct {
		name     string
		limit    *int
		attempts int
	}{
		{name: "default", attempts: 5},
		{name: "disabled", limit: new(1), attempts: 1},
		{name: "custom", limit: new(2), attempts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestAdapterWithConfig(t, Config{Endpoint: "https://example.com/responses", MaxAttempts: test.limit}).(*adapter)
			policy := client.remoteRequest(nil, "").RetryPolicy
			if client.maxAttempts != test.attempts || policy.MaxAttempts != 1 {
				t.Fatalf("exchange attempts = %d, request default attempts = %d", client.maxAttempts, policy.MaxAttempts)
			}
		})
	}
}

func TestProviderErrorPreservesNonstandardStatusCode(t *testing.T) {
	for _, statusCode := range []int{520, 521, 522, 523, 524, 529} {
		t.Run(strconv.Itoa(statusCode), func(t *testing.T) {
			err := providerError(statusCode, nil)
			if err.StatusCode != statusCode || !strings.Contains(err.Error(), "status "+strconv.Itoa(statusCode)) {
				t.Fatalf("error = %#v, message = %q", err, err.Error())
			}
		})
	}
}

func TestAdapterRejectsInvalidMaxAttempts(t *testing.T) {
	remote := primitives.NewRemoteClient()
	t.Cleanup(func() {
		if err := remote.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, limit := range []int{-1, 0} {
		client, err := NewAdapter(remote, Config{Endpoint: "https://example.com/responses", MaxAttempts: &limit})
		if err == nil || client != nil {
			t.Fatalf("max attempts %d: client, error = (%v, %v)", limit, client, err)
		}
	}
}

func TestAdapterRetriesTransientFailures(t *testing.T) {
	for _, failure := range []string{"status", "connection", "partial response"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int64
			bodies := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					return
				}
				bodies <- string(body)
				if attempts.Add(1) == 1 {
					switch failure {
					case "status":
						writer.WriteHeader(http.StatusBadGateway)
						if _, err := io.WriteString(writer, "bad gateway"); err != nil {
							t.Error(err)
						}
					case "connection":
						connection, _, err := http.NewResponseController(writer).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						if err := connection.Close(); err != nil {
							t.Error(err)
						}
					case "partial response":
						writer.Header().Set("Content-Type", "text/event-stream")
						writer.Header().Set("Content-Length", "4096")
						if _, err := io.WriteString(writer, `data: {"type":"response.created","response":{"id":"discard-this-partial-response",`); err != nil {
							t.Error(err)
						}
					}
					return
				}
				writeStreamResponse(t, writer, `{"id":"recovered","status":"completed","output":[],"usage":{}}`)
			}))
			defer server.Close()
			client := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(2)})
			response, err := client.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if response.ID != "recovered" || attempts.Load() != 2 {
				t.Fatalf("response = %#v, attempts = %d", response, attempts.Load())
			}
			if first, second := <-bodies, <-bodies; first != second {
				t.Fatalf("retry body differs: %s != %s", first, second)
			}
		})
	}
}

func TestAdapterBoundsRetries(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				writer.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			client := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: &limit})
			_, err := client.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("error = %v", err)
			}
			if got := attempts.Load(); got != int64(limit) {
				t.Fatalf("attempts = %d, want %d", got, limit)
			}
		})
	}
}

func TestAdapterDoesNotRetryMissingRequiredParameter(t *testing.T) {
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
		if _, err := io.WriteString(writer, `{"error":{"code":"missing_required_parameter","param":"model","message":"Missing required parameter: 'model'."}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := newTestAdapter(t, server.URL)
	_, err := client.Respond(t.Context(), validRequest(), llm.RequestOptions{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Code != "missing_required_parameter" || attempts.Load() != 1 {
		t.Fatalf("error = %v, attempts = %d", err, attempts.Load())
	}
}

func TestDoesNotRetryTerminalInBandErrors(t *testing.T) {
	// A 200 stream that carries an in-band terminal failure (e.g. credit_balance_exhausted,
	// nested under "error") must surface the error and NOT be retried.
	for _, c := range []struct {
		name string
		err  *APIError
	}{
		{"credit_balance_exhausted code", &APIError{StatusCode: 200, Code: "credit_balance_exhausted", Message: "You have no credits remaining.", Type: "insufficient_quota"}},
		{"insufficient_quota type", &APIError{StatusCode: 200, Message: "quota", Type: "insufficient_quota"}},
	} {
		if retryableResponseError(c.err, nil) {
			t.Fatalf("%s: expected non-retryable, got retryable", c.name)
		}
	}
}
