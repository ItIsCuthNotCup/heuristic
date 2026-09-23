package bash_test

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/tool/bash"
)

type recordingContext struct {
	specs []operation.Spec
}

func (ctx *recordingContext) Submit(spec operation.Spec) operation.ID {
	ctx.specs = append(ctx.specs, spec)
	return operation.ID("operation-1")
}

func TestTranslatorSubmitsShellOperation(t *testing.T) {
	config := bash.Config{
		Shell:         "/bin/bash",
		Directory:     "/workspace",
		BaseDirectory: "/operations",
	}
	translator := bash.New(config)
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{
		CallID:    "call-1",
		Name:      "Bash",
		Arguments: `{"command":"  printf '%s' \"$HOME\"; exit 7  "}`,
	})

	if status.Error != "" || !reflect.DeepEqual(status.WaitingFor, []operation.ID{"operation-1"}) {
		t.Fatalf("status = %#v", status)
	}
	if len(ctx.specs) != 1 {
		t.Fatalf("submitted specs = %d, want 1", len(ctx.specs))
	}
	spec := ctx.specs[0]
	if spec.Type != operation.TypeShell || spec.Version != operation.VersionShell {
		t.Fatalf("spec type/version = %q/%d", spec.Type, spec.Version)
	}
	var state operation.ShellState
	if err := json.Unmarshal(spec.State, &state); err != nil {
		t.Fatalf("decode shell state: %v", err)
	}
	wantInput := operation.ShellInput{
		Command:   `  printf '%s' "$HOME"; exit 7  `,
		Shell:     config.Shell,
		Directory: config.Directory,
	}
	if !reflect.DeepEqual(state.Input, wantInput) {
		t.Fatalf("shell input = %#v, want %#v", state.Input, wantInput)
	}
	if state.BaseDirectory != config.BaseDirectory || spec.MaxOutputLength != operation.DefaultMaxOutputLength {
		t.Fatalf("shell configuration = %#v", state)
	}
}

func TestTranslatorTranslatesShellOperationResults(t *testing.T) {
	tests := []struct {
		name   string
		status operation.Status
		state  operation.ShellState
		want   string
	}{
		{
			name:   "completed",
			status: operation.StatusCompleted,
			state: operation.ShellState{
				Input:         operation.ShellInput{Command: "secret command"},
				BaseDirectory: "/secret/path",

				Result: &operation.ShellResult{
					Out:     "ok\n",
					OutSize: 3,
				},
			},
			want: "ok\n",
		},
		{
			name:   "empty output",
			status: operation.StatusCompleted,
			state:  operation.ShellState{Result: &operation.ShellResult{}},
			want:   "(no output)",
		},
		{
			name:   "stdout preserves whitespace and quotes",
			status: operation.StatusCompleted,
			state: operation.ShellState{Result: &operation.ShellResult{
				Out: "\n  \"quoted\" \\ output\t\n\n",
			}},
			want: "\n  \"quoted\" \\ output\t\n\n",
		},
		{
			name:   "successful stderr",
			status: operation.StatusCompleted,
			state:  operation.ShellState{Result: &operation.ShellResult{Err: "warning\n"}},
			want:   "Stderr:\nwarning\n",
		},
		{
			name:   "both streams",
			status: operation.StatusCompleted,
			state:  operation.ShellState{Result: &operation.ShellResult{Out: "out\n", Err: "err\n"}},
			want:   "out\n\nStderr:\nerr\n",
		},
		{
			name:   "nonzero exit without output",
			status: operation.StatusCompleted,
			state:  operation.ShellState{Result: &operation.ShellResult{ExitCode: 1}},
			want:   "Exit code: 1",
		},
		{
			name:   "nonzero exit with stderr",
			status: operation.StatusCompleted,
			state: operation.ShellState{Result: &operation.ShellResult{
				Err: "command failed\n", ErrSize: 15, ExitCode: 7,
			}},
			want: "Stderr:\ncommand failed\n\nExit code: 7",
		},
		{
			name:   "truncated stdout",
			status: operation.StatusCompleted,
			state: operation.ShellState{OutTruncated: true, OutPath: "/captures/out", Result: &operation.ShellResult{
				Out: "head...92 bytes truncated; complete output in /captures/out...tail", OutSize: 100,
			}},
			want: "head...92 bytes truncated; complete output in /captures/out...tail",
		},
		{
			name:   "truncated stderr",
			status: operation.StatusCompleted,
			state: operation.ShellState{ErrTruncated: true, ErrPath: "/captures/err", Result: &operation.ShellResult{
				Out: "output", OutSize: 6,
				Err: "head...92 bytes truncated; complete output in /captures/err...tail", ErrSize: 100, ExitCode: 7,
			}},
			want: "output\nStderr:\nhead...92 bytes truncated; complete output in /captures/err...tail\nExit code: 7",
		},
		{
			name:   "ready",
			state:  operation.ShellState{OutTruncated: true, ErrTruncated: true},
			status: operation.StatusReady,
			want:   "Command is still running.",
		},
		{
			name:   "awaiting",
			state:  operation.ShellState{OutTruncated: true, ErrTruncated: true},
			status: operation.StatusAwaiting,
			want:   "Command is still running.",
		},
		{
			name:   "canceling",
			state:  operation.ShellState{OutTruncated: true, ErrTruncated: true},
			status: operation.StatusCanceling,
			want:   "Command is still running.",
		},
		{
			name:   "canceled",
			status: operation.StatusCanceled,
			state:  operation.ShellState{OutTruncated: true, ErrTruncated: true, TerminalError: "canceled by user"},
			want:   "Error: canceled by user",
		},
		{
			name:   "failed",
			status: operation.StatusFailed,
			state:  operation.ShellState{OutTruncated: true, ErrTruncated: true, TerminalError: "process failed"},
			want:   "Error: process failed",
		},
		{
			name:   "failed without explanation",
			status: operation.StatusFailed,
			want:   "Error: shell operation failed",
		},
		{
			name:   "canceled without explanation",
			status: operation.StatusCanceled,
			want:   "Error: shell operation canceled",
		},
		{
			name:   "truncated error",
			status: operation.StatusFailed,
			state:  operation.ShellState{TerminalError: "err...100 bytes truncated...end", ErrorTruncated: true},
			want:   "Error: err...100 bytes truncated...end",
		},
		{
			name:   "failed with captures",
			status: operation.StatusFailed,
			state: operation.ShellState{
				TerminalError: "read failed", OutPath: "/captures/out", ErrPath: "/captures/err",
			},
			want: "Stdout capture: /captures/out\nStderr capture: /captures/err\nError: read failed",
		},
	}
	translator := bash.New(bash.Config{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := json.Marshal(test.state)
			if err != nil {
				t.Fatal(err)
			}
			result, err := translator.TranslateResult("call-1", tool.CallStatus{}, []operation.Operation{{
				ID:      "operation-1",
				Type:    operation.TypeShell,
				Version: operation.VersionShell, MaxOutputLength: operation.DefaultMaxOutputLength,
				Status: test.status,
				State:  state,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if result.CallID != "call-1" {
				t.Fatalf("call ID = %q", result.CallID)
			}
			if result.Output[0].Value != test.want {
				t.Fatalf("result = %s, want %s", result.Output[0].Value, test.want)
			}
			for _, internal := range []string{"secret command", "/secret/path", "operation-1"} {
				if strings.Contains(result.Output[0].Value, internal) {
					t.Fatalf("result exposes internal value %q: %s", internal, result.Output[0].Value)
				}
			}
		})
	}
}

func TestTranslatorTranslatesValidationErrorWithoutOperations(t *testing.T) {
	translator := bash.New(bash.Config{})
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{
		CallID:    "call-1",
		Arguments: `{"command":42}`,
	})
	if status.Error == "" || len(ctx.specs) != 0 {
		t.Fatalf("status = %#v, submitted specs = %d", status, len(ctx.specs))
	}

	result, err := translator.TranslateResult("call-1", status, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.CallID != "call-1" || result.Output[0].Value != "Error: "+status.Error {
		t.Fatalf("result = %#v", result)
	}
}

func TestTranslatorRejectsInvalidOperationCounts(t *testing.T) {
	translator := bash.New(bash.Config{})
	for _, test := range []struct {
		name   string
		status tool.CallStatus
		count  int
		want   string
	}{
		{name: "missing operation", want: "has 0 operations, want 1"},
		{name: "multiple operations", count: 2, want: "has 2 operations, want 1"},
		{name: "validation error with operation", status: tool.CallStatus{Error: "invalid"}, count: 1, want: "both a validation error and operations"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := translator.TranslateResult("call-1", test.status, make([]operation.Operation, test.count))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestTranslatorRejectsInvalidShellOperationResults(t *testing.T) {
	validState, err := json.Marshal(operation.ShellState{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		operations []operation.Operation
		want       string
	}{
		{
			name: "wrong type",
			operations: []operation.Operation{{
				ID: "operation-1", Type: "other", Version: operation.VersionShell, MaxOutputLength: operation.DefaultMaxOutputLength,
			}},
			want: `has type "other", want "shell"`,
		},
		{
			name: "malformed state",
			operations: []operation.Operation{{
				ID: "operation-1", Type: operation.TypeShell, Version: operation.VersionShell, MaxOutputLength: operation.DefaultMaxOutputLength,
				State: []byte(`{`),
			}},
			want: "decode Bash operation",
		},
		{
			name: "invalid status",
			operations: []operation.Operation{{
				ID: "operation-1", Type: operation.TypeShell, Version: operation.VersionShell, MaxOutputLength: operation.DefaultMaxOutputLength,
				Status: "unknown", State: validState,
			}},
			want: "invalid status",
		},
		{
			name: "completed without result",
			operations: []operation.Operation{{
				ID: "operation-1", Type: operation.TypeShell, Version: operation.VersionShell, MaxOutputLength: operation.DefaultMaxOutputLength,
				Status: operation.StatusCompleted, State: validState,
			}},
			want: "completed operation",
		},
	}
	translator := bash.New(bash.Config{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := translator.TranslateResult("call-1", tool.CallStatus{}, test.operations)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestTranslatorAcceptsEmptyCommand(t *testing.T) {
	translator := bash.New(bash.Config{
		Shell:         "/bin/bash",
		BaseDirectory: "/operations",
	})
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{Arguments: `{"command":""}`})

	if status.Error != "" || len(status.WaitingFor) != 1 || len(ctx.specs) != 1 {
		t.Fatalf("status = %#v, submitted specs = %d", status, len(ctx.specs))
	}
}

func TestTranslatorRejectsInvalidArguments(t *testing.T) {
	translator := bash.New(bash.Config{
		Shell:         "/bin/bash",
		BaseDirectory: "/operations",
	})
	tests := []struct {
		name      string
		arguments string
		want      string
	}{
		{name: "malformed JSON", arguments: `{`, want: "decode Bash arguments: jsontext: unexpected EOF"},
		{name: "duplicate command", arguments: `{"command":"pwd","command":"ls"}`, want: `duplicate object member name "command"`},
		{name: "missing command", arguments: `{}`, want: `bash argument "command" must be set`},
		{name: "uppercase command", arguments: `{"COMMAND":"pwd"}`, want: `bash argument "command" must be set`},
		{name: "null command", arguments: `{"command":null}`, want: `bash argument "command" must be a string`},
		{name: "non-string command", arguments: `{"command":42}`, want: `decode Bash argument "command": json:`},
		{name: "NUL at start", arguments: `{"command":"\u0000pwd"}`, want: `bash argument "command" contains a NUL byte at offset 0`},
		{name: "NUL at end", arguments: `{"command":"pwd\u0000"}`, want: `bash argument "command" contains a NUL byte at offset 3`},
		{name: "first of multiple NULs", arguments: `{"command":"a\u0000b\u0000"}`, want: `bash argument "command" contains a NUL byte at offset 1`},
		{name: "NUL after multibyte text", arguments: `{"command":"é雪\u0000"}`, want: `bash argument "command" contains a NUL byte at offset 5`},
		{name: "NUL in heredoc", arguments: `{"command":"cat <<'EOF'\ncaf\u0000e9\nEOF\n"}`, want: `bash argument "command" contains a NUL byte at offset 15`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := &recordingContext{}
			status := translator.Translate(ctx, llm.ToolCall{Arguments: test.arguments})
			if !strings.Contains(status.Error, test.want) {
				t.Fatalf("error = %q, want substring %q", status.Error, test.want)
			}
			if len(status.WaitingFor) != 0 || len(ctx.specs) != 0 {
				t.Fatalf("status = %#v, submitted specs = %d", status, len(ctx.specs))
			}
		})
	}
}

func TestTranslatorPreservesEscapedNULCommands(t *testing.T) {
	translator := bash.New(bash.Config{Shell: "/bin/bash", BaseDirectory: "/operations"})
	for _, command := range []string{
		"cat <<'EOF'\ncaf\\x00e9\nEOF\n",
		`printf 'a\000b'`,
		`printf '%s' '\u0000'`,
	} {
		t.Run(command, func(t *testing.T) {
			arguments, err := json.Marshal(map[string]string{"command": command})
			if err != nil {
				t.Fatal(err)
			}
			ctx := &recordingContext{}
			status := translator.Translate(ctx, llm.ToolCall{Arguments: string(arguments)})
			if status.Error != "" || len(status.WaitingFor) != 1 || len(ctx.specs) != 1 {
				t.Fatalf("status = %#v, submitted specs = %d", status, len(ctx.specs))
			}
			var state operation.ShellState
			if err := json.Unmarshal(ctx.specs[0].State, &state); err != nil {
				t.Fatal(err)
			}
			if state.Input.Command != command {
				t.Fatalf("command = %q, want %q", state.Input.Command, command)
			}
		})
	}
}

func TestTranslatorReturnsShellConfigurationError(t *testing.T) {
	translator := bash.New(bash.Config{Shell: "bash", BaseDirectory: "/operations"})
	ctx := &recordingContext{}
	status := translator.Translate(ctx, llm.ToolCall{Arguments: `{"command":"pwd"}`})

	if status.Error != "build Bash operation: shell path must be absolute" {
		t.Fatalf("error = %q", status.Error)
	}
	if len(ctx.specs) != 0 {
		t.Fatalf("submitted specs = %d, want 0", len(ctx.specs))
	}
}
