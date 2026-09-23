package agentrunner

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
)

func TestRunMainHeartbeatReleasesWaitingBashAndReplays(t *testing.T) {
	workspace, sessions := t.TempDir(), t.TempDir()
	path := filepath.Join(workspace, "release")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	pipe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pipe.Close(); err != nil {
			t.Error(err)
		}
	})
	started, released, finished := false, false, false
	waitingCall := llm.ToolCall{
		CallID: "waiting-call", Name: "Bash", Arguments: `{"command":"read value < release; printf '%s' \"$value\""}`,
	}
	client := &fakeClient{respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
		if !started {
			started = true
			return llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: waitingCall}}}, nil
		}
		for _, item := range request.Input {
			if item.Type != llm.ItemToolResult {
				continue
			}
			result := item.Data.(llm.ToolResult)
			if result.CallID != "waiting-call" || result.Output[0].Value == contextbuilder.ToolCallRunningPayload {
				continue
			}
			if result.Output[0].Value != "released" {
				return llm.Response{}, fmt.Errorf("unexpected Bash result: %s", result.Output[0].Value)
			}
			finished = true
			return llm.Response{}, nil
		}
		if heartbeatMessageCount(request) == 0 {
			return llm.Response{}, errors.New("waiting turn has no heartbeat")
		}
		if !released {
			if _, err := io.WriteString(pipe, "released\n"); err != nil {
				return llm.Response{}, err
			}
			released = true
		}
		return llm.Response{}, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	getenv := func(name string) string {
		switch name {
		case llmAPIKeyEnvironment:
			return "secret"
		case "SHELL":
			return "/bin/sh"
		default:
			return ""
		}
	}
	args := []string{"-workspace", workspace, "-session-directory", sessions, "-tool-heartbeat-interval", "10ms"}
	var stdout, stderr bytes.Buffer
	if code := RunMain(ctx, args, getenv, func() []string { return nil },
		strings.NewReader(`{"prompt":"run it","session_id":"heartbeat-session"}`),
		&stdout, &stderr, testConfig(client)); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !released || !finished {
		t.Fatal("heartbeat did not release Bash and deliver its result")
	}
	heartbeats := 0
	decoder := jsontext.NewDecoder(&stdout)
	for {
		var item sessionstore.Item
		if err := json.UnmarshalDecode(decoder, &item); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if item.Kind == sessionstore.ItemInput {
			input := item.Data.(inbox.Input)
			if input.Kind != inbox.InputControl {
				continue
			}
			control, err := input.DecodeControlMessage()
			if err != nil {
				t.Fatal(err)
			}
			if control.Mode == inbox.Heartbeat {
				encoded, ok := strings.CutPrefix(control.Reason, "Heartbeat: waited 0.01 seconds for tool calls.\nRunning: ")
				if !ok {
					t.Fatalf("heartbeat reason = %q", control.Reason)
				}
				var calls []llm.ToolCall
				if err := json.Unmarshal([]byte(encoded), &calls); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(calls, []llm.ToolCall{waitingCall}) {
					t.Fatalf("heartbeat tool calls = %#v, want %#v", calls, waitingCall)
				}
				heartbeats++
			}
		}
	}
	if heartbeats == 0 {
		t.Fatal("heartbeat was not recorded in the session log")
	}
	resumed := &fakeClient{respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
		if got := heartbeatMessageCount(request); got != heartbeats {
			return llm.Response{}, fmt.Errorf("replayed heartbeat messages = %d, want %d", got, heartbeats)
		}
		return llm.Response{}, nil
	}}
	if code := RunMain(ctx, args, getenv, func() []string { return nil },
		strings.NewReader(`{"prompt":"summarize","session_id":"heartbeat-session"}`),
		io.Discard, &stderr, testConfig(resumed)); code != 0 {
		t.Fatalf("resume exit = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunMainRejectsInvalidHeartbeatInterval(t *testing.T) {
	for _, interval := range []string{"-1s", "invalid", "999999999999999999999s"} {
		t.Run(interval, func(t *testing.T) {
			var stderr bytes.Buffer
			code := RunMain(t.Context(), []string{"-tool-heartbeat-interval", interval},
				func(string) string { return "" }, func() []string { return nil },
				strings.NewReader(`{"prompt":"hello"}`), io.Discard, &stderr, Config{Name: "heuristic", ParseRequest: parseTestRequest})
			if code != 1 || !strings.Contains(stderr.String(), "heartbeat") {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
		})
	}
}

func heartbeatMessageCount(request llm.Request) int {
	count := 0
	for _, item := range request.Input {
		if item.Type == llm.ItemMessage {
			message := item.Data.(llm.Message)
			if message.Role == llm.RoleUser && strings.HasPrefix(message.Text, "Heartbeat: waited 0.01 seconds for tool calls.\nRunning: ") {
				count++
			}
		}
	}
	return count
}
