package openai

import (
	"errors"
	"strings"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/responsesapi"
	"github.com/ItIsCuthNotCup/heuristic/harness/primitives"
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
		return nil, errors.New("OpenAI API key must be set")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("OpenAI base URL must be set")
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
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{UsePromptCacheKeyField: true},
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{Adapter: adapter, remote: remote}, nil
}

func (client *Client) Close() error {
	return client.remote.Close()
}
