package responsesapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

const completedResponse = `{"id":"resp-1","status":"completed","output":[{"id":"fc-1","type":"function_call","call_id":"call-1","name":"Bash","arguments":"{\"command\":\"pwd\"}","status":"completed"},{"id":"rs-1","type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"Check directory"}]}],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":5},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":4}}}`

func TestResponsesStreamingReturnsTerminalResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Error("not requesting SSE")
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, ": keepalive\r\n\r\nevent: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"not authoritative\"}\r\n\r\n")
		event := "event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":" + completedResponse + "}\r\n\r\n"
		for i := 0; i < len(event); i += 13 {
			_, _ = io.WriteString(w, event[i:min(i+13, len(event))])
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	var traced Exchange
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, Trace: func(e Exchange) { traced = e }})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := adapter.Respond(ctx, validRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeResponse([]byte(completedResponse))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want %#v", got, want)
	}
	if string(traced.ResponseBody) != completedResponse || traced.StatusCode != 200 {
		t.Fatal("trace did not contain terminal response")
	}
}

func TestResponsesStreamFailures(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body, match string
		status                         int
		code                           string
	}{
		{name: "empty", contentType: "text/event-stream", match: "without a terminal"},
		{name: "done only", contentType: "text/event-stream", body: "data: [DONE]\n\n", match: "without a terminal"},
		{name: "truncated frame", contentType: "text/event-stream", body: `data: {"type":"response.completed","response":` + completedResponse + `}`, match: "without a terminal"},
		{name: "invalid JSON", contentType: "text/event-stream", body: "data: {bad}\n\n", match: "invalid Responses"},
		{name: "wrong content type", contentType: "application/json", body: completedResponse, match: "without a terminal response"},
		{name: "nonterminal response status", contentType: "text/event-stream", body: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"in_progress\"}}\n\n", match: "unsupported response status"},
		{name: "error event", contentType: "text/event-stream", body: "data: {\"type\":\"error\",\"code\":\"usage_limit_reached\",\"message\":\"limit reached\"}\n\n", match: "limit reached", code: "usage_limit_reached"},
		{name: "error event nested", contentType: "text/event-stream", body: "data: {\"type\":\"error\",\"error\":{\"type\":\"insufficient_quota\",\"code\":\"credit_balance_exhausted\",\"message\":\"You have no credits remaining.\"}}\n\n", match: "no credits remaining", code: "credit_balance_exhausted"},
		{name: "unauthorized", status: 401, contentType: "application/json", body: `{"error":{"code":"invalid_token","message":"expired"}}`, match: "expired", code: "invalid_token"},
		{name: "quota", status: 429, contentType: "application/json", body: `{"error":{"code":"usage_limit_reached","message":"quota"}}`, match: "quota", code: "usage_limit_reached"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(1)})
			got, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			if err == nil || !strings.Contains(err.Error(), test.match) || len(got.Output) != 0 {
				t.Fatalf("response/error = %#v, %v", got, err)
			}
			if test.code != "" {
				var apiError *APIError
				if !errors.As(err, &apiError) || apiError.Code != test.code {
					t.Fatalf("API error = %v", err)
				}
				if test.status != 0 && apiError.StatusCode != test.status {
					t.Fatalf("status = %d", apiError.StatusCode)
				}
			}
		})
	}
}

func TestResponsesTerminalStatuses(t *testing.T) {
	for _, test := range []struct {
		status, extra string
		stop          llm.StopReason
		failure       bool
	}{
		{status: "completed", stop: llm.StopComplete},
		{status: "incomplete", extra: `,"incomplete_details":{"reason":"max_output_tokens"}`, stop: llm.StopMaxOutputTokens},
		{status: "incomplete", extra: `,"incomplete_details":{"reason":"content_filter"}`, stop: llm.StopRefused},
		{status: "failed", extra: `,"error":{"code":"server_error","message":"failed"}`, failure: true},
	} {
		t.Run(test.status+string(test.stop), func(t *testing.T) {
			frame := fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"id\":\"r\",\"status\":\"%s\",\"output\":[]%s}}", test.status, test.status, test.extra)
			var state responseState
			if err := state.observe(primitives.SSEData([]byte(frame))); err != nil {
				t.Fatal(err)
			}
			body, err := state.unwrap()
			if err != nil {
				t.Fatal(err)
			}
			response, err := decodeResponse(body)
			if err != nil || response.Stop != test.stop || (response.Failure != nil) != test.failure {
				t.Fatalf("response = %#v, %v", response, err)
			}
		})
	}
}

func TestResponsesStreamRetryDiscardsPartialAttempt(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%t", disconnect), func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			bodies := make(chan string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				bodies <- string(body)
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/responses?tenant=1" {
					t.Errorf("request = %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if attempts.Add(1) == 1 {
					if disconnect {
						w.Header().Set("Content-Length", "100000")
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"old\"},\"sequence_number\":0}\n\n")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s,\"sequence_number\":1}\n\n", fallbackCall)
					return
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"new\"},\"sequence_number\":0}\n\n")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":%s}\n\n", fallbackMessage)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"new\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer server.Close()
			adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL + "/responses?tenant=1", MaxAttempts: new(2)})
			got, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			if err != nil || attempts.Load() != 2 || got.ID != "new" || len(got.Output) != 1 {
				t.Fatalf("attempts=%d response=%#v error=%v", attempts.Load(), got, err)
			}
			if got.Output[0].Data.(llm.Message).Text != "fallback" {
				t.Fatal("retried generation retained the old tool call")
			}
			if first, second := <-bodies, <-bodies; first != second {
				t.Fatal("retry changed request body")
			}
		})
	}
}

func TestResponsesStreamCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := adapter.Respond(ctx, validRequest(), llm.RequestOptions{}); done <- err }()
	waitForSignal(t, started, "request did not start")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not terminate response")
	}
}

func TestResponsesTerminalPreservesProviderUsage(t *testing.T) {
	const raw = `{"id":"r","status":"completed","output":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"provider_extension":{"cost":0.25}}}`
	var state responseState
	if err := state.observe([]byte(`{"type":"response.completed","response":` + raw + `}`)); err != nil {
		t.Fatal(err)
	}
	body, err := state.unwrap()
	if err != nil || string(body) != raw {
		t.Fatalf("terminal response = (%#v, %v)", body, err)
	}
	response, err := decodeResponse(body)
	if err != nil || !strings.Contains(string(response.Usage.Raw), `"provider_extension":{"cost":0.25}`) {
		t.Fatalf("usage = %s, error = %v", response.Usage.Raw, err)
	}
}

func TestResponsesTracesAssembledResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", fallbackMessage)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	var traces atomic.Int32
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, Trace: func(Exchange) { traces.Add(1) }})
	response, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
	if err != nil || response.ID != "r" || len(response.Output) != 1 || traces.Load() != 1 {
		t.Fatalf("response=%#v error=%v traces=%d", response, err, traces.Load())
	}
	if response.Output[0].Data.(llm.Message).Text != "fallback" {
		t.Fatal("collected output lost")
	}
}

func TestResponsesCapturesTerminalResponse(t *testing.T) {
	for _, test := range []struct{ name, prefix, trailer, errorText string }{
		{name: "EOF"},
		{name: "leading BOM", prefix: "\ufeff"},
		{name: "BOM after heartbeat", prefix: ": heartbeat\n\n\ufeff"},
		{name: "heartbeat", trailer: ": heartbeat\n\n"},
		{name: "done", trailer: "data: [DONE]\n\n: heartbeat\n\n"},
		{name: "nonterminal", trailer: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"unfinished\"}\n\n"},
		{name: "malformed before terminal", prefix: "data: {bad}\n\n", errorText: "invalid Responses stream event JSON"},
		{name: "malformed after terminal", trailer: "data: {bad}\n\n"},
		{name: "malformed followed by valid trailer", trailer: "data: {bad}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"unfinished\"}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "%sdata: {\"type\":\"response.completed\",\"response\":%s}\n\n%s", test.prefix, completedResponse, test.trailer)
			}))
			defer server.Close()
			response, err := newTestAdapter(t, server.URL).Respond(t.Context(), validRequest(), llm.RequestOptions{})
			if calls.Load() != 1 {
				t.Fatalf("requests = %d, want 1", calls.Load())
			}
			if test.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorText) || len(response.Output) != 0 {
					t.Fatalf("response/error = %#v, %v", response, err)
				}
			} else if err != nil || response.ID != "resp-1" {
				t.Fatalf("response/error = %#v, %v", response, err)
			}
		})
	}
}

func TestResponsesPreservesTerminalAfterDisconnect(t *testing.T) {
	for _, maxAttempts := range []int{1, 2} {
		t.Run(fmt.Sprint(maxAttempts), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", "100000")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", completedResponse)
			}))
			defer server.Close()
			adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: &maxAttempts})
			response, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
			if err != nil || calls.Load() != 1 {
				t.Fatalf("error=%v requests=%d", err, calls.Load())
			}
			want, err := decodeResponse([]byte(completedResponse))
			if err != nil || !reflect.DeepEqual(response, want) {
				t.Fatalf("response=%#v want=%#v error=%v", response, want, err)
			}
		})
	}
}

func TestResponseStateValidatesEnvelope(t *testing.T) {
	for _, input := range []string{
		`{"response":{}}`,
		`{"type":"","response":{}}`,
		`{"type":1,"response":{}}`,
		`{"type":"other.completed","response":{}}`,
		`{"type":"response.","response":{}}`,
		`{"type":"response.completed"}`,
		`{"type":"response.completed","response":null}`,
		`{"type":"response.completed","response":[]}`,
		`{"type":"response.completed","response":"not an object"}`,
		`{"type":"response.output_text.delta","delta":"unfinished"}`,
		`null`,
		`[]`,
	} {
		var state responseState
		if err := state.observe([]byte(input)); err != nil {
			continue
		}
		if body, err := state.unwrap(); err == nil || body != nil {
			t.Fatalf("accepted invalid envelope %s: %s, %v", input, body, err)
		}
	}
}

func TestResponseStateLeavesStatusToResponseDecoder(t *testing.T) {
	const raw = `{"id":"r","status":"in_progress","output":[]}`
	var state responseState
	if err := state.observe([]byte(`{"type":"response.completed","response":` + raw + `}`)); err != nil {
		t.Fatal(err)
	}
	body, err := state.unwrap()
	if err != nil || string(body) != raw {
		t.Fatalf("unwrap = %s, %v", body, err)
	}
	if _, err := decodeResponse(body); err == nil || !strings.Contains(err.Error(), "unsupported response status") {
		t.Fatalf("decode = %v", err)
	}
}

func TestResponseStateCapturesOnlyTerminalTypes(t *testing.T) {
	for _, test := range []struct {
		kind     string
		terminal bool
	}{
		{"response.created", false},
		{"response.queued", false},
		{"response.in_progress", false},
		{"response.completed", true},
		{"response.failed", true},
		{"response.incomplete", true},
		{"response.output_item.done", false},
		{"response.output_text.done", false},
	} {
		t.Run(test.kind, func(t *testing.T) {
			raw := fmt.Sprintf(`{"id":"r","status":%q,"output":[]}`, strings.TrimPrefix(test.kind, "response."))
			event := fmt.Sprintf(`{"type":%q,"sequence_number":7,"response":%s}`, test.kind, raw)
			if test.kind == "response.output_item.done" {
				event = fmt.Sprintf(`{"type":%q,"sequence_number":7,"output_index":0,"item":{"type":"reasoning","id":"rs","summary":[]}}`, test.kind)
			}
			var state responseState
			if err := state.observe([]byte(event)); err != nil {
				t.Fatal(err)
			}
			body, err := state.unwrap()
			if test.terminal {
				if err != nil || string(body) != raw {
					t.Fatalf("terminal response = %s, %v", body, err)
				}
			} else if state.response != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("nonterminal response captured: %s, %v", state.response, err)
			}
		})
	}
}

func TestResponseStateKeepsCapturedTerminalResponse(t *testing.T) {
	var state responseState
	if err := state.observe([]byte(`{"type":"response.completed","response":` + completedResponse + `}`)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"response.in_progress", "response.queued", "response.created", "response.output_text.delta"} {
		event := fmt.Sprintf(`{"type":%q,"response":{"id":"not-terminal","status":"in_progress"}}`, kind)
		if err := state.observe([]byte(event)); err != nil {
			t.Fatal(err)
		}
		body, err := state.unwrap()
		if err != nil || string(body) != completedResponse {
			t.Fatalf("%s overwrote terminal response: %s, %v", kind, body, err)
		}
	}
}
