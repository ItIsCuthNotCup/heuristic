package responsesapi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestAdapterRemoteRequestsUseUUIDCorrelationIDs(t *testing.T) {
	adapter := &adapter{endpoint: "https://example.com/responses"}
	first := adapter.remoteRequest(nil, "").CorrelationID
	second := adapter.remoteRequest(nil, "").CorrelationID

	if _, err := uuid.Parse(string(first)); err != nil {
		t.Fatalf("first correlation ID = %q: %v", first, err)
	}
	if _, err := uuid.Parse(string(second)); err != nil {
		t.Fatalf("second correlation ID = %q: %v", second, err)
	}
	if first == second {
		t.Fatalf("correlation IDs are equal: %q", first)
	}
}

func TestAdapterResponds(t *testing.T) {
	requestBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assertRequest(t, request, "text/event-stream")
		var body map[string]any
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requestBody <- body

		writeStreamResponse(t, writer, `{
			"id":"resp-1",
			"object":"response",
			"status":"completed",
			"output":[
				{"id":"message-1","type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]},
				{"id":"reasoning-1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"Used tools."}],"encrypted_content":"opaque"},
				{"id":"function-1","type":"function_call","call_id":"call-2","name":"weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}
			],
			"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":3},"output_tokens":10,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":30}
		}`)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL+"/responses")
	got, err := adapter.Respond(t.Context(), detailedRequest(), llm.RequestOptions{})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	gotRequestBody := <-requestBody
	if gotRequestBody["stream"] != true {
		t.Fatal("request must enable streaming")
	}
	assertRequestBody(t, gotRequestBody)

	want := llm.Response{
		ID:   "resp-1",
		Stop: llm.StopComplete,
		Output: []llm.Item{
			{
				ProviderID: "message-1", Type: llm.ItemMessage,
				Data: llm.Message{
					Role: llm.RoleAssistant, Text: "hello", Phase: "final_answer",
				},
			},
			{
				ProviderID: "reasoning-1",
				Type:       llm.ItemReasoning,
				Data: llm.Reasoning{
					Summary: []string{"Used tools."},
					Raw: jsontext.Value(`{"id":"reasoning-1","type":"reasoning","status":"completed",` +
						`"summary":[{"type":"summary_text","text":"Used tools."}],"encrypted_content":"opaque"}`),
				},
			},
			{
				ProviderID: "function-1",
				Type:       llm.ItemToolCall,
				Data: llm.ToolCall{
					CallID:    "call-2",
					Name:      "weather",
					Arguments: `{"city":"Paris"}`,
				},
			},
		},
		Usage: llm.Usage{
			InputTokens: 20, CachedInputTokens: 8, CacheWriteInputTokens: 3,
			OutputTokens: 10, ReasoningTokens: 4,
			Raw: jsontext.Value(`{"input_tokens":20,"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":3},` +
				`"output_tokens":10,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":30}`),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v\nwant %#v", got, want)
	}
}

func TestAdapterReturnsProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error":{"code":"invalid_request","message":"model is required","param":"model","type":"invalid_request_error"}}`)
	}))
	defer server.Close()

	adapter := newTestAdapter(t, server.URL+"/responses")
	_, err := adapter.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %#v", err)
	}
	if apiError.StatusCode != http.StatusBadRequest || apiError.Code != "invalid_request" ||
		apiError.Param != "model" || apiError.Type != "invalid_request_error" {
		t.Fatalf("API error = %#v", apiError)
	}
}

func TestRespondCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	adapter := newTestAdapter(t, server.URL+"/responses")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Respond(ctx, validRequest(), llm.RequestOptions{})
		done <- err
	}()
	waitForSignal(t, started, "provider request did not start")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provider request was not canceled")
	}
}

func TestResponseSeparatesStopReasonsFromFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want llm.Response
	}{
		{
			name: "truncated",
			body: `{"id":"resp-1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{}}}`,
			want: llm.Response{
				ID: "resp-1", Stop: llm.StopMaxOutputTokens, Output: []llm.Item{},
				Usage: llm.Usage{
					InputTokens: 2, OutputTokens: 1,
					Raw: jsontext.Value(`{"input_tokens":2,"input_tokens_details":{},"output_tokens":1,"output_tokens_details":{}}`),
				},
			},
		},
		{
			name: "refused",
			body: `{"id":"resp-2","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":0,"output_tokens_details":{}}}`,
			want: llm.Response{
				ID: "resp-2", Stop: llm.StopRefused, Output: []llm.Item{},
				Usage: llm.Usage{
					InputTokens: 2,
					Raw:         jsontext.Value(`{"input_tokens":2,"input_tokens_details":{},"output_tokens":0,"output_tokens_details":{}}`),
				},
			},
		},
		{
			name: "failed",
			body: `{"id":"resp-3","status":"failed","error":{"code":"server_error","message":"failed"},"output":[],"usage":{"input_tokens":2,"input_tokens_details":{},"output_tokens":0,"output_tokens_details":{}}}`,
			want: llm.Response{
				ID: "resp-3", Output: []llm.Item{},
				Usage: llm.Usage{
					InputTokens: 2,
					Raw:         jsontext.Value(`{"input_tokens":2,"input_tokens_details":{},"output_tokens":0,"output_tokens_details":{}}`),
				},
				Failure: &llm.Failure{Code: "server_error", Message: "failed"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeResponse([]byte(test.body))
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("response = (%#v, %v), want %#v", got, err, test.want)
			}
		})
	}
}

func TestResponseKeepsOutputWhenTruncated(t *testing.T) {
	got, err := decodeResponse([]byte(`{
		"id":"resp-1",
		"status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"id":"message-1","type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"partial","annotations":[],"logprobs":[]}]}]
	}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := llm.Response{
		ID:   "resp-1",
		Stop: llm.StopMaxOutputTokens,
		Output: []llm.Item{{
			ProviderID: "message-1",
			Type:       llm.ItemMessage,
			Data:       llm.Message{Role: llm.RoleAssistant, Text: "partial"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v\nwant %#v", got, want)
	}
}

func TestResponseReadsRefusalAsMessageText(t *testing.T) {
	got, err := decodeResponse([]byte(`{
		"id":"resp-1",
		"status":"completed",
		"output":[{"id":"message-1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I cannot help with that."}]}]
	}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := llm.Message{Role: llm.RoleAssistant, Text: "I cannot help with that."}
	if len(got.Output) != 1 || !reflect.DeepEqual(got.Output[0].Data, want) {
		t.Fatalf("output = %#v", got.Output)
	}
}

func TestResponseCarriesUnknownMessagePhase(t *testing.T) {
	got, err := decodeResponse([]byte(`{
		"id":"resp-1",
		"status":"completed",
		"output":[{"id":"message-1","type":"message","role":"assistant","status":"completed","phase":"analysis","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}]
	}`))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	message, ok := got.Output[0].Data.(llm.Message)
	if !ok || message.Phase != "analysis" {
		t.Fatalf("output = %#v", got.Output[0].Data)
	}
}

func TestResponseRejectsUnfinishedStatus(t *testing.T) {
	_, err := decodeResponse([]byte(`{"id":"resp-1","status":"queued","output":[]}`))
	if err == nil || !strings.Contains(err.Error(), `unsupported response status "queued"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestResponseRejectsUnsupportedIncompleteReason(t *testing.T) {
	_, err := decodeResponse([]byte(`{"id":"resp-1","status":"incomplete","incomplete_details":{"reason":"unknown"},"output":[]}`))
	if err == nil || !strings.Contains(err.Error(), `unsupported incomplete reason "unknown"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestResponseRejectsUnsupportedOutput(t *testing.T) {
	_, err := decodeResponse([]byte(`{
		"id":"resp-1",
		"status":"completed",
		"output":[{"type":"file_search_call","id":"search-1"}]
	}`))
	if err == nil || !strings.Contains(err.Error(), `unsupported output item type "file_search_call"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestAdapterTracesProviderExchange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeStreamResponse(t, writer, `{"id":"resp-1","status":"completed","output":[],"usage":{}}`)
	}))
	defer server.Close()

	remote := primitives.NewRemoteClient()
	t.Cleanup(func() {
		if err := remote.Close(); err != nil {
			t.Errorf("close remote client: %v", err)
		}
	})
	var traced []Exchange
	adapter, err := NewAdapter(remote, Config{
		Endpoint: server.URL + "/responses",
		Trace:    func(exchange Exchange) { traced = append(traced, exchange) },
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	if _, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{}); err != nil {
		t.Fatalf("respond: %v", err)
	}

	if len(traced) != 1 {
		t.Fatalf("traced = %#v", traced)
	}
	if traced[0].StatusCode != http.StatusOK ||
		!strings.Contains(string(traced[0].RequestBody), `"model":"gpt-test"`) ||
		!strings.Contains(string(traced[0].ResponseBody), `"id":"resp-1"`) {
		t.Fatalf("exchange = %#v", traced[0])
	}
}

func TestNewAdapterRequiresDependencies(t *testing.T) {
	adapter, err := NewAdapter(nil, Config{Endpoint: "https://example.com/responses"})
	if err == nil || adapter != nil {
		t.Fatalf("adapter, error = (%#v, %v)", adapter, err)
	}

	remote := primitives.NewRemoteClient()
	t.Cleanup(func() { _ = remote.Close() })
	adapter, err = NewAdapter(remote, Config{})
	if err == nil || adapter != nil {
		t.Fatalf("adapter, error = (%#v, %v)", adapter, err)
	}
}

func TestRequestInputRejectsUnsupportedType(t *testing.T) {
	_, err := requestInputItem(llm.Item{Type: "image"})
	if err == nil || err.Error() != `unsupported input item type "image"` {
		t.Fatalf("error = %v", err)
	}
}

func newTestAdapter(t *testing.T, endpoint string) llm.Adapter {
	t.Helper()
	return newTestAdapterWithConfig(t, Config{
		Endpoint: endpoint,
		Headers: map[string][]string{
			"Authorization": {"Bearer test-key"},
			"Content-Type":  {"application/json"},
		},
	})
}

func newTestAdapterWithConfig(t *testing.T, config Config) llm.Adapter {
	t.Helper()
	remote := primitives.NewRemoteClient()
	t.Cleanup(func() {
		if err := remote.Close(); err != nil {
			t.Errorf("close remote client: %v", err)
		}
	})
	adapter, err := NewAdapter(remote, config)
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	return adapter
}

func validRequest() llm.Request {
	return llm.Request{
		Model: llm.Model{ID: "gpt-test"},
		Input: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleUser, Text: "hello"},
		}},
	}
}

func detailedRequest() llm.Request {
	tool := llm.Tool{
		Type:        llm.ToolFunction,
		Name:        "weather",
		Description: "Get weather",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required":             []any{"city"},
			"additionalProperties": false,
		},
	}
	maxOutputTokens := int64(128)
	return llm.Request{
		Model: llm.Model{ID: "gpt-test", MaxOutputTokens: &maxOutputTokens},
		Input: []llm.Item{
			{
				Type: llm.ItemMessage,
				Data: llm.Message{Role: llm.RoleSystem, Text: "Be concise."},
			},
			{
				Type: llm.ItemMessage,
				Data: llm.Message{Role: llm.RoleUser, Text: "weather?"},
			},
			{
				Type: llm.ItemMessage,
				Data: llm.Message{Role: llm.RoleSystem, Text: "Use tools."},
			},
			{
				ProviderID: "previous-message", Type: llm.ItemMessage,
				Data: llm.Message{
					Role: llm.RoleAssistant, Text: "Checking.", Phase: "commentary",
				},
			},
			{
				ProviderID: "previous-reasoning",
				Type:       llm.ItemReasoning,
				Data: llm.Reasoning{
					Summary: []string{"Checked the request."},
					Raw: jsontext.Value(`{"id":"previous-reasoning","type":"reasoning","status":"completed",` +
						`"summary":[{"type":"summary_text","text":"Checked the request."}],` +
						`"content":[{"type":"reasoning_text","text":"verbatim"}],` +
						`"encrypted_content":"previous-opaque"}`),
				},
			},
			{
				ProviderID: "previous-tool-call",
				Type:       llm.ItemToolCall,
				Data: llm.ToolCall{
					CallID:    "call-1",
					Name:      "weather",
					Arguments: `{"city":"London"}`,
				},
			},
			{
				Type: llm.ItemToolResult,
				Data: llm.ToolResult{CallID: "call-1", Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "sunny"}}},
			},
		},
		Tools: []llm.Tool{tool},
	}
}

func assertRequest(t *testing.T, request *http.Request, accept string) {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.Path != "/responses" {
		t.Errorf("request = %s %s", request.Method, request.URL.Path)
	}
	if authorization := request.Header.Get("Authorization"); authorization != "Bearer test-key" {
		t.Errorf("authorization = %q", authorization)
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "application/json" {
		t.Errorf("content type = %q", contentType)
	}
	if got := request.Header.Get("Accept"); got != accept {
		t.Errorf("accept = %q, want %q", got, accept)
	}
}

func assertRequestBody(t *testing.T, got map[string]any) {
	t.Helper()
	wantJSON := `{
		"max_output_tokens":128,
		"stream":true,
		"store":false,
		"include":["reasoning.encrypted_content"],
		"input":[
			{"content":"Be concise.","role":"system"},
			{"content":"weather?","role":"user"},
			{"content":"Use tools.","role":"system"},
			{"content":[{"annotations":[],"logprobs":[],"text":"Checking.","type":"output_text"}],"id":"previous-message","phase":"commentary","role":"assistant","status":"completed","type":"message"},
			{"id":"previous-reasoning","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"Checked the request."}],"content":[{"type":"reasoning_text","text":"verbatim"}],"encrypted_content":"previous-opaque"},
			{"arguments":"{\"city\":\"London\"}","call_id":"call-1","name":"weather","id":"previous-tool-call","type":"function_call"},
			{"call_id":"call-1","output":[{"type":"input_text","text":"sunny"}],"type":"function_call_output"}
		],
		"model":"gpt-test",
		"tools":[{
			"description":"Get weather",
			"name":"weather",
			"parameters":{
				"type":"object",
				"properties":{"city":{"type":"string"}},
				"required":["city"],
				"additionalProperties":false
			},
			"strict":false,
			"type":"function"
		}]
	}`
	var want map[string]any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("unmarshal expected request: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got, jsontext.WithIndent("  "))
		wantJSON, _ := json.Marshal(want, jsontext.WithIndent("  "))
		t.Fatalf("request body =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
}

func TestAdapterModelRequestsUseTheModelIdleBound(t *testing.T) {
	adapter := newTestAdapter(t, "http://example.invalid/responses").(*adapter)
	request := adapter.remoteRequest(nil, "")
	if request.ResponseIdleTimeout != modelResponseIdleTimeout || request.RetryPolicy.MaxAttempts != 1 ||
		request.SSE == nil || request.SSE.MaxFrameSize != maxSSEFrameBytes ||
		request.SSE.FrameDelimiter != primitives.SSEFrameDelimiterStrip || request.Headers["Accept"][0] != "text/event-stream" {
		t.Fatalf("remote request = %#v", request)
	}
}

func TestAdapterCacheKeyPlacement(t *testing.T) {
	keys := map[string]struct{ key, want string }{
		"empty":             {},
		"session":           {key: "session-1", want: "84097828fc31a8c8d29210df48901a85de7fd013f686b17be77d1be29cb7a98b"},
		"another-session":   {key: "session-2", want: "5d9061408048c12d053925aed45333a142997f26a2cd1e0c4a87678c53a1e3ae"},
		"long":              {key: strings.Repeat("s", 300), want: "2955c7328c57ca39d0568bb930a5360e6b0e7f33931639c819d7cbfeaf0a88c7"},
		"long-distinct":     {key: strings.Repeat("s", 300) + "t", want: "39a446f53f3a89913a4cc295c285287870053e373201678b6a6401e7bbb5dfef"},
		"unicode":           {key: "会話", want: "098eb2e3728cd354d91d5ff240891cbb20e225247cda47eeb8d49686fe7bd248"},
		"header-characters": {key: "key\r\nwith\tcontrols", want: "45ffa7b66165175c09a7c0c0396d13359f51bdd60a348c910da142212717fb04"},
	}
	for name, config := range map[string]CacheKeyPlacement{
		"disabled": {},
		"body":     {UsePromptCacheKeyField: true},
		"header":   {Header: "x-cache-affinity"},
		"both":     {Header: "x-cache-affinity", UsePromptCacheKeyField: true},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body struct {
					Input []struct {
						Content string `json:"content"`
					} `json:"input"`
					PromptCacheKey *string `json:"prompt_cache_key"`
				}
				if err := json.UnmarshalRead(request.Body, &body); err != nil {
					t.Errorf("decode request: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(body.Input) != 1 {
					t.Errorf("input = %#v", body.Input)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				caseName := body.Input[0].Content
				key, ok := keys[caseName]
				if !ok {
					t.Errorf("unexpected input = %q", caseName)
				}
				wantBody, wantHeader := "", ""
				if config.UsePromptCacheKeyField {
					wantBody = key.want
				}
				if config.Header != "" {
					wantHeader = key.want
				}
				if wantBody == "" {
					if body.PromptCacheKey != nil {
						t.Errorf("%s: unexpected prompt_cache_key = %q", caseName, *body.PromptCacheKey)
					}
				} else if body.PromptCacheKey == nil || *body.PromptCacheKey != wantBody {
					t.Errorf("%s: prompt_cache_key = %v, want %q", caseName, body.PromptCacheKey, wantBody)
				}
				if got := request.Header.Get("x-cache-affinity"); got != wantHeader {
					t.Errorf("%s: affinity header = %q, want %q", caseName, got, wantHeader)
				}
				if got := request.Header.Get("X-Test"); got != "kept" {
					t.Errorf("%s: X-Test = %q, want kept", caseName, got)
				}
				writeStreamResponse(t, writer, `{"id":"resp-1","status":"completed","output":[],"usage":{}}`)
			}))
			t.Cleanup(server.Close)
			remote := primitives.NewRemoteClient()
			t.Cleanup(func() {
				if err := remote.Close(); err != nil {
					t.Errorf("close remote client: %v", err)
				}
			})
			headers := map[string][]string{"X-Test": {"kept"}}
			adapter, err := NewAdapter(remote, Config{Endpoint: server.URL, Headers: headers, CacheKeyPlacement: config})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if len(headers) != 1 || !reflect.DeepEqual(headers["X-Test"], []string{"kept"}) {
					t.Errorf("configured headers mutated: %#v", headers)
				}
			})
			for name, key := range keys {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					request := validRequest()
					request.Input = []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: name}}}
					for range 2 {
						if _, err := adapter.Respond(t.Context(), request, llm.RequestOptions{CacheKey: key.key}); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func writeStreamResponse(t *testing.T, writer http.ResponseWriter, response string) {
	t.Helper()
	body, err := json.Marshal(struct {
		Type     string         `json:"type"`
		Response jsontext.Value `json:"response"`
	}{Type: "response.completed", Response: jsontext.Value(response)})
	if err != nil {
		t.Error(err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	if _, err := io.WriteString(writer, "data: "+string(body)+"\n\n"); err != nil {
		t.Error(err)
	}
}

func TestHeaderlessHTTPErrorPreservesDetails(t *testing.T) {
	const body = `{"error":{"code":"invalid_token","message":"Token expired; renew credentials.","type":"authentication_error","param":null}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	var traced Exchange
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, Trace: func(exchange Exchange) { traced = exchange }})
	_, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
	apiError, ok := errors.AsType[*APIError](err)
	if !ok || apiError.StatusCode != 401 || apiError.Code != "invalid_token" ||
		apiError.Message != "Token expired; renew credentials." || apiError.Type != "authentication_error" {
		t.Fatalf("error details lost: %v", err)
	}
	if traced.StatusCode != 401 || string(traced.ResponseBody) != body {
		t.Fatalf("error body lost: %#v", traced)
	}
}
