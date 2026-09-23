package openaicodex

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/responsesapi"
)

func TestClientUsesSubscriptionProtocol(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/responses" || r.Method != "POST" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		for key, want := range map[string]string{"Authorization": "Bearer access-token", "ChatGPT-Account-ID": "account", "Accept": "text/event-stream", "Content-Type": "application/json", "originator": "heuristic"} {
			if r.Header.Get(key) != want {
				t.Errorf("incorrect %s header", key)
			}
		}
		var body map[string]any
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		if body["model"] != "gpt-test" || body["stream"] != true || body["store"] != false {
			t.Errorf("body = %#v", body)
		}
		input, ok := body["input"].([]any)
		if !ok || len(input) != 2 {
			t.Error("missing conversation input")
			return
		}
		if first := input[0].(map[string]any); first["role"] != "system" || first["content"] != "Our own instructions" {
			t.Error("system message was changed")
		}
		if body["prompt_cache_key"] == nil || body["prompt_cache_key"] != r.Header.Get("session-id") {
			t.Error("cache affinity missing")
		}
		if r.Header.Get("session_id") != "" {
			t.Error("obsolete session_id header sent")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Config{AccessToken: "access-token", AccountID: "account", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{Model: llm.Model{ID: "gpt-test"}, Input: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "Our own instructions"}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hello"}},
	}}
	response, err := client.Respond(t.Context(), request, llm.RequestOptions{CacheKey: "session"})
	if err != nil || response.ID != "r" || response.Stop != llm.StopComplete || requests.Load() != 1 {
		t.Fatalf("response = %#v, %v", response, err)
	}
}

func TestClientLoadsCredentialsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeTestAuth(t, path, "token-1", "account-1")
	seen := make(chan [2]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- [2]string{r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")}
		if r.Header.Get("session-id") == "" || r.Header.Get("session_id") != "" {
			t.Error("incorrect Codex session header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(Config{AuthFile: path, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := llm.Request{}
	check := func(client *Client, token, account string) {
		t.Helper()
		if _, err := client.Respond(t.Context(), request, llm.RequestOptions{CacheKey: "session"}); err != nil {
			t.Fatal(err)
		}
		if got := <-seen; got != [2]string{"Bearer " + token, account} {
			t.Error("unexpected credential snapshot")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(client, "token-1", "account-1")
	if err := os.WriteFile(path, []byte("invalid JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(client, "token-1", "account-1")
	writeTestAuth(t, path, "token-2", "account-2")
	check(client, "token-1", "account-1")
	updated, err := NewClient(Config{AuthFile: path, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer updated.Close()
	check(updated, "token-2", "account-2")
	check(client, "token-1", "account-1")
}

func TestClientRejectsRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewClient(Config{AccessToken: "opaque", AccountID: "account", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	var apiErr *responsesapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 307 || redirected.Load() != 0 {
		t.Fatalf("redirect error = %v, target requests = %d", err, redirected.Load())
	}
}

func TestClientUnauthorizedDoesNotFallback(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"unauthorized","code":"invalid_token"}}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{AccessToken: "opaque", AccountID: "account", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	var apiErr *responsesapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || requests.Load() != 1 || !strings.Contains(err.Error(), "renew them externally") {
		t.Fatalf("error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Respond(ctx, llm.Request{}, llm.RequestOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("canceled request sent")
	}
}

func TestClientConfigurationValidation(t *testing.T) {
	for _, url := range []string{"", BaseURL, BaseURL + "/", "http://127.0.0.1:1234/codex", "http://[::1]:1234"} {
		if _, err := validateBaseURL(url); err != nil {
			t.Errorf("rejected %q: %v", url, err)
		}
	}
	for _, url := range []string{"https://api.openai.com/v1", "http://chatgpt.com/backend-api/codex", "https://chatgpt.com.evil.example/backend-api/codex", BaseURL + "?token=bad", BaseURL + "#fragment", "http://localhost:1234", "http://user:pass@127.0.0.1:1234", "file:///tmp/auth"} {
		if _, err := validateBaseURL(url); err == nil {
			t.Errorf("accepted %q", url)
		}
	}
	for _, config := range []Config{
		{},
		{AccessToken: "opaque", AccountID: "account", MaxAttempts: new(0)},
		{AccessToken: "opaque", AccountID: "account", BaseURL: "https://evil.example"},
	} {
		if client, err := NewClient(config); err == nil || client != nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
