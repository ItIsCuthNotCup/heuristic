package responsesapi

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/internal/openaiapi"
)

func decodeResponse(body []byte) (llm.Response, error) {
	var envelope struct {
		Usage jsontext.Value `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return llm.Response{}, fmt.Errorf("decode response: %w", err)
	}
	var source openaiapi.Response
	if err := json.Unmarshal(body, &source); err != nil {
		return llm.Response{}, fmt.Errorf("decode response: %w", err)
	}
	converted, err := response(source)
	if err != nil {
		return llm.Response{}, err
	}
	converted.Usage.Raw = envelope.Usage
	return converted, nil
}

func response(source openaiapi.Response) (llm.Response, error) {
	if source.Status == nil {
		return llm.Response{}, fmt.Errorf("unsupported response status %q", "")
	}
	converted := llm.Response{
		ID:     source.Id,
		Output: make([]llm.Item, 0, len(source.Output)),
		Usage:  responseUsage(source.Usage),
	}
	switch *source.Status {
	case openaiapi.ResponseStatusCompleted:
		converted.Stop = llm.StopComplete
	case openaiapi.ResponseStatusIncomplete:
		stop, err := incompleteStopReason(source)
		if err != nil {
			return llm.Response{}, err
		}
		converted.Stop = stop
	case openaiapi.ResponseStatusFailed:
		failure := responseFailure(source)
		converted.Failure = &failure
	default:
		return llm.Response{}, fmt.Errorf("unsupported response status %q", *source.Status)
	}
	for index, output := range source.Output {
		item, included, err := responseOutputItem(output)
		if err != nil {
			return llm.Response{}, fmt.Errorf("output item %d: %w", index, err)
		}
		if !included {
			continue
		}
		converted.Output = append(converted.Output, item)
	}
	return converted, nil
}

func responseOutputItem(source openaiapi.OutputItem) (llm.Item, bool, error) {
	itemType, err := source.Discriminator()
	if err != nil {
		return llm.Item{}, false, fmt.Errorf("decode output item type: %w", err)
	}

	switch itemType {
	case "web_search_call":
		return llm.Item{}, false, nil
	case "message":
		message, err := source.AsOutputMessage()
		if err != nil {
			return llm.Item{}, false, err
		}
		var text strings.Builder
		for _, content := range message.Content {
			contentType, err := content.Discriminator()
			if err != nil {
				return llm.Item{}, false, fmt.Errorf("decode message content type: %w", err)
			}
			switch contentType {
			case "output_text":
				part, err := content.AsOutputTextContent()
				if err != nil {
					return llm.Item{}, false, err
				}
				text.WriteString(part.Text)
			case "refusal":
				part, err := content.AsRefusalContent()
				if err != nil {
					return llm.Item{}, false, err
				}
				text.WriteString(part.Refusal)
			default:
				return llm.Item{}, false, fmt.Errorf("unsupported message content type %q", contentType)
			}
		}
		return llm.Item{
			ProviderID: message.Id,
			Type:       llm.ItemMessage,
			Data: llm.Message{
				Role:  llm.RoleAssistant,
				Text:  text.String(),
				Phase: dereference(message.Phase),
			},
		}, true, nil
	case "function_call":
		call, err := source.AsFunctionToolCall()
		if err != nil {
			return llm.Item{}, false, err
		}
		if call.Status != nil {
			switch *call.Status {
			case openaiapi.FunctionToolCallStatusInProgress,
				openaiapi.FunctionToolCallStatusIncomplete:
				return llm.Item{}, false, nil
			}
		}
		return llm.Item{
			ProviderID: dereference(call.Id),
			Type:       llm.ItemToolCall,
			Data: llm.ToolCall{
				CallID:    call.CallId,
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		}, true, nil
	case "reasoning":
		reasoning, err := source.AsReasoningItem()
		if err != nil {
			return llm.Item{}, false, err
		}
		raw, err := source.MarshalJSON()
		if err != nil {
			return llm.Item{}, false, err
		}
		summary := make([]string, len(reasoning.Summary))
		for index, part := range reasoning.Summary {
			summary[index] = part.Text
		}
		return llm.Item{
			ProviderID: reasoning.Id,
			Type:       llm.ItemReasoning,
			Data: llm.Reasoning{
				Summary: summary,
				Raw:     raw,
			},
		}, true, nil
	default:
		return llm.Item{}, false, fmt.Errorf("unsupported output item type %q", itemType)
	}
}

func incompleteStopReason(source openaiapi.Response) (llm.StopReason, error) {
	if source.IncompleteDetails == nil || source.IncompleteDetails.Reason == nil {
		return "", fmt.Errorf("unsupported incomplete reason %q", "")
	}
	switch *source.IncompleteDetails.Reason {
	case openaiapi.MaxOutputTokens:
		return llm.StopMaxOutputTokens, nil
	case openaiapi.ContentFilter:
		return llm.StopRefused, nil
	default:
		return "", fmt.Errorf("unsupported incomplete reason %q", *source.IncompleteDetails.Reason)
	}
}

func responseFailure(source openaiapi.Response) llm.Failure {
	if source.Error == nil {
		return llm.Failure{Message: "response failed"}
	}
	return llm.Failure{
		Code:    string(source.Error.Code),
		Message: source.Error.Message,
	}
}

func responseUsage(source *openaiapi.ResponseUsage) llm.Usage {
	if source == nil {
		return llm.Usage{}
	}
	return llm.Usage{
		InputTokens:           int64(source.InputTokens),
		CachedInputTokens:     int64(source.InputTokensDetails.CachedTokens),
		CacheWriteInputTokens: int64(source.InputTokensDetails.CacheWriteTokens),
		OutputTokens:          int64(source.OutputTokens),
		ReasoningTokens:       int64(source.OutputTokensDetails.ReasoningTokens),
	}
}

func dereference[T ~string](value *T) string {
	if value == nil {
		return ""
	}
	return string(*value)
}
