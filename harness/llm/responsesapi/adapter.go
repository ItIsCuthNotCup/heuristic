package responsesapi

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/primitives"
)

const remoteSource primitives.SourceID = "llm.responsesapi"

const DefaultMaxAttempts = primitives.DefaultRemoteMaxAttempts

type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Param      string
	Type       string
}

func (err *APIError) Error() string {
	if err.Code != "" {
		return fmt.Sprintf("responses API error %s: %s", err.Code, err.Message)
	}
	if err.StatusCode != 0 {
		return fmt.Sprintf("responses API request failed with status %d: %s", err.StatusCode, err.Message)
	}
	return "responses API request failed: " + err.Message
}

type Exchange struct {
	RequestBody  []byte
	StatusCode   int
	ResponseBody []byte
}

type CacheKeyPlacement struct {
	Header                 string
	UsePromptCacheKeyField bool
}

type Config struct {
	Endpoint          string
	Headers           map[string][]string
	CacheKeyPlacement CacheKeyPlacement
	// Nil uses DefaultMaxAttempts.
	MaxAttempts *int
	// Trace borrows read-only bodies: request JSON and terminal response JSON or HTTP error.
	Trace func(Exchange)
	// Extensions are provider-specific top-level fields merged into every request
	// body, such as OpenRouter's automatic prompt caching and upstream routing.
	// A key that names a standard Responses API field is rejected per request.
	Extensions map[string]jsontext.Value
}

type adapter struct {
	remote            *primitives.RemoteClient
	endpoint          string
	headers           map[string][]string
	trace             func(Exchange)
	cacheKeyPlacement CacheKeyPlacement
	maxAttempts       int
	extensions        map[string]jsontext.Value
}

var _ llm.Adapter = (*adapter)(nil)

func NewAdapter(remote *primitives.RemoteClient, config Config) (llm.Adapter, error) {
	if remote == nil {
		return nil, errors.New("remote client must be set")
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, errors.New("responses API endpoint must be set")
	}
	maxAttempts := DefaultMaxAttempts
	if config.MaxAttempts != nil {
		maxAttempts = *config.MaxAttempts
	}
	if maxAttempts <= 0 {
		return nil, errors.New("max attempts must be positive")
	}
	return &adapter{
		remote:            remote,
		endpoint:          config.Endpoint,
		headers:           config.Headers,
		trace:             config.Trace,
		cacheKeyPlacement: config.CacheKeyPlacement,
		maxAttempts:       maxAttempts,
		extensions:        config.Extensions,
	}, nil
}

func (adapter *adapter) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	key := ""
	if options.CacheKey != "" {
		key = fmt.Sprintf("%x", sha256.Sum256([]byte(options.CacheKey)))
	}
	promptCacheKey := ""
	if adapter.cacheKeyPlacement.UsePromptCacheKeyField {
		promptCacheKey = key
	}
	body, err := requestBody(request, promptCacheKey, adapter.extensions)
	if err != nil {
		return llm.Response{}, err
	}
	statusCode, responseBody, err := adapter.exchange(ctx, body, key)
	if err != nil {
		return llm.Response{}, err
	}
	if adapter.trace != nil {
		adapter.trace(Exchange{RequestBody: body, StatusCode: statusCode, ResponseBody: responseBody})
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return llm.Response{}, fmt.Errorf("create response: %w", providerError(statusCode, responseBody))
	}
	return decodeResponse(responseBody)
}

const modelResponseIdleTimeout = 30 * time.Minute
const maxSSEFrameBytes = 256 << 20

func (adapter *adapter) remoteRequest(body []byte, cacheKey string) primitives.RemoteRequest {
	correlationID := primitives.CorrelationID(uuid.New().String())
	request := primitives.DefaultRemoteRequest(remoteSource, correlationID, adapter.endpoint)
	request.Method = http.MethodPost
	request.Body = body
	request.Headers = make(map[string][]string, len(adapter.headers)+1)
	for name, values := range adapter.headers {
		if strings.EqualFold(name, "Accept") {
			continue
		}
		request.Headers[name] = values
	}
	request.Headers["Accept"] = []string{"text/event-stream"}
	if adapter.cacheKeyPlacement.Header != "" && cacheKey != "" {
		request.Headers[adapter.cacheKeyPlacement.Header] = []string{cacheKey}
	}
	// Reasoning can produce multi-minute gaps between events.
	request.ResponseIdleTimeout = modelResponseIdleTimeout
	request.RetryPolicy.MaxAttempts = 1
	request.SSE = &primitives.RemoteSSEOptions{
		MaxFrameSize:   maxSSEFrameBytes,
		FrameDelimiter: primitives.SSEFrameDelimiterStrip,
	}
	return request
}

func providerError(statusCode int, body []byte) *APIError {
	var envelope struct {
		Error struct {
			Code    *string `json:"code"`
			Message string  `json:"message"`
			Param   *string `json:"param"`
			Type    string  `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && (envelope.Error.Message != "" || dereference(envelope.Error.Code) != "") {
		return &APIError{
			StatusCode: statusCode,
			Code:       dereference(envelope.Error.Code),
			Message:    envelope.Error.Message,
			Param:      dereference(envelope.Error.Param),
			Type:       envelope.Error.Type,
		}
	}

	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return &APIError{StatusCode: statusCode, Message: message}
}

func remoteFailureError(ctx context.Context, event primitives.PrimitiveEvent) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure, ok := event.Result.(primitives.PrimitiveFailureResult)
	if !ok {
		return errors.New("remote request failed with an invalid result")
	}
	return errors.New(failure.Error)
}

func canceledError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return context.Canceled
}
