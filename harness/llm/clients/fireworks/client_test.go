package fireworks

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestNewClientRequiresAPIKey(t *testing.T) {
	client, err := NewClient(Config{APIKey: " "})
	if err == nil || err.Error() != "fireworks API key must be set" || client != nil {
		t.Fatalf("client, error = (%#v, %v)", client, err)
	}
}

func TestNewClientRequiresBaseURL(t *testing.T) {
	client, err := NewClient(Config{APIKey: "test-key", BaseURL: " "})
	if err == nil || err.Error() != "fireworks base URL must be set" || client != nil {
		t.Fatalf("client, error = (%#v, %v)", client, err)
	}
}

func TestClientCallsResponsesAPI(t *testing.T) {
	const cacheKey = "84097828fc31a8c8d29210df48901a85de7fd013f686b17be77d1be29cb7a98b"
	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer test-key" {
			t.Errorf("authorization = %q", authorization)
		}
		if accept := request.Header.Get("Accept"); accept != "text/event-stream" {
			t.Errorf("accept = %q", accept)
		}
		var body struct {
			PromptCacheKey *string `json:"prompt_cache_key"`
			Model          string  `json:"model"`
			Store          *bool   `json:"store"`
			Stream         *bool   `json:"stream"`
		}
		if err := json.UnmarshalRead(request.Body, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Model != "accounts/fireworks/models/glm-5p3-flash" || body.Store == nil || *body.Store || (body.Stream == nil || !*body.Stream) {
			t.Errorf("body = %#v", body)
		}
		if got := request.Header.Get("x-session-affinity"); got != cacheKey || body.PromptCacheKey != nil {
			t.Errorf("cache placement: header = %q, body = %v", got, body.PromptCacheKey)
		}
		requestSeen <- struct{}{}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + `{"id":"resp-1","status":"completed","output":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":5}}}` + "}\n\n"))
	}))
	defer server.Close()

	client, err := NewClient(Config{APIKey: "test-key", BaseURL: server.URL + "/"})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})
	request := llm.Request{
		Model: llm.Model{ID: "accounts/fireworks/models/glm-5p3-flash"},
		Input: []llm.Item{{
			Type: llm.ItemMessage,
			Data: llm.Message{Role: llm.RoleUser, Text: "hello"},
		}},
	}
	response, err := client.Respond(t.Context(), request, llm.RequestOptions{CacheKey: "session-1"})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	<-requestSeen
	if response.ID != "resp-1" || response.Stop != llm.StopComplete ||
		response.Usage.InputTokens != 11 || response.Usage.CachedInputTokens != 3 ||
		response.Usage.OutputTokens != 7 || response.Usage.ReasoningTokens != 5 {
		t.Fatalf("response = %#v", response)
	}
}
