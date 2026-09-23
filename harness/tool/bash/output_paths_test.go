package bash_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool/bash"
)

func TestCapturePathsSurviveFailureAndCancellation(t *testing.T) {
	for _, stage := range []struct {
		name    string
		creates int
	}{
		{"before captures", 1},
		{"stdout exists", 2},
		{"both captures exist", 3},
	} {
		for _, terminal := range []operation.Status{operation.StatusFailed, operation.StatusCanceled} {
			t.Run(stage.name+"/"+string(terminal), func(t *testing.T) {
				base := t.TempDir()
				spec, err := operation.NewShellSpec(operation.ShellInput{Shell: "/bin/sh"}, base, 100)
				if err != nil {
					t.Fatal(err)
				}
				current := operation.Operation{ID: "captures", Type: spec.Type, Version: spec.Version, State: spec.State, MaxOutputLength: spec.MaxOutputLength, Status: operation.StatusReady}
				step, err := advanceShellOnce(t, current, nil)
				if err != nil {
					t.Fatal(err)
				}
				for range stage.creates {
					if len(step.Dispatches) != 1 || step.Dispatches[0].Type != primitives.PrimitiveDispatchIOCreate {
						t.Fatalf("expected capture creation: %#v", step)
					}
					request := step.Dispatches[0].Data.(primitives.IOCreateRequest)
					events := make(chan primitives.PrimitiveEvent)
					primitives.Create(t.Context(), request, events)
					event := <-events
					if event.Type != primitives.PrimitiveEventIOCreateCompleted {
						t.Fatalf("creation failed: %#v", event)
					}
					step, err = advanceShellOnce(t, *step.Operation, &event)
					if err != nil {
						t.Fatal(err)
					}
				}
				if terminal == operation.StatusFailed {
					step, err = advanceShellOnce(t, *step.Operation, &primitives.PrimitiveEvent{
						Source: primitives.SourceID(current.ID), Type: primitives.PrimitiveEventFailed,
						Result: primitives.PrimitiveFailureResult{Error: "capture creation failed"},
					})
				} else {
					step.Operation.Status = operation.StatusCanceling
					step, err = advanceShellOnce(t, *step.Operation, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				state, err := operation.DecodeShellState(*step.Operation)
				if err != nil {
					t.Fatal(err)
				}
				if step.Operation.Status != terminal || state.Result != nil {
					t.Fatalf("unexpected terminal operation: %#v", step.Operation)
				}
				result, err := bash.New(bash.Config{}).TranslateResult("call", tool.CallStatus{}, []operation.Operation{*step.Operation})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasSuffix(result.Output[0].Value, "Error: "+state.TerminalError) {
					t.Fatalf("unexpected tool result: %s", result.Output[0].Value)
				}
				for _, stream := range []struct {
					filename, recorded string
					exists             bool
				}{
					{operation.ShellOutFilename, state.OutPath, stage.creates >= 2},
					{operation.ShellErrFilename, state.ErrPath, stage.creates >= 3},
				} {
					path := filepath.Join(base, string(current.ID), stream.filename)
					_, statErr := os.Stat(path)
					want := ""
					if stream.exists {
						if statErr != nil {
							t.Fatal(statErr)
						}
						want = path
					} else if !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("unexpected capture file at %q: %v", path, statErr)
					}
					if stream.recorded != want {
						t.Fatalf("%s path: recorded %q, want %q", stream.filename, stream.recorded, want)
					}
					wantReferences := 0
					if stream.exists {
						wantReferences = 1
					}
					if strings.Count(result.Output[0].Value, path) != wantReferences {
						t.Fatalf("expected %d references to %q in %q", wantReferences, path, result.Output[0].Value)
					}
				}
			})
		}
	}
}

func TestTruncatedOutputCanBeReadFromCaptureFiles(t *testing.T) {
	for _, test := range []struct {
		name           string
		stdout, stderr string
		limit          int
	}{
		{name: "default character limit", stdout: strings.Repeat("界", 40001), stderr: strings.Repeat("e", 40001)},
		{name: "newlines and backslashes within default", stdout: strings.Repeat("\n", 20001), stderr: strings.Repeat("\\", 20001)},
		{name: "newlines and backslashes exceed default", stdout: strings.Repeat("\n", 40001), stderr: strings.Repeat("\\", 40001)},
		{name: "source read within default", stdout: strings.Repeat("界", 25000)},
		{name: "default boundary", stdout: strings.Repeat("x", 40000)},
		{name: "complete output", stdout: "ok"},
		{name: "requested limit", stdout: "界界界界", stderr: "éééé", limit: 3},
		{name: "requested limit below default", stdout: strings.Repeat("界", 5000), limit: 5000},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "captures with spaces")
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OUT", test.stdout)
			t.Setenv("ERR", test.stderr)
			translator := bash.New(bash.Config{
				Shell: "/bin/sh", BaseDirectory: base,
			})
			submitted := &recordingContext{}
			arguments := map[string]any{"command": `printf '%s' "$OUT"; printf '%s' "$ERR" >&2`}
			if test.limit != 0 {
				arguments["max_output_length"] = test.limit
			}
			encoded, err := json.Marshal(arguments)
			if err != nil {
				t.Fatal(err)
			}
			status := translator.Translate(submitted, llm.ToolCall{CallID: "call-1", Arguments: string(encoded)})
			if status.Error != "" || len(submitted.specs) != 1 {
				t.Fatalf("status = %#v", status)
			}
			spec := submitted.specs[0]
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			manager := operation.NewLocalOperationManager(ctx)
			if err := manager.Add(operation.Operation{ID: status.WaitingFor[0], Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, MaxOutputLength: spec.MaxOutputLength, State: spec.State}); err != nil {
				t.Fatal(err)
			}
			var completed operation.Operation
			for completed.Status != operation.StatusCompleted {
				select {
				case update, open := <-manager.Updates():
					if !open || update.Status == operation.StatusFailed || update.Status == operation.StatusCanceled {
						t.Fatalf("operation did not complete: %#v", update)
					}
					completed = update
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			// A resumed translator must use the operation's capture directory.
			translator = bash.New(bash.Config{BaseDirectory: t.TempDir()})
			result, err := translator.TranslateResult("call-1", status, []operation.Operation{completed})
			if err != nil {
				t.Fatal(err)
			}
			state, err := operation.DecodeShellState(completed)
			if err != nil {
				t.Fatal(err)
			}
			want := state.Result.Out
			if state.Result.Err != "" {
				want += "\nStderr:\n" + state.Result.Err
			}
			if result.Output[0].Value != want {
				t.Fatalf("translator changed prepared output: %q, want %q", result.Output[0].Value, want)
			}
			for _, stream := range []struct {
				capturePath, text, preview string
				truncated                  bool
			}{
				{state.OutPath, test.stdout, state.Result.Out, state.OutTruncated},
				{state.ErrPath, test.stderr, state.Result.Err, state.ErrTruncated},
			} {
				if !filepath.IsAbs(stream.capturePath) || !strings.HasPrefix(stream.capturePath, base+string(filepath.Separator)) {
					t.Fatalf("capture path = %q", stream.capturePath)
				}
				full, err := os.ReadFile(stream.capturePath)
				if err != nil {
					t.Fatal(err)
				}
				if string(full) != stream.text {
					t.Fatalf("capture does not contain full output: %q", full)
				}
				limit := test.limit
				if limit == 0 {
					limit = operation.DefaultMaxOutputLength
				}
				wantTruncated := utf8.RuneCountInString(stream.text) > limit
				if stream.truncated != wantTruncated {
					t.Fatalf("truncation = %t, want %t", stream.truncated, wantTruncated)
				}
				if !wantTruncated {
					if strings.Contains(result.Output[0].Value, stream.capturePath) || stream.preview != stream.text {
						t.Fatalf("complete output = %#v", stream)
					}
					continue
				}
				if strings.Count(result.Output[0].Value, stream.capturePath) != 1 {
					t.Fatalf("tool result must contain capture path %q exactly once", stream.capturePath)
				}
			}
		})
	}
}

func advanceShellOnce(t *testing.T, current operation.Operation, event *primitives.PrimitiveEvent) (operation.Step, error) {
	t.Helper()
	shell, err := operation.NewShell(current)
	if err != nil {
		return operation.Step{}, err
	}
	return shell.Handle(event)
}
