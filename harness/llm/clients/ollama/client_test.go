package ollama

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

func TestClientUsesResponsesWithoutAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("incorrect request headers")
		}
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		if body["model"] != "local-model:tag" || body["stream"] != true || body["store"] != false || body["prompt_cache_key"] != nil {
			t.Errorf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL + "/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "local-model:tag"}}, llm.RequestOptions{CacheKey: "session"})
	if err != nil || response.ID != "r" || response.Stop != llm.StopComplete {
		t.Fatalf("response = %#v, error = %v", response, err)
	}
}
