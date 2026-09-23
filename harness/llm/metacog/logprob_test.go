package metacog

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestYesProbability(t *testing.T) {
	// exp(-0.2)/(exp(-0.2)+exp(-1.5))
	want := math.Exp(-0.2) / (math.Exp(-0.2) + math.Exp(-1.5))
	got := yesProbability(map[string]float64{" Yes": -0.2, "no": -1.5, "foo": -0.01})
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("yesProbability = %v, want %v", got, want)
	}
	if got := yesProbability(map[string]float64{"hello": -0.1}); got != 0.5 {
		t.Fatalf("empty family should give 0.5, got %v", got)
	}
}

func TestLogprobScore(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"choices":[{"logprobs":{"content":[{"token":"Yes","logprob":-0.2,"top_logprobs":[
			{"token":"Yes","logprob":-0.2},
			{"token":" yes","logprob":-0.4},
			{"token":"No","logprob":-1.5},
			{"token":"▁no","logprob":-2.0}
		]}]},"message":{"content":"Yes"}}]}`)
	}))
	defer server.Close()

	judge := NewLogprob(LogprobConfig{BaseURL: server.URL + "/v1", Model: "m"})
	scores, err := judge.Score(context.Background(), "the problem", []string{"a path"})
	if err != nil {
		t.Fatal(err)
	}
	pYes := math.Exp(-0.2) + math.Exp(-0.4)
	pNo := math.Exp(-1.5) + math.Exp(-2.0)
	want := pYes / (pYes + pNo)
	if len(scores) != 1 || math.Abs(scores[0]-want) > 1e-9 {
		t.Fatalf("scores = %v, want [%v]", scores, want)
	}
	if gotBody["max_tokens"] != float64(1) || gotBody["logprobs"] != true || gotBody["top_logprobs"] != float64(20) {
		t.Errorf("body params = %v", gotBody)
	}
	msgs := gotBody["messages"].([]any)
	user := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "problem:\nthe problem") || !strings.Contains(user, "path:\na path") || !strings.HasSuffix(user, "Answer yes or no.") {
		t.Errorf("user prompt = %q", user)
	}
}

func TestLogprobMissingLogprobs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"yes"}}]}`)
	}))
	defer server.Close()
	judge := NewLogprob(LogprobConfig{BaseURL: server.URL, Model: "m"})
	_, err := judge.Score(context.Background(), "p", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "top_logprobs") {
		t.Fatalf("err = %v, want top_logprobs error", err)
	}
}
