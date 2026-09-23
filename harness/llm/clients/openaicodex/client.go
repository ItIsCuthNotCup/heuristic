package openaicodex

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm/responsesapi"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

const BaseURL = "https://chatgpt.com/backend-api/codex"

type Config struct {
	AccessToken string
	AccountID   string
	// Read once at construction; exclusive with inline credentials.
	AuthFile    string
	BaseURL     string
	MaxAttempts *int
	Trace       func(responsesapi.Exchange)
}

type Client struct {
	adapter llm.Adapter
	remote  *primitives.RemoteClient
}

var _ llm.Adapter = (*Client)(nil)

func NewClient(config Config) (*Client, error) {
	baseURL, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if config.MaxAttempts != nil && *config.MaxAttempts <= 0 {
		return nil, errors.New("max attempts must be positive")
	}
	credentials, err := config.credentials()
	if err != nil {
		return nil, err
	}
	// Never forward subscription credentials through redirects.
	remote := primitives.NewRemoteClientWithHTTPClient(&http.Client{
		Transport:     http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	})
	adapter, err := responsesapi.NewAdapter(remote, responsesapi.Config{
		Endpoint: baseURL + "/responses",
		Headers: map[string][]string{
			"Authorization":      {"Bearer " + credentials.accessToken},
			"ChatGPT-Account-ID": {credentials.accountID},
			"Content-Type":       {"application/json"},
			"originator":         {"metacog-agent"},
			"User-Agent":         {"metacog-agent"},
		},
		CacheKeyPlacement: responsesapi.CacheKeyPlacement{UsePromptCacheKeyField: true, Header: "session-id"},
		MaxAttempts:       config.MaxAttempts,
		Trace:             config.Trace,
	})
	if err != nil {
		_ = remote.Close()
		return nil, err
	}
	return &Client{adapter: adapter, remote: remote}, nil
}

func (client *Client) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if request.Model.MaxOutputTokens != nil {
		return llm.Response{}, errors.New("codex does not support max_output_tokens")
	}
	response, err := client.adapter.Respond(ctx, request, options)
	var apiErr *responsesapi.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		return llm.Response{}, fmt.Errorf("codex credentials rejected; renew them externally and recreate the client: %w", err)
	}
	return response, err
}

func (client *Client) Close() error { return client.remote.Close() }

func validateBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" || value == BaseURL {
		return BaseURL, nil
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") {
		ip := net.ParseIP(parsed.Hostname())
		if ip != nil && ip.IsLoopback() {
			return value, nil
		}
	}
	return "", errors.New("codex base URL must be https://chatgpt.com/backend-api/codex or an explicit loopback IP endpoint")
}
