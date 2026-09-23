package responsesapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
)

func TestExchangeRetriesHeaderTimeout(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		writeStreamResponse(t, w, `{"id":"recovered","status":"completed","output":[]}`)
	}))
	defer server.Close()
	defer close(release)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	remote := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: transport})
	defer remote.Close()
	adapter, err := NewAdapter(remote, Config{Endpoint: server.URL, MaxAttempts: new(2)})
	if err != nil {
		t.Fatal(err)
	}
	response, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
	if err != nil || response.ID != "recovered" || calls.Load() != 2 {
		t.Fatalf("response=%#v error=%v calls=%d", response, err, calls.Load())
	}
}

func TestExchangeBoundsConnectionFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		connection, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(2)})
	_, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{})
	if err == nil || !strings.Contains(err.Error(), "EOF") || calls.Load() != 2 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}
