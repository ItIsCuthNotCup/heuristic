package viewimage

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

const (
	DefaultMaxSize   = 5_000_000 - 1_000
	DefaultMaxWidth  = 2000
	DefaultMaxHeight = 2000
)

type Config struct {
	Directory string
	Limits    operation.ViewImageConfig
}

type translator struct {
	config Config
}

func New(config Config) tool.Translator {
	if config.Limits.MaxSize == 0 {
		config.Limits.MaxSize = DefaultMaxSize
	}
	if config.Limits.MaxWidth == 0 {
		config.Limits.MaxWidth = DefaultMaxWidth
	}
	if config.Limits.MaxHeight == 0 {
		config.Limits.MaxHeight = DefaultMaxHeight
	}
	return &translator{config: config}
}

func (translator *translator) Translate(ctx tool.Context, call llm.ToolCall) tool.CallStatus {
	var arguments struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil {
		return tool.ErrorStatus(fmt.Sprintf("decode ViewImage arguments: %v", err), operation.DefaultMaxOutputLength)
	}
	if strings.TrimSpace(arguments.Path) == "" {
		return tool.CallStatus{Error: `ViewImage argument "path" must be set`}
	}
	if offset := strings.IndexByte(arguments.Path, 0); offset >= 0 {
		return tool.CallStatus{Error: fmt.Sprintf(`ViewImage argument "path" contains a NUL byte at offset %d`, offset)}
	}
	path := arguments.Path
	if translator.config.Directory != "" && !filepath.IsAbs(path) {
		// Preserve .. for filesystem resolution across symlinks; filepath.Join would clean it.
		path = translator.config.Directory + string(filepath.Separator) + path
	}
	spec, err := operation.NewViewImageSpec(path, translator.config.Limits)
	if err != nil {
		return tool.CallStatus{Error: fmt.Sprintf("build ViewImage operation: %v", err)}
	}
	return tool.CallStatus{WaitingFor: []operation.ID{ctx.Submit(spec)}}
}

func (translator *translator) TranslateResult(
	callID string,
	status tool.CallStatus,
	operations []operation.Operation,
) (llm.ToolResult, error) {
	result := llm.ToolResult{CallID: callID}
	if status.Error != "" {
		if len(operations) != 0 {
			return result, fmt.Errorf("ViewImage call %q has both a validation error and operations", callID)
		}
		result.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "Error: " + status.Error}}
		return result, nil
	}
	if len(operations) != 1 {
		return result, fmt.Errorf("ViewImage call %q has %d operations, want 1", callID, len(operations))
	}
	current := operations[0]
	state, err := operation.DecodeViewImageState(current)
	if err != nil {
		return result, fmt.Errorf("decode ViewImage call %q result: %w", callID, err)
	}
	var details []string
	switch current.Status {
	case operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling:
		details = append(details, "Image is still loading.")
	case operation.StatusFailed, operation.StatusCanceled:
		message := "view-image operation " + string(current.Status)
		if state.Result != nil {
			if state.Result.Error != "" {
				message = state.Result.Error
			}
			details = append(details, imageMetadata(*state.Result, true)...)
		}
		details = append([]string{"Error: " + message}, details...)
	case operation.StatusCompleted:
		if state.Result == nil || state.Result.Content == "" || state.Result.EncodedMIMEType == "" ||
			state.Result.ScaleRatio <= 0 || state.Result.ScaleRatio > 1 || state.Result.Error != "" {
			return result, fmt.Errorf("ViewImage call %q completed operation %q has an invalid image result", callID, current.ID)
		}
		result.Output = append(result.Output, llm.ToolResultOutput{
			Kind: llm.ToolResultImage, Value: "data:" + state.Result.EncodedMIMEType + ";base64," + state.Result.Content,
		})
		details = imageMetadata(*state.Result, false)
		if state.Result.ScaleRatio < 1 {
			details = append(details, fmt.Sprintf("multiply coordinates by %.2f to approximate original", 1/state.Result.ScaleRatio))
		}
	default:
		return result, fmt.Errorf("ViewImage call %q operation %q has invalid status %q", callID, current.ID, current.Status)
	}
	if len(details) != 0 {
		result.Output = append(result.Output, llm.ToolResultOutput{Kind: llm.ToolResultText, Value: strings.Join(details, "; ")})
	}
	return result, nil
}

func imageMetadata(result operation.ViewImageResult, failed bool) []string {
	var details []string
	if result.OriginalMIMEType != "" && (failed || result.OriginalMIMEType != result.EncodedMIMEType) {
		details = append(details, "original MIME type: "+result.OriginalMIMEType)
	}
	if result.OriginalWidth > 0 && result.OriginalHeight > 0 && (failed || result.ScaleRatio < 1) {
		details = append(details, fmt.Sprintf("original dimensions: %dx%d", result.OriginalWidth, result.OriginalHeight))
	}
	return details
}
