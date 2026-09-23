package fireworks

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/responsesapi"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

type Config struct {
	APIKey      string
	BaseURL     string
	MaxAttempts *int
	Trace       func(Exchange)
}

type Exchange = responsesapi.Exchange

type Client struct {
	llm.Adapter
	remote *primitives.RemoteClient
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("fireworks API key must be set")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("fireworks base URL must be set")
	}

	remote := primitives.NewRemoteClient()
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: baseURL + "/responses",
		Headers: map[string][]string{
			"Authorization": {"Bearer " + config.APIKey},
			"Content-Type":  {"application/json"},
		},
		Trace:             config.Trace,
		MaxAttempts:       config.MaxAttempts,
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{Header: "x-session-affinity"},
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{Adapter: adapter, remote: remote}, nil
}

func (client *Client) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	response, err := client.Adapter.Respond(ctx, request, options)
	if err != nil {
		return llm.Response{}, err
	}
	if len(response.Usage.Raw) == 0 {
		return response, nil
	}

	var usage struct {
		PromptTokens        *int64 `json:"prompt_tokens"`
		CompletionTokens    *int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(response.Usage.Raw, &usage); err != nil {
		return llm.Response{}, fmt.Errorf("decode Fireworks response usage: %w", err)
	}
	if usage.PromptTokens != nil {
		response.Usage.InputTokens = *usage.PromptTokens
	}
	if usage.CompletionTokens != nil {
		response.Usage.OutputTokens = *usage.CompletionTokens
	}
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens != nil {
		response.Usage.CachedInputTokens = *usage.PromptTokensDetails.CachedTokens
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens != nil {
		response.Usage.ReasoningTokens = *usage.CompletionTokensDetails.ReasoningTokens
	}
	return response, nil
}

func (client *Client) Close() error {
	return client.remote.Close()
}
