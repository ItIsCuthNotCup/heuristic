package responsesapi_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/clients/fireworks"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/clients/openai"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/clients/openaicodex"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/clients/openrouter"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/responsesapi"
)

type streamingClient interface {
	llm.Adapter
	Close() error
}

func TestProvidersOnlyReturnTerminalResponse(t *testing.T) {
	constructors := map[string]func(string, func(responsesapi.Exchange)) (streamingClient, error){
		"openai": func(url string, trace func(responsesapi.Exchange)) (streamingClient, error) {
			return openai.NewClient(openai.Config{APIKey: "test-key", BaseURL: url, Trace: trace})
		},
		"openrouter": func(url string, trace func(responsesapi.Exchange)) (streamingClient, error) {
			return openrouter.NewClient(openrouter.Config{APIKey: "test-key", BaseURL: url, Trace: trace})
		},
		"fireworks": func(url string, trace func(responsesapi.Exchange)) (streamingClient, error) {
			return fireworks.NewClient(fireworks.Config{APIKey: "test-key", BaseURL: url, Trace: trace})
		},
		"openai-codex": func(url string, trace func(responsesapi.Exchange)) (streamingClient, error) {
			return openaicodex.NewClient(openaicodex.Config{AccessToken: "subscription-token", AccountID: "account", BaseURL: url, Trace: trace})
		},
	}
	for name, newClient := range constructors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			prefixSent := make(chan struct{}, 1)
			release := make(chan struct{}, 1)
			const toolCall = `{"type":"function_call","id":"fc-1","call_id":"call-1","name":"Bash","arguments":"{\"command\":\"pwd\"}","status":"completed"}`
			const response = `{"id":"r","status":"completed","output":[` + toolCall + `,{"type":"message","id":"m-1","role":"assistant","phase":"final_answer","status":"completed","content":[{"type":"output_text","text":"final text"}]}],"usage":{"input_tokens":12,"output_tokens":8,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":4}}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "text/event-stream" {
					t.Error("provider did not request SSE")
				}
				var body struct {
					Stream bool `json:"stream"`
				}
				if err := json.UnmarshalRead(r.Body, &body); err != nil || !body.Stream {
					t.Errorf("provider did not enable streaming: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial text must not escape\"}\n\n")
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", toolCall)
				w.(http.Flusher).Flush()
				prefixSent <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				w.(http.Flusher).Flush()
			}))
			defer server.Close()
			var traces atomic.Int32
			client, err := newClient(server.URL, func(exchange responsesapi.Exchange) {
				traces.Add(1)
				if exchange.StatusCode != http.StatusOK || string(exchange.ResponseBody) != response {
					t.Error("trace did not contain only the terminal response")
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			type result struct {
				response llm.Response
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := client.Respond(ctx, llm.Request{Model: llm.Model{ID: "test"}}, llm.RequestOptions{})
				done <- result{response, err}
			}()
			select {
			case <-prefixSent:
			case <-ctx.Done():
				t.Fatal("stream did not start")
			}
			select {
			case result := <-done:
				t.Fatalf("Respond returned before the terminal event: %#v", result)
			case <-time.After(50 * time.Millisecond):
			}
			if traces.Load() != 0 {
				t.Fatal("trace emitted a partial exchange")
			}
			release <- struct{}{}
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
				if got.response.Stop != llm.StopComplete || len(got.response.Output) != 2 || traces.Load() != 1 {
					t.Fatalf("response = %#v, traces = %d", got.response, traces.Load())
				}
				call := got.response.Output[0].Data.(llm.ToolCall)
				message := got.response.Output[1].Data.(llm.Message)
				if call.CallID != "call-1" || message.Text != "final text" || message.Phase != "final_answer" {
					t.Fatal("final output was changed")
				}
				if got.response.Usage.InputTokens != 12 || got.response.Usage.OutputTokens != 8 || got.response.Usage.CachedInputTokens != 5 || got.response.Usage.ReasoningTokens != 4 {
					t.Fatal("terminal usage was lost")
				}
				if strings.Contains(message.Text, "partial") {
					t.Fatal("text delta escaped into the completed response")
				}
			case <-ctx.Done():
				t.Fatal("terminal event did not finish response")
			}
		})
	}
}
