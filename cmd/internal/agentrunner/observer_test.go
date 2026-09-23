package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
)

func TestSessionObserverDoesNotStopOnTextResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	observer := &sessionObserver{sessionID: "session", output: &output, cancel: cancel}
	observer.Observe("session", sessionstore.Item{
		Kind: sessionstore.ItemModelResponse,
		Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{
			Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "The checks are still running."},
		}}}},
	})
	if err := observer.Err(); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("model response was not written")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("observer canceled the run: %v", err)
	}
}

func TestSessionObserverStopsOnOutputFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	want := errors.New("output unavailable")
	output := &failingItemWriter{kind: sessionstore.ItemTurn, err: want}
	observer := &sessionObserver{sessionID: "session", output: output, cancel: cancel}
	item := sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "turn-1", Type: session.TurnRegular}}
	observer.Observe("other-session", item)
	if output.writes != 0 || ctx.Err() != nil {
		t.Fatal("unrelated session affected the observer")
	}
	observer.Observe("session", item)
	if !errors.Is(observer.Err(), want) || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("observer error = %v, context error = %v", observer.Err(), ctx.Err())
	}
	output.err = errors.New("later failure")
	observer.Observe("session", item)
	if output.writes != 1 || !errors.Is(observer.Err(), want) {
		t.Fatal("observer wrote again or replaced its first error")
	}
}

type failingItemWriter struct {
	kind      sessionstore.ItemKind
	inputKind inbox.InputKind
	err       error
	writes    int
}

func (output *failingItemWriter) Write(data []byte) (int, error) {
	output.writes++
	var item sessionstore.Item
	if err := json.Unmarshal(data, &item); err != nil {
		return 0, err
	}
	if item.Kind == output.kind {
		if output.inputKind != "" && item.Data.(inbox.Input).Kind != output.inputKind {
			return len(data), nil
		}
		return 0, output.err
	}
	return len(data), nil
}
