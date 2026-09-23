package tool

import (
	"fmt"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/operation"
)

type unavailableTranslator struct {
	name string
}

func (translator unavailableTranslator) Translate(Context, llm.ToolCall) CallStatus {
	return CallStatus{Error: translator.errorMessage()}
}

func (translator unavailableTranslator) TranslateResult(
	callID string,
	_ CallStatus,
	_ []operation.Operation,
) (llm.ToolResult, error) {
	return llm.ToolResult{CallID: callID, Output: []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: translator.errorMessage()}}}, nil
}

func (translator unavailableTranslator) errorMessage() string {
	return fmt.Sprintf("static tool %q is not configured", translator.name)
}

func StaticNames() []string {
	definitions := staticDefinitions()
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Tool.Name)
	}
	return names
}

func staticDefinitions() []Definition {
	return []Definition{
		{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        BashName,
			Description: "Execute a shell command in background. Independent commands may be issued as parallel tool calls in one turn. Command child processes are killed when the shell exits.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command to execute.",
					},
					"max_output_length": maxOutputLengthSchema(),
				},
				"required": []any{"command"},
			},
		}},
		{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        ViewImageName,
			Description: "View a local JPEG, PNG, BMP, TIFF, or WebP image.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Image file path, absolute or relative to the workspace.",
					},
				},
				"required": []any{"path"},
			},
		}},
		{Tool: llm.Tool{
			Type:        llm.ToolFunction,
			Name:        SkillUseName,
			Description: "Load the instructions for a registered skill.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The exact name of the skill to load.",
					},
				},
				"required": []any{"name"},
			},
		}},
	}
}

func maxOutputLengthSchema() map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": fmt.Sprintf("Maximum characters per output text field. Truncated text keeps its head and tail, around a marker stating how much was omitted, and path to the file with the complete stream. Defaults to %d.", operation.DefaultMaxOutputLength),
		"minimum":     1,
		"maximum":     operation.MaxOutputLength,
		"default":     operation.DefaultMaxOutputLength,
	}
}
