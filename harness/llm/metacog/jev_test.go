package metacog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestJevScoreRequestShapeAndOrder(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing bearer auth")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		path := body["state"].(map[string]any)["path"].(string)
		var noul float64
		if _, err := fmt.Sscanf(path, "%f", &noul); err != nil {
			noul = 0
		}
		time.Sleep(5 * time.Millisecond) // make concurrency observable
		fmt.Fprintf(w, `{"answers":{"is_correct":{"noul":%f}}}`, noul)
	}))
	defer server.Close()

	jev := NewJev(JevConfig{BaseURL: server.URL, APIKey: "k", Model: "jev-latest", Concurrency: 8})
	cands := []string{"0.1", "0.7", "0.3", "0.9"}
	scores, err := jev.Score(context.Background(), "the problem", cands)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.1, 0.7, 0.3, 0.9}
	for i := range want {
		if scores[i] != want[i] {
			t.Fatalf("scores[%d]=%v, want %v (order must match candidates)", i, scores[i], want[i])
		}
	}
	if len(bodies) != 4 {
		t.Fatalf("requests = %d, want 4", len(bodies))
	}
	for _, body := range bodies {
		if body["model"] != "jev-latest" {
			t.Errorf("model = %v", body["model"])
		}
		state := body["state"].(map[string]any)
		if state["problem"] != "the problem" {
			t.Errorf("problem = %v", state["problem"])
		}
		q := body["questions"].(map[string]any)["is_correct"].(map[string]any)
		if q["type"] != "noul" || q["instructions"] != DefaultScoreInstructions {
			t.Errorf("question = %v", q)
		}
	}
}

func TestJevTruncatesPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		gotPath = body["state"].(map[string]any)["path"].(string)
		fmt.Fprint(w, `{"answers":{"is_correct":{"noul":0.5}}}`)
	}))
	defer server.Close()
	jev := NewJev(JevConfig{BaseURL: server.URL, MaxCharsPerPath: 10})
	if _, err := jev.Score(context.Background(), "p", []string{"0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	// keeps the LAST 10 runes, prefixed with an ellipsis
	if gotPath != "…6789abcdef" {
		t.Fatalf("path = %q, want %q", gotPath, "…6789abcdef")
	}
}
