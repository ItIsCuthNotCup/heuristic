package agentrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore/localfile"
)

func TestRunResumesAfterOutputFailure(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      sessionstore.ItemKind
		inputKind inbox.InputKind
	}{
		{"settings", sessionstore.ItemInput, inbox.InputControl},
		{"message", sessionstore.ItemInput, inbox.InputExternal},
		{"response", sessionstore.ItemModelResponse, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, sessions := t.TempDir(), t.TempDir()
			request := `{"session_id":"output-failure","messages":[{"role":"user","content":"hello","message_id":"69621f8d-4f4d-49a5-8f7d-3b24fd855c01"}]}`
			requests := make(chan context.Context, 4)
			client := &fakeClient{respond: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
				requests <- ctx
				return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Hello."}}}}, nil
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			run := func(output io.Writer) error {
				return Run(ctx, []string{"-workspace", workspace, "-session-directory", sessions},
					func(name string) string {
						if name == llmAPIKeyEnvironment {
							return "secret"
						}
						return ""
					}, func() []string { return nil }, strings.NewReader(request), output, io.Discard, testConfig(client))
			}
			want := errors.New("output unavailable")
			if err := run(&failingItemWriter{kind: test.kind, inputKind: test.inputKind, err: want}); !errors.Is(err, want) {
				t.Fatalf("Run error = %v, want original output error", err)
			}
			store, err := localfile.New(sessions)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.Items(ctx, "output-failure", sessionstore.BeforeFirst, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range page.Items {
				found = found || item.Kind == test.kind
			}
			if !found {
				t.Fatal("item whose output failed was not committed")
			}
			var output bytes.Buffer
			if err := run(&output); err != nil {
				t.Fatal(err)
			}
			wantInputs := 0
			if test.name == "settings" {
				wantInputs = 1
			}
			if got := len(inputIDs(t, output.String())); got != wantInputs {
				t.Fatalf("retry persisted %d user inputs, want %d", got, wantInputs)
			}
			if len(requests) != 1 {
				t.Fatalf("model calls across failure and retry = %d, want 1", len(requests))
			}
			if err := (<-requests).Err(); !errors.Is(err, context.Canceled) {
				t.Fatalf("model context error = %v, want cancellation", err)
			}
			if err := run(io.Discard); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 0 {
				t.Fatal("settled retry started another model request")
			}
		})
	}
}

func TestRunMainResumesInterruptedDeliveryWithDuplicateInput(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		name := "first-response"
		if withTool {
			name = "tool-result"
		}
		t.Run(name, func(t *testing.T) {
			workspace, sessions := t.TempDir(), t.TempDir()
			model, effort := "initial-model", llm.ReasoningEffortLow
			messageID := "69621f8d-4f4d-49a5-8f7d-3b24fd855c01"
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			run := func(client Client) (int, string, string) {
				var stdout, stderr bytes.Buffer
				request := fmt.Sprintf(`{"model":%q,"thinking_level":%q,"session_id":"resume-delivery","messages":[{"role":"user","content":"run it","message_id":%q}]}`, model, effort, messageID)
				code := RunMain(ctx, []string{"-workspace", workspace, "-session-directory", sessions},
					func(name string) string {
						switch name {
						case llmAPIKeyEnvironment:
							return "secret"
						case "SHELL":
							return "/bin/sh"
						default:
							return ""
						}
					}, func() []string { return []string{"PATH=/usr/bin:/bin"} },
					strings.NewReader(request), &stdout, &stderr, testConfig(client))
				return code, stdout.String(), stderr.String()
			}
			hasResult := func(request llm.Request) bool {
				for _, item := range request.Input {
					if item.Type == llm.ItemToolResult {
						result := item.Data.(llm.ToolResult)
						if result.CallID == "call-1" && result.Output[0].Value == "hello" {
							return true
						}
					}
				}
				return false
			}
			interrupted := &fakeClient{}
			interrupted.respond = func(_ context.Context, request llm.Request) (llm.Response, error) {
				interrupted.calls++
				if request.Model.ID != model || request.Model.ReasoningEffort != effort {
					return llm.Response{}, fmt.Errorf("initial settings = %#v", request.Model)
				}
				if withTool && interrupted.calls == 1 {
					return llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{
						CallID: "call-1", Name: "Bash", Arguments: `{"command":"printf hello"}`,
					}}}}, nil
				}
				if withTool && !hasResult(request) {
					return llm.Response{}, errors.New("missing completed result")
				}
				return llm.Response{}, errors.New("interrupted delivery")
			}
			code, _, stderr := run(interrupted)
			wantCalls := 1
			if withTool {
				wantCalls = 2
			}
			if code != 1 || interrupted.calls != wantCalls || !strings.Contains(stderr, "interrupted delivery") {
				t.Fatalf("first run: exit=%d, calls=%d, stderr=%s", code, interrupted.calls, stderr)
			}
			model, effort = "resumed-model", llm.ReasoningEffortMax
			resumed := &fakeClient{}
			resumed.respond = func(_ context.Context, request llm.Request) (llm.Response, error) {
				resumed.calls++
				if request.Model.ID != model || request.Model.ReasoningEffort != llm.ReasoningEffortLow {
					return llm.Response{}, fmt.Errorf("recovered request settings = %#v", request.Model)
				}
				if withTool && !hasResult(request) {
					return llm.Response{}, errors.New("missing restored result")
				}
				return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "Done."}}}}, nil
			}
			code, stdout, stderr := run(resumed)
			if code != 0 || resumed.calls != 1 {
				t.Fatalf("resumed run: exit=%d, calls=%d, stderr=%s", code, resumed.calls, stderr)
			}
			if ids := inputIDs(t, stdout); len(ids) != 0 {
				t.Fatalf("duplicate input was recorded again: %v", ids)
			}
			items := decodeLogItems(t, []byte(stdout))
			settings := 0
			for _, item := range items {
				if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputControl {
					control, err := input.DecodeControlMessage()
					if err != nil {
						t.Fatal(err)
					}
					if control.Mode == inbox.UpdateSettings {
						settings++
						if control.Parameters != (inbox.Settings{ReasoningEffort: effort}) {
							t.Fatalf("recorded settings = %#v", control.Parameters)
						}
					}
				}
			}
			if settings != 1 {
				t.Fatalf("recorded settings controls = %d, want 1", settings)
			}
			responses := 0
			for _, kind := range itemKinds(t, stdout) {
				if kind == sessionstore.ItemModelResponse {
					responses++
				}
			}
			if responses != 1 {
				t.Fatalf("recorded responses = %d, want 1", responses)
			}
			resumed.calls = 0
			code, _, stderr = run(resumed)
			if code != 0 || resumed.calls != 0 {
				t.Fatalf("completed retry: exit=%d, calls=%d, stderr=%s", code, resumed.calls, stderr)
			}
			messageID = "49621f8d-4f4d-49a5-8f7d-3b24fd855c02"
			resumed.respond = func(_ context.Context, request llm.Request) (llm.Response, error) {
				resumed.calls++
				if request.Model.ID != model || request.Model.ReasoningEffort != effort {
					return llm.Response{}, fmt.Errorf("following request settings = %#v", request.Model)
				}
				return llm.Response{}, nil
			}
			code, _, stderr = run(resumed)
			if code != 0 || resumed.calls != 1 {
				t.Fatalf("follow-up: exit=%d, calls=%d, stderr=%s", code, resumed.calls, stderr)
			}
		})
	}
}

func TestRunResumesSessionWithoutRecordedSettings(t *testing.T) {
	workspace, sessions := t.TempDir(), t.TempDir()
	store, err := localfile.New(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendInput(t.Context(), "legacy", inbox.Input{
		ID: "69621f8d-4f4d-49a5-8f7d-3b24fd855c01", Kind: inbox.InputExternal, Payload: []byte(`"hello"`),
	}); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{}
	client.respond = func(_ context.Context, request llm.Request) (llm.Response, error) {
		client.calls++
		if request.Model.ID != "selected-model" || request.Model.ReasoningEffort != llm.ReasoningEffortMedium {
			return llm.Response{}, fmt.Errorf("legacy session request settings = %#v", request.Model)
		}
		return llm.Response{}, nil
	}
	var output bytes.Buffer
	err = Run(t.Context(), []string{"-workspace", workspace, "-session-directory", sessions},
		func(name string) string {
			if name == llmAPIKeyEnvironment {
				return "secret"
			}
			return ""
		}, func() []string { return nil }, strings.NewReader(`{
			"session_id":"legacy", "model":"selected-model", "thinking_level":"medium",
			"messages":[{"role":"user","content":"hello","message_id":"69621f8d-4f4d-49a5-8f7d-3b24fd855c01"}]
		}`), &output, io.Discard, testConfig(client))
	if err != nil || client.calls != 1 {
		t.Fatalf("legacy session: calls=%d, error=%v", client.calls, err)
	}
	if ids := inputIDs(t, output.String()); len(ids) != 0 {
		t.Fatalf("legacy input was recorded again: %v", ids)
	}
}
