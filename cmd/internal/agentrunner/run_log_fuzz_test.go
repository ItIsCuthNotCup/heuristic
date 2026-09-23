package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/inbox"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/session"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/sessionstore/localfile"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
)

func FuzzRunLogMatchesExecution(f *testing.F) {
	addLogFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, actions []byte, text string, input, cached, written, output, reasoning uint64) {
		if len(actions) > 16 || len(text) > 128<<10 || len(actions)*len(text) > 256<<10 {
			t.Skip()
		}
		synctest.Test(t, func(t *testing.T) {
			text = strings.ToValidUTF8(text, "\uFFFD")
			usage := fuzzLogUsage(t, text, input, cached, written, output, reasoning)
			workspace, sessions := t.TempDir(), t.TempDir()
			skillPath := filepath.Join(workspace, ".harness", "skills", "journal", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skillPath), 0o700); err != nil {
				t.Fatal(err)
			}
			skillContent := "---\nname: journal\ndescription: Read the journal fixture.\n---\n" + text
			if err := os.WriteFile(skillPath, []byte(skillContent), 0o600); err != nil {
				t.Fatal(err)
			}
			const id session.ID = "execution"
			messageID := "69621f8d-4f4d-49a5-8f7d-3b24fd855c01"
			encodedRequest := logJSON(t, Request{
				SessionID: new(string(id)), Model: "journal-model",
				Messages: []RequestMessage{
					{Role: "user", Content: text, MessageID: &messageID},
					{Role: "user", Content: text, MessageID: &messageID},
				},
			})
			var responses []llm.Response
			validCalls := make(map[string]bool)
			for index, action := range actions {
				response := fuzzLogResponse(t, text, usage, index, action)
				for callIndex := 0; callIndex < 1+int(action%2); callIndex++ {
					call := llm.ToolCall{CallID: fmt.Sprintf("call-%d-%d", index, callIndex), Name: tool.SkillUseName, Arguments: `{"name":"journal"}`}
					switch action % 4 {
					case 0:
						call.Name = "UnknownTool"
					case 1:
						call.Arguments = `{`
					default:
						validCalls[call.CallID] = true
					}
					response.Output = append(response.Output, llm.Item{ProviderID: call.CallID, Type: llm.ItemToolCall, Data: call})
				}
				responses = append(responses, response)
			}
			responses = append(responses, fuzzLogResponse(t, text, usage, len(responses), byte(len(actions))))

			mode, failAt := 0, 0
			if len(actions) > 0 {
				mode = int(actions[0]) % 4
				failAt = int(actions[len(actions)-1]) % len(responses)
			}
			injected := false
			providerFailure := errors.New("generated provider failure")
			outputFailure := errors.New("generated stdout failure")
			var returned []llm.Response
			requests := 0
			var history []sessionstore.Item
			var allLogged []sessionstore.Item
			var previousLog []byte
			logDirectory := filepath.Join(workspace, "logs")
			for run := range 3 {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				client := &fakeClient{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
					if request.Model.ID != "journal-model" || request.Model.ReasoningEffort != llm.ReasoningEffortHigh {
						return llm.Response{}, fmt.Errorf("unexpected request settings: %#v", request.Model)
					}
					if err := ctx.Err(); err != nil {
						return llm.Response{}, err
					}
					requests++
					if !injected && len(returned) == failAt && (mode == 1 || mode == 2) {
						injected = true
						if mode == 2 {
							cancel()
							return llm.Response{}, context.Canceled
						}
						return llm.Response{}, providerFailure
					}
					if len(returned) == len(responses) {
						return llm.Response{}, errors.New("settled execution requested another model response")
					}
					response := responses[len(returned)]
					returned = append(returned, response)
					return copyLogResponse(response), nil
				}}
				var stdout bytes.Buffer
				var destination io.Writer = &stdout
				if run == 0 && mode == 3 {
					destination = &fuzzLogOutput{output: &stdout, responses: failAt, err: outputFailure}
				}
				err := Run(ctx, []string{"-workspace", workspace, "-session-directory", sessions, "-log-directory", logDirectory, "-tool-heartbeat-interval", "0"},
					func(name string) string {
						if name == llmAPIKeyEnvironment {
							return "secret"
						}
						return ""
					}, func() []string { return nil }, bytes.NewReader(encodedRequest), destination, io.Discard, testConfig(client))
				cancel()
				synctest.Wait()
				var wantErr error
				if run == 0 {
					switch mode {
					case 1:
						wantErr = providerFailure
					case 2:
						wantErr = context.Canceled
					case 3:
						wantErr = outputFailure
					}
				}
				if !errors.Is(err, wantErr) {
					t.Fatalf("run %d: error = %v, want %v", run, err, wantErr)
				}
				if !client.closed {
					t.Fatal("runner did not close the client")
				}
				entries, err := os.ReadDir(logDirectory)
				if err != nil || len(entries) == 0 {
					t.Fatalf("log files = %v, error = %v", entries, err)
				}
				var combined []byte
				for _, entry := range entries {
					logged, err := os.ReadFile(filepath.Join(logDirectory, entry.Name()))
					if err != nil {
						t.Fatal(err)
					}
					combined = append(combined, logged...)
				}
				if !bytes.HasPrefix(combined, previousLog) {
					t.Fatal("resuming changed the previous file log")
				}
				logged := combined[len(previousLog):]
				previousLog = combined
				if run == 0 && mode == 3 {
					if !bytes.HasPrefix(logged, stdout.Bytes()) || len(logged) == stdout.Len() {
						t.Fatal("file log did not preserve the record whose stdout write failed")
					}
				} else if !bytes.Equal(logged, stdout.Bytes()) {
					t.Fatal("file log differs from stdout")
				}
				store, err := localfile.New(sessions)
				if err != nil {
					t.Fatal(err)
				}
				items := decodeLogItems(t, logged)
				nextHistory := readLogHistory(t, store, id, 1+len(actions)%7)
				if len(nextHistory) < len(history) || (len(history) != 0 && !reflect.DeepEqual(nextHistory[:len(history)], history)) {
					t.Fatal("resuming changed existing history")
				}
				if !reflect.DeepEqual(nextHistory[len(history):], items) {
					t.Fatal("file log does not exactly match newly persisted history")
				}
				history = nextHistory
				allLogged = append(allLogged, items...)
				assertExecutionLog(t, allLogged, returned, requests, text, skillContent, validCalls, run+1, run > 0 || mode == 0)
				if run > 0 {
					resumed, err := store.Resume(t.Context(), id)
					if err != nil || len(resumed.Operations) != 0 {
						t.Fatalf("settled run has unfinished operations: %v, %v", resumed.Operations, err)
					}
				}
			}
			if len(returned) != len(responses) {
				t.Fatalf("execution returned %d of %d planned responses", len(returned), len(responses))
			}
		})
	})
}

func assertExecutionLog(t *testing.T, items []sessionstore.Item, returned []llm.Response, requests int, text, skillContent string, validCalls map[string]bool, runs int, settled bool) {
	t.Helper()
	var responses []llm.Response
	var turns []session.Turn
	var previous session.TurnID
	inputs := 0
	stops, settings := 0, 0
	inputIDs := make(map[inbox.ID]bool)
	calls := make(map[string]session.TurnID)
	statuses := make(map[string]sessionstore.ToolCallStatus)
	responded := make(map[session.TurnID]bool)
	for index, item := range items {
		if item.Sequence != sessionstore.Sequence(index+1) || item.RecordedAt.IsZero() {
			t.Fatalf("invalid sequence or timestamp at item %d: %#v", index, item)
		}
		switch value := item.Data.(type) {
		case inbox.Input:
			if value.ID == "" || inputIDs[value.ID] {
				t.Fatalf("empty or duplicate input ID: %q", value.ID)
			}
			inputIDs[value.ID] = true
			switch value.Kind {
			case inbox.InputExternal:
				inputs++
				var content string
				if err := json.Unmarshal(value.Payload, &content); err != nil || content != text {
					t.Fatalf("external input changed: %s, %v", value.Payload, err)
				}
			case inbox.InputControl:
				control, err := value.DecodeControlMessage()
				if err != nil {
					t.Fatal(err)
				}
				switch control.Mode {
				case inbox.StopWhenIdle:
					stops++
				case inbox.UpdateSettings:
					settings++
					if control.Parameters != (inbox.Settings{ReasoningEffort: llm.ReasoningEffortHigh}) {
						t.Fatalf("logged settings differ from the runner request: %#v", control.Parameters)
					}
				default:
					t.Fatalf("unexpected control: %#v", control)
				}
			default:
				t.Fatalf("unexpected input kind %q", value.Kind)
			}
		case session.Turn:
			if value.ID == "" || value.PreviousTurnID != previous || value.Type != session.TurnRegular {
				t.Fatalf("invalid turn chain: %#v after %q", value, previous)
			}
			previous = value.ID
			turns = append(turns, value)
		case sessionstore.ModelResponse:
			if value.TurnID != previous || responded[value.TurnID] {
				t.Fatalf("response duplicated or attached to the wrong turn: %q", value.TurnID)
			}
			responded[value.TurnID] = true
			responses = append(responses, value.Response)
			for _, output := range value.Response.Output {
				if call, ok := output.Data.(llm.ToolCall); ok {
					calls[call.CallID] = value.TurnID
				}
			}
		case sessionstore.ToolCallStatus:
			if turnID, exists := calls[value.CallID]; !exists || value.TurnID != turnID {
				t.Fatalf("tool status preceded its call or changed turn: %#v", value)
			}
			if !validCalls[value.CallID] {
				if value.Status.Error == "" || len(value.Operations) != 0 || len(value.Status.WaitingFor) != 0 {
					t.Fatalf("invalid call was not recorded as an error: %#v", value)
				}
			} else {
				if value.Status.Error != "" || len(value.Operations) != 1 || len(value.Status.WaitingFor) != 1 || value.Status.WaitingFor[0] != value.Operations[0].ID {
					t.Fatalf("tool status lost its operation: %#v", value)
				}
				if prior, exists := statuses[value.CallID]; exists && prior.Operations[0].ID != value.Operations[0].ID {
					t.Fatal("tool operation identity changed")
				}
				if value.Operations[0].Status == operation.StatusCompleted {
					state, err := operation.DecodeSkillUse(value.Operations[0])
					if err != nil || string(state.Content) != skillContent {
						t.Fatalf("logged tool result differs from the file read: %#v, %v", state, err)
					}
				}
			}
			statuses[value.CallID] = value
		default:
			t.Fatalf("unexpected logged item: %#v", item)
		}
	}
	if inputs != 1 {
		t.Fatalf("logged external inputs = %d, want one despite retries", inputs)
	}
	if stops != runs || settings != runs {
		t.Fatalf("logged stops = %d, settings = %d, runs = %d", stops, settings, runs)
	}
	if len(turns) != requests {
		t.Fatalf("logged turns = %d, actual model requests = %d", len(turns), requests)
	}
	if !reflect.DeepEqual(responses, returned) {
		t.Fatalf("logged responses differ from completed model responses (including usage):\ngot  %s\nwant %s", logJSON(t, responses), logJSON(t, returned))
	}
	if settled {
		for callID := range calls {
			status, exists := statuses[callID]
			if !exists || validCalls[callID] && status.Operations[0].Status != operation.StatusCompleted {
				t.Fatalf("settled execution lost the result for call %q", callID)
			}
		}
	}
}

type fuzzLogOutput struct {
	output    io.Writer
	responses int
	err       error
}

func (writer *fuzzLogOutput) Write(data []byte) (int, error) {
	var item sessionstore.Item
	if err := json.Unmarshal(data, &item); err != nil {
		return 0, err
	}
	if item.Kind == sessionstore.ItemModelResponse {
		if writer.responses == 0 {
			return 0, writer.err
		}
		writer.responses--
	}
	return writer.output.Write(data)
}
