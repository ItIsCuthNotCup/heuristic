package coordinator

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/responsesapi"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

func FuzzCoordinatorFaults(f *testing.F) {
	for mode := range uint8(14) {
		f.Add([]byte{0, 17, 35}, mode, uint8(0), "result\n\"会話", uint64(1<<53+1))
		f.Add([]byte{48, 2, 19}, mode, uint8(255), "", uint64(math.MaxInt64))
	}
	f.Add([]byte(nil), uint8(0), uint8(0), "", uint64(0))
	f.Fuzz(func(t *testing.T, actions []byte, mode, occurrence uint8, text string, tokens uint64) {
		if len(actions) > 8 || len(text) > 4096 {
			t.Skip()
		}
		synctest.Test(t, func(t *testing.T) {
			text = strings.ToValidUTF8(text, "\uFFFD")
			if len(actions) == 0 {
				actions = []byte{0}
			}
			mode %= 14
			hardStop := mode == 8 || mode == 12
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			sites := []string{"", "history", "input", "turn", "response", "status", "save", "add", "cancel", "transport", "http", "body", "", ""}
			counts := []int{1, 1, 2, 2, 2, 2 * len(actions), 2 * len(actions), len(actions), len(actions), 2, 2, 2, 1, 1}
			trace := &coordinatorFaultTrace{site: sites[mode], remaining: int(occurrence) % counts[mode]}
			store := &faultMemoryStore{trace: trace}
			const id session.ID = "faults"
			if _, err := store.Create(ctx, id); err != nil {
				t.Fatal(err)
			}
			inputs, err := inbox.New(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := inputs.Submit(ctx, inbox.Input{ID: "user", Kind: inbox.InputExternal, Payload: faultJSON(t, text)}); err != nil {
				t.Fatal(err)
			}
			if err := inputs.Submit(ctx, inbox.Input{ID: "idle", Kind: inbox.InputControl, Payload: faultJSON(t, inbox.ControlMessage{Mode: inbox.StopWhenIdle})}); err != nil {
				t.Fatal(err)
			}
			registry := tool.NewRegistry(tool.StaticTranslators{}, tool.SkillUseName)
			manager := &controlledFaultOperations{
				ctx: ctx, trace: trace, plans: make(map[string]faultOperationPlan),
				updates: make(chan operation.Operation, len(actions)*4), started: make(chan struct{}),
				cancels: make(map[operation.ID]context.CancelFunc),
			}
			var wireCalls []map[string]any
			results := make(map[string]string)
			for index, action := range actions {
				name := fmt.Sprintf("skill-%d", index)
				plan := faultOperationPlan{
					path: "/fuzz/" + name, result: fmt.Sprintf("result-%d: %s", index, text),
					delay: time.Duration(1+action%16) * time.Millisecond, fail: action&16 != 0, repeat: action&32 != 0,
				}
				if hardStop {
					plan.delay = time.Hour
				}
				manager.plans[plan.path] = plan
				if _, err := registry.RegisterSkill(tool.Skill{Name: name, Description: "Controlled operation fixture.", Path: plan.path}); err != nil {
					t.Fatal(err)
				}
				callID := fmt.Sprintf("call-%d", index)
				results[callID] = plan.result
				wireCalls = append(wireCalls, map[string]any{
					"type": "function_call", "id": "provider-" + callID, "call_id": callID,
					"name": tool.SkillUseName, "arguments": string(faultJSON(t, map[string]string{"name": name})),
				})
			}
			usage := faultJSON(t, map[string]uint64{"input_tokens": tokens & math.MaxInt64, "output_tokens": tokens % 997})
			transport := &coordinatorFaultTransport{trace: trace, started: make(chan struct{}), block: mode == 13}
			for index, output := range [][]map[string]any{wireCalls, {}} {
				transport.bodies = append(transport.bodies, "data: "+string(faultJSON(t, map[string]any{
					"type":     "response.completed",
					"response": map[string]any{"id": fmt.Sprintf("response-%d", index), "status": "completed", "output": output, "usage": usage},
				}))+"\n\n")
			}
			remote := primitives.NewRemoteClientWithHTTPClient(&http.Client{Transport: transport})
			defer remote.Close()
			adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{Endpoint: "https://fuzz.invalid/responses", MaxAttempts: new(1)})
			if err != nil {
				t.Fatal(err)
			}
			builder := contextbuilder.NewBuilder(registry.Skills()...)
			builder.SetModel(llm.Model{ID: "fuzz-model"})
			for _, definition := range registry.StaticDefinitions() {
				builder.AddTool(definition.Tool)
			}
			current := New(Dependencies{SessionID: id, Inbox: inputs, Sessions: store, ContextBuilder: builder, LLM: adapter, Tools: registry, Operations: manager})
			if hardStop {
				go func() {
					select {
					case <-manager.started:
						payload := jsontext.Value(`{"Mode":"hard","Reason":"fuzz stop"}`)
						_ = inputs.Submit(ctx, inbox.Input{ID: "hard", Kind: inbox.InputControl, Payload: payload})
					case <-ctx.Done():
					}
				}()
			}
			if mode == 13 {
				go func() {
					select {
					case <-transport.started:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			err = current.Run(ctx)
			cancel()
			synctest.Wait()
			switch {
			case mode == 13:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled HTTP request: %v", err)
				}
			case trace.site != "":
				if !trace.failed || err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("fault %q reached=%v, coordinator error=%v", trace.site, trace.failed, err)
				}
				if mode < 9 && !errors.Is(err, errCoordinatorFuzzFault) {
					t.Fatalf("coordinator lost dependency error: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			assertCoordinatorFaultTrace(t, trace.events, results, usage, tokens, hardStop, err == nil)
		})
	})
}

type coordinatorFaultTransport struct {
	trace   *coordinatorFaultTrace
	bodies  []string
	started chan struct{}
	block   bool
	calls   int
}

func (transport *coordinatorFaultTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	defer request.Body.Close()
	encoded, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	transport.trace.record("request", encoded)
	index := transport.calls
	transport.calls++
	if index == 0 {
		close(transport.started)
	}
	if transport.block {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	if err := transport.trace.fail("transport"); err != nil {
		return nil, err
	}
	if err := transport.trace.fail("http"); err != nil {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"injected HTTP failure"}}`))}, nil
	}
	if err := transport.trace.fail("body"); err != nil {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(coordinatorFaultReader{})}, nil
	}
	if index >= len(transport.bodies) {
		return nil, errors.New("coordinator requested an extra model response")
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(transport.bodies[index]))}, nil
}

type coordinatorFaultReader struct{}

func (coordinatorFaultReader) Read([]byte) (int, error) { return 0, errCoordinatorFuzzFault }

func faultJSON(t *testing.T, value any) jsontext.Value {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertCoordinatorFaultTrace(t *testing.T, events []coordinatorFaultEvent, results map[string]string, usage jsontext.Value, tokens uint64, hardStop, settled bool) {
	t.Helper()
	var previous session.TurnID
	turns, requests, responses := 0, 0, 0
	failed := false
	calls := make(map[string]session.TurnID)
	committed := make(map[operation.ID]operation.Operation)
	started := make(map[operation.ID]bool)
	updates := make(map[operation.ID][]operation.Operation)
	statuses := make(map[operation.ID]operation.Operation)
	responded := make(map[session.TurnID]bool)
	for _, event := range events {
		if failed && (event.kind == "commit" || event.kind == "save" || event.kind == "request" || event.kind == "add") {
			t.Fatalf("coordinator continued %q after dependency failure", event.kind)
		}
		switch event.kind {
		case "failure":
			failed = true
		case "commit":
			item := event.value.(sessionstore.Item)
			switch value := item.Data.(type) {
			case session.Turn:
				if value.ID == "" || value.PreviousTurnID != previous {
					t.Fatalf("broken turn chain: %#v", value)
				}
				previous = value.ID
				turns++
			case sessionstore.ModelResponse:
				if responded[value.TurnID] || value.TurnID != previous || value.Response.ID != fmt.Sprintf("response-%d", requests-1) {
					t.Fatalf("response duplicated or associated with the wrong request: %#v", value)
				}
				responded[value.TurnID] = true
				responses++
				if value.Response.Usage.InputTokens != int64(tokens&math.MaxInt64) || value.Response.Usage.OutputTokens != int64(tokens%997) || string(value.Response.Usage.Raw) != string(usage) {
					t.Fatalf("usage changed between HTTP response and persistence: %#v", value.Response.Usage)
				}
				for _, item := range value.Response.Output {
					if call, ok := item.Data.(llm.ToolCall); ok {
						calls[call.CallID] = value.TurnID
					}
				}
			case sessionstore.ToolCallStatus:
				if turnID, exists := calls[value.CallID]; !exists || value.TurnID != turnID || value.Status.Error != "" || len(value.Operations) != 1 || len(value.Status.WaitingFor) != 1 || value.Status.WaitingFor[0] != value.Operations[0].ID {
					t.Fatalf("status lacks its committed call or operation: %#v", value)
				}
				op := value.Operations[0]
				if prior, exists := committed[op.ID]; exists {
					if !reflect.DeepEqual(op, prior) {
						t.Fatal("tool status differs from the saved operation")
					}
				} else if op.Status != operation.StatusReady {
					t.Fatal("operation was initialized after execution")
				}
				committed[op.ID], statuses[op.ID] = op, op
			}
		case "request":
			requests++
			if requests != turns || hardStop && requests > 1 {
				t.Fatal("HTTP request preceded its committed turn or followed a hard stop")
			}
			if requests > 1 {
				var request struct {
					Input []struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Output []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"output"`
					} `json:"input"`
				}
				if err := json.Unmarshal(event.value.([]byte), &request); err != nil {
					t.Fatal(err)
				}
				delivered := make(map[string]string)
				for _, item := range request.Input {
					if item.Type == "function_call_output" {
						if _, exists := delivered[item.CallID]; exists {
							t.Fatal("tool result delivered more than once")
						}
						if len(item.Output) != 1 || item.Output[0].Type != "input_text" {
							t.Fatalf("unexpected tool output: %#v", item.Output)
						}
						delivered[item.CallID] = item.Output[0].Text
					}
				}
				if !reflect.DeepEqual(delivered, results) {
					t.Fatalf("model received wrong tool results: got %#v, want %#v", delivered, results)
				}
			}
		case "add":
			value := event.value.(operation.Operation)
			if prior, exists := committed[value.ID]; !exists || !reflect.DeepEqual(prior, value) {
				t.Fatal("operation dispatched before its status and state committed")
			}
		case "start":
			value := event.value.(operation.Operation)
			if started[value.ID] {
				t.Fatal("operation started more than once")
			}
			started[value.ID] = true
		case "cancel":
			if !started[event.value.(operation.ID)] {
				t.Fatal("operation canceled before the manager knew it")
			}
		case "update":
			value := event.value.(operation.Operation)
			updates[value.ID] = append(updates[value.ID], value)
		case "save":
			value := event.value.(operation.Operation)
			found := false
			for _, update := range updates[value.ID] {
				found = found || reflect.DeepEqual(update, value)
			}
			if !started[value.ID] || !found {
				t.Fatal("saved operation was never reported by the manager")
			}
			committed[value.ID] = value
		}
	}
	if turns != requests {
		t.Fatalf("committed turns=%d, HTTP requests=%d", turns, requests)
	}
	if settled {
		wantResponses := 2
		if hardStop {
			wantResponses = 1
		}
		if responses != wantResponses || len(started) != len(results) {
			t.Fatalf("settled execution: responses=%d, started operations=%d", responses, len(started))
		}
		for id := range started {
			value := committed[id]
			if value.Status != operation.StatusCompleted && value.Status != operation.StatusFailed && value.Status != operation.StatusCanceled || !reflect.DeepEqual(statuses[id], value) {
				t.Fatal("coordinator stopped before recording terminal operation and tool status")
			}
			if hardStop && value.Status != operation.StatusCanceled {
				t.Fatal("hard stop did not cancel the pending operation")
			}
		}
	}
}
