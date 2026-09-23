package bash

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

type Config struct {
	Shell         string
	Directory     string
	BaseDirectory string
}

type translator struct {
	config Config
}

func New(config Config) tool.Translator {
	return &translator{config: config}
}

func (translator *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	command, limit, err := validateArguments(call.Arguments)
	if err != nil {
		return tool.ErrorStatus(err.Error(), limit)
	}
	spec, err := translator.buildOperation(command, limit)
	if err != nil {
		return tool.ErrorStatus(err.Error(), limit)
	}

	id := ctx.Submit(spec)
	return tool.CallStatus{WaitingFor: []operation.ID{id}}
}

func (translator *translator) TranslateResult(
	callID string,
	status tool.CallStatus,
	operations []operation.Operation,
) (llm.ToolResult, error) {
	if status.Error != "" {
		if len(operations) != 0 {
			return llm.ToolResult{}, fmt.Errorf("bash tool call %q has both a validation error and operations", callID)
		}
		return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Error: " + status.Error}}}, nil
	}
	if len(operations) != 1 {
		return llm.ToolResult{}, fmt.Errorf("bash tool call %q has %d operations, want 1", callID, len(operations))
	}

	output, err := translateOperationResult(callID, operations[0])
	if err != nil {
		return llm.ToolResult{}, err
	}
	return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: output}}}, nil
}

func translateOperationResult(
	callID string,
	current operation.Operation,
) (string, error) {
	if current.Type != operation.TypeShell {
		return "", fmt.Errorf(
			"bash tool call %q operation %q has type %q, want %q",
			callID,
			current.ID,
			current.Type,
			operation.TypeShell,
		)
	}

	state, err := operation.DecodeShellState(current)
	if err != nil {
		return "", fmt.Errorf("decode Bash operation %q state: %w", current.ID, err)
	}
	switch current.Status {
	case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
		return "Command is still running.", nil
	case operation.StatusCompleted:
		if state.Result == nil {
			return "", fmt.Errorf("bash tool call %q completed operation %q has no result", callID, current.ID)
		}
	case operation.StatusFailed, operation.StatusCanceled:
		if state.TerminalError == "" {
			state.TerminalError = "shell operation " + string(current.Status)
		}
	default:
		return "", fmt.Errorf("bash tool call %q operation %q has invalid status %q", callID, current.ID, current.Status)
	}

	var parts []string
	if state.Result != nil {
		if state.Result.Out != "" {
			parts = append(parts, state.Result.Out)
		}
		if state.Result.Err != "" {
			parts = append(parts, "Stderr:\n"+state.Result.Err)
		}
		if state.Result.ExitCode != 0 {
			parts = append(parts, fmt.Sprintf("Exit code: %d", state.Result.ExitCode))
		}
	} else {
		if state.OutPath != "" {
			parts = append(parts, "Stdout capture: "+state.OutPath)
		}
		if state.ErrPath != "" {
			parts = append(parts, "Stderr capture: "+state.ErrPath)
		}
	}
	if state.TerminalError != "" {
		parts = append(parts, "Error: "+state.TerminalError)
	}
	if len(parts) == 0 {
		return "(no output)", nil
	}
	return strings.Join(parts, "\n"), nil
}

func validateArguments(encoded string) (string, int, error) {
	var arguments map[string]jsontext.Value
	if err := json.Unmarshal([]byte(encoded), &arguments); err != nil {
		return "", 0, fmt.Errorf("decode Bash arguments: %w", err)
	}
	limit, err := tool.ParseMaxOutputLength(arguments["max_output_length"])
	if err != nil {
		return "", 0, fmt.Errorf("bash argument: %w", err)
	}
	encodedCommand, exists := arguments["command"]
	if !exists {
		return "", limit, errors.New(`bash argument "command" must be set`)
	}
	var command *string
	if err := json.Unmarshal(encodedCommand, &command); err != nil {
		return "", limit, fmt.Errorf(`decode Bash argument "command": %w`, err)
	}
	if command == nil {
		return "", limit, errors.New(`bash argument "command" must be a string`)
	}
	if offset := strings.IndexByte(*command, 0); offset >= 0 {
		return "", limit, fmt.Errorf(`bash argument "command" contains a NUL byte at offset %d`, offset)
	}
	return *command, limit, nil
}

func (translator *translator) buildOperation(command string, limit int) (operation.Spec, error) {
	spec, err := operation.NewShellSpec(operation.ShellInput{
		Command:   command,
		Shell:     translator.config.Shell,
		Directory: translator.config.Directory,
	}, translator.config.BaseDirectory, limit)
	if err != nil {
		return operation.Spec{}, fmt.Errorf("build Bash operation: %w", err)
	}
	return spec, nil
}
