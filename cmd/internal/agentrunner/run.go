package agentrunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/ItIsCuthNotCup/heuristic/harness/contextbuilder"
	"github.com/ItIsCuthNotCup/heuristic/harness/coordinator"
	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/responsesapi"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore/localfile"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool/bash"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool/viewimage"
)

const (
	defaultProvider           = "openai"
	defaultSessionDirectory   = ".harness/sessions"
	llmAPIKeyEnvironment      = "UNREAL_HARNESS_LLM_API_KEY"
	llmBaseURLEnvironment     = "UNREAL_HARNESS_LLM_BASE_URL"
	llmModelEnvironment       = "UNREAL_HARNESS_LLM_MODEL"
	llmProviderEnvironment    = "UNREAL_HARNESS_LLM_PROVIDER"
	llmMaxAttemptsEnvironment = "UNREAL_HARNESS_LLM_MAX_ATTEMPTS"
)

// lookupEnv reads the HEURISTIC_ equivalent of an environment name first,
// then falls back to the upstream name so existing deployments keep working:
// lookupEnv(getenv, "UNREAL_HARNESS_LLM_MODEL") checks
// HEURISTIC_LLM_MODEL, then UNREAL_HARNESS_LLM_MODEL.
func lookupEnv(getenv func(string) string, name string) string {
	suffix, _ := strings.CutPrefix(name, "UNREAL_HARNESS_")
	if value := strings.TrimSpace(getenv("HEURISTIC_" + suffix)); value != "" {
		return value
	}
	return getenv(name)
}

const defaultSystemPrompt = `You are an AI agent running inside an isolated sandbox container.

## Guidelines
- Save output files to the workspace root.
- For large datasets, inspect a sample first before processing everything.
`

type Client interface {
	llm.Adapter
	Close() error
}

type Provider struct {
	Name              string
	BaseURL           string
	DefaultModel      string
	APIKeyEnvironment string // Empty delegates authentication to NewClient.
	NewClient         func(apiKey, baseURL string, maxAttempts int, getenv func(string) string) (Client, error)
}

type Request struct {
	Messages               []RequestMessage `json:"messages"`
	Prompt                 *string          `json:"prompt"`
	SystemPrompt           *string          `json:"system_prompt"`
	Model                  string           `json:"model"`
	MaxAttempts            *int             `json:"max_attempts"`
	SessionID              *string          `json:"session_id"`
	ThinkingLevel          string           `json:"thinking_level"`
	IncludePartialMessages *bool            `json:"include_partial_messages"`
	ExtraAllowedTools      []string         `json:"extra_allowed_tools"`
	DisallowedTools        []string         `json:"disallowed_tools"`
	MetaCog                *MetaCogRequest  `json:"metacog"`
}

type RequestMessage struct {
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	MessageID *string `json:"message_id"`
}

type environmentChange struct {
	name    string
	value   string
	present bool
}

type environmentScope struct {
	changes []environmentChange
}

type errorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type sessionObserver struct {
	sessionID session.ID
	output    io.Writer
	cancel    context.CancelFunc

	mu  sync.Mutex
	err error
}

func RunMain(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	environ func() []string,
	input io.Reader,
	output io.Writer,
	stderr io.Writer,
	config Config,
) int {
	err := Run(ctx, args, getenv, environ, input, output, stderr, config)
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if context.Cause(ctx) != nil {
		return 130
	}
	if !config.Interactive {
		encoded, encodeErr := json.Marshal(errorEvent{Type: "error", Message: err.Error()})
		if encodeErr != nil {
			err = errors.Join(err, fmt.Errorf("encode error event: %w", encodeErr))
		} else if _, writeErr := fmt.Fprintf(output, "%s\n", encoded); writeErr != nil {
			err = errors.Join(err, fmt.Errorf("write error event: %w", writeErr))
		}
	}
	prefix := ""
	if config.Name != "" {
		prefix = config.Name + ": "
	}
	if _, writeErr := fmt.Fprintf(stderr, "%s%v\n", prefix, err); writeErr != nil {
		return 1
	}
	return 1
}

func Run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	environ func() []string,
	input io.Reader,
	output io.Writer,
	flagOutput io.Writer,
	config Config,
) (runErr error) {
	if strings.TrimSpace(config.Name) == "" {
		return errors.New("runner name must be set")
	}
	if config.ParseRequest == nil {
		return errors.New("request parser must be set")
	}
	flags := flag.NewFlagSet(config.Name, flag.ContinueOnError)
	flags.SetOutput(flagOutput)
	var usageErr error
	flags.Usage = func() {
		usageErr = writeUsage(flags, config.Interactive)
	}
	var prompt *string
	flags.Func("p", "send a request with the given `prompt` without reading stdin", func(value string) error {
		prompt = &value
		return nil
	})
	sessionDirectory := flags.String("session-directory", defaultSessionDirectory, "directory containing session files")
	workspaceDirectory := flags.String("workspace", ".", "agent workspace and Bash working directory")
	logDirectory := flags.String("log-directory", "", "session JSONL log directory; defaults to <workspace>/logs")
	toolHeartbeatInterval := flags.Duration("tool-heartbeat-interval", 10*time.Minute, "tool-wait heartbeat interval (0 disables)")
	metacogMode := flags.String("metacog", "", "metacognition mode: off|final|all (off disables the judge wrapper)")
	var resumeID *string
	if config.Interactive {
		resumeID = flags.String("resume", "", "resume the session with this ID")
	}
	if err := flags.Parse(args); err != nil {
		if usageErr != nil {
			if errors.Is(err, flag.ErrHelp) {
				return usageErr
			}
			return errors.Join(err, usageErr)
		}
		return err
	}
	if flags.NArg() > 1 && !config.Interactive {
		return errors.New("expected at most one positional JSON request")
	}
	if prompt != nil && flags.NArg() != 0 {
		return errors.New("-p cannot be combined with a positional JSON request")
	}
	if *toolHeartbeatInterval < 0 {
		return errors.New("tool heartbeat interval must not be negative")
	}
	if *metacogMode != "" {
		if _, err := metacog.ParseMode(*metacogMode); err != nil {
			return err
		}
	}

	var parsed Request
	var newTools ToolFactory
	var err error
	if config.Interactive {
		// Positional args join into a plain-text prompt; stdin is the
		// follow-up input channel, not a JSON request.
		initial := ""
		if prompt != nil {
			initial = *prompt
		} else if flags.NArg() > 0 {
			initial = strings.Join(flags.Args(), " ")
		}
		if initial != "" {
			parsed.Prompt = &initial
		}
		if *resumeID != "" {
			parsed.SessionID = resumeID
		}
		newTools = defaultToolFactory(parsed)
	} else {
		if prompt != nil {
			encoded, err := json.Marshal(struct {
				Prompt string `json:"prompt"`
			}{Prompt: *prompt})
			if err != nil {
				return fmt.Errorf("encode prompt request: %w", err)
			}
			input = bytes.NewReader(encoded)
		} else if flags.NArg() == 1 {
			input = strings.NewReader(flags.Arg(0))
		}
		parsed, newTools, err = config.ParseRequest(input)
		if err != nil {
			return err
		}
	}
	if newTools == nil {
		return errors.New("request parser returned no tool factory")
	}
	var messages []RequestMessage
	if config.Interactive && parsed.Prompt == nil && parsed.Messages == nil {
		// Interactive start with no initial message: wait for stdin.
	} else if messages, err = validateRequest(parsed); err != nil {
		return err
	}
	workspace, err := filepath.Abs(strings.TrimSpace(*workspaceDirectory))
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("inspect workspace: %w", err)
	}
	if !workspaceInfo.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", workspace)
	}
	environment, err := loadDotEnv(filepath.Join(workspace, ".env"))
	if err != nil {
		return err
	}
	defer func() {
		if err := environment.Close(); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	maxAttempts, err := resolveMaxAttempts(parsed.MaxAttempts, getenv)
	if err != nil {
		return err
	}
	providerName := strings.TrimSpace(lookupEnv(getenv, llmProviderEnvironment))
	if providerName == "" {
		providerName = defaultProvider
	}
	selected, err := selectProvider(config.Providers, providerName)
	if err != nil {
		return err
	}
	configuredBaseURL := strings.TrimSpace(lookupEnv(getenv, llmBaseURLEnvironment))
	if configuredBaseURL == "" {
		configuredBaseURL = selected.BaseURL
	}

	model := strings.TrimSpace(parsed.Model)
	if model == "" {
		model = strings.TrimSpace(lookupEnv(getenv, llmModelEnvironment))
	}
	if model == "" {
		model = selected.DefaultModel
	}
	if model == "" {
		return fmt.Errorf("model must be set in the request or %s", llmModelEnvironment)
	}
	var apiKey string
	if selected.APIKeyEnvironment != "" {
		apiKey = lookupEnv(getenv, llmAPIKeyEnvironment)
		if strings.TrimSpace(apiKey) == "" {
			apiKey = lookupEnv(getenv, selected.APIKeyEnvironment)
		}
		if strings.TrimSpace(apiKey) == "" {
			return fmt.Errorf(
				"%s or %s must be set",
				llmAPIKeyEnvironment,
				selected.APIKeyEnvironment,
			)
		}
	}
	client, err := selected.NewClient(apiKey, configuredBaseURL, maxAttempts, getenv)
	if err != nil {
		return fmt.Errorf("create %s client: %w", selected.Name, err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close %s client: %w", selected.Name, err))
		}
	}()

	mcConfig, mcJudge, err := resolveMetaCog(getenv, parsed.MetaCog, *metacogMode)
	if err != nil {
		return err
	}
	var render *renderer
	if config.Interactive {
		render = newRenderer(output, getenv, prompt == nil)
	}
	var llmAdapter llm.Adapter = client
	if mcConfig.Mode != metacog.ModeOff && mcConfig.Judge != nil {
		if !config.Interactive {
			if _, err := fmt.Fprintf(flagOutput, "metacog: judge=%s mode=%s\n", mcJudge, mcConfig.Mode); err != nil {
				return fmt.Errorf("write metacog notice: %w", err)
			}
		}
		var traceMu sync.Mutex
		mcConfig.Trace = func(event metacog.Event) {
			encoded, err := json.Marshal(event)
			if err != nil {
				return
			}
			traceMu.Lock()
			defer traceMu.Unlock()
			if render != nil {
				render.Event(event)
				return
			}
			fmt.Fprintf(flagOutput, "metacog %s\n", encoded)
		}
		llmAdapter = metacog.New(client, mcConfig)
	}

	storeDirectory, err := filepath.Abs(strings.TrimSpace(*sessionDirectory))
	if err != nil {
		return fmt.Errorf("resolve session directory: %w", err)
	}
	store, err := localfile.New(storeDirectory)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	sessionID, restored, err := openSession(ctx, store, parsed.SessionID)
	if err != nil {
		return err
	}
	logFile, err := openDatetimeLog(resolveLogDirectory(workspace, *logDirectory), time.Now())
	if err != nil {
		return err
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close session log: %w", err))
		}
	}()
	// Interactive mode keeps the JSONL session items in the log file only;
	// the terminal gets the rendered transcript from a second observer.
	observedOutput := io.Writer(logFile)
	if !config.Interactive {
		observedOutput = io.MultiWriter(logFile, output)
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	operationDirectory := filepath.Join(storeDirectory, "operations", string(sessionID))
	if err := os.MkdirAll(operationDirectory, 0o700); err != nil {
		return fmt.Errorf("create operation directory: %w", err)
	}
	shell := strings.TrimSpace(getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	skills, skillErrors := tool.DiscoverSkills(filepath.Join(workspace, ".harness", "skills"))
	names := []string{tool.BashName, tool.ViewImageName}
	if len(skills) != 0 {
		names = append(names, tool.SkillUseName)
	}
	toolConfig := ToolConfig{
		SessionID: sessionID, Getenv: getenv, Names: names,
		Translators: tool.StaticTranslators{
			Bash: bash.New(bash.Config{
				Shell:         shell,
				Directory:     workspace,
				BaseDirectory: operationDirectory,
			}),
			ViewImage: viewimage.New(viewimage.Config{Directory: workspace}),
		},
	}
	configuredTools, err := newTools(runContext, toolConfig)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		if configuredTools.Close != nil {
			runErr = errors.Join(runErr, configuredTools.Close())
		}
	}()
	registry := configuredTools.Registry
	if _, enabled := registry.Resolve(tool.SkillUseName); enabled {
		for _, skill := range skills {
			if _, err := registry.RegisterSkill(skill); err != nil {
				return fmt.Errorf("register skill %q: %w", skill.Path, err)
			}
		}
	}
	for _, skillErr := range skillErrors {
		if _, err := fmt.Fprintf(flagOutput, "skill error> %s\n", skillErr); err != nil {
			return fmt.Errorf("write skill error: %w", err)
		}
	}

	operations := operation.NewLocalOperationManager(runContext, configuredTools.RemoteJobs...)
	inputs, err := inbox.New(runContext, restored.ExternalInputIDs)
	if err != nil {
		return fmt.Errorf("open inbox: %w", err)
	}
	settingsPayload, err := json.Marshal(inbox.ControlMessage{
		Mode: inbox.UpdateSettings,
		Parameters: inbox.Settings{
			ReasoningEffort: reasoningEffort(parsed.ThinkingLevel),
		},
	})
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if err := inputs.Submit(runContext, inbox.Input{
		ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: settingsPayload,
	}); err != nil {
		return fmt.Errorf("submit settings: %w", err)
	}
	for index, message := range messages {
		payload, err := json.Marshal(message.Content)
		if err != nil {
			return fmt.Errorf("encode message %d: %w", index, err)
		}
		var messageID inbox.ID
		if message.MessageID != nil {
			messageID = inbox.ID(strings.TrimSpace(*message.MessageID))
		} else {
			messageID = inbox.ID(uuid.New().String())
		}
		if err := inputs.Submit(runContext, inbox.Input{
			ID: messageID, Kind: inbox.InputExternal, Payload: payload,
		}); err != nil {
			return fmt.Errorf("submit message %d: %w", index, err)
		}
	}

	// Interactive REPL mode stays alive for follow-up input; the input loop
	// submits StopWhenIdle on /quit or EOF. -p still runs to idle.
	if !config.Interactive || prompt != nil {
		stopPayload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.StopWhenIdle})
		if err != nil {
			return fmt.Errorf("encode stop request: %w", err)
		}
		if err := inputs.Submit(runContext, inbox.Input{
			ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: stopPayload,
		}); err != nil {
			return fmt.Errorf("submit stop request: %w", err)
		}
	}

	builder := contextbuilder.NewBuilder(registry.Skills()...)
	builder.SetModel(llm.Model{
		ID:              model,
		ReasoningEffort: reasoningEffort(parsed.ThinkingLevel),
	})
	systemPrompt := defaultSystemPrompt
	if parsed.SystemPrompt != nil {
		systemPrompt = *parsed.SystemPrompt
	}
	builder.SetSystemPrompt(systemPrompt)
	for _, definition := range registry.StaticDefinitions() {
		builder.AddTool(definition.Tool)
	}

	observer := &sessionObserver{
		sessionID: sessionID,
		output:    observedOutput,
		cancel:    cancel,
	}
	observerID := store.AddObserver(observer.Observe)
	defer store.RemoveObserver(observerID)
	var renderID sessionstore.ObserverID
	if render != nil {
		renderID = store.AddObserver(render.Observe)
		defer store.RemoveObserver(renderID)
	}
	if config.Interactive {
		mcLine := "metacog: off"
		if mcConfig.Mode != metacog.ModeOff && mcConfig.Judge != nil {
			mcLine = fmt.Sprintf("metacog: judge=%s mode=%s", mcJudge, mcConfig.Mode)
		}
		if _, err := fmt.Fprintf(output,
			"Heuristic — an agent that doesn't overthink\n%s/%s · workspace %s\n%s · session %s\ntype a request; /help for commands, /quit to exit\n",
			selected.Name, model, workspace, mcLine, sessionID); err != nil {
			return fmt.Errorf("write banner: %w", err)
		}
		if prompt == nil {
			if _, err := fmt.Fprint(output, "\n› "); err != nil {
				return fmt.Errorf("write prompt: %w", err)
			}
		}
	}
	if config.Interactive && prompt == nil {
		go runInputLoop(runContext, input, inputs, output, sessionID)
	}
	current := coordinator.New(coordinator.Dependencies{
		ToolHeartbeatInterval: *toolHeartbeatInterval,
		SessionID:             sessionID,
		Inbox:                 inputs,
		Restored:              restored,
		Sessions:              store,
		ContextBuilder:        builder,
		LLM:                   llmAdapter,
		Tools:                 registry,
		Operations:            operations,
	})
	coordinatorErr := current.Run(runContext)
	if observerErr := observer.Err(); observerErr != nil {
		return observerErr
	}
	if coordinatorErr != nil {
		if config.Interactive && errors.Is(coordinatorErr, context.Canceled) && ctx.Err() != nil {
			fmt.Fprintf(output, "\ninterrupted — resume with: %s -resume %s\n", config.Name, sessionID)
			return nil
		}
		return fmt.Errorf("run coordinator: %w", coordinatorErr)
	}
	return nil
}

// runInputLoop feeds interactive stdin lines into the inbox as external
// inputs and handles slash commands. It ends on /quit, /exit, or EOF, all of
// which submit StopWhenIdle so the coordinator drains and Run returns.
func runInputLoop(ctx context.Context, input io.Reader, inputs *inbox.Inbox, output io.Writer, sessionID session.ID) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	stop := func() {
		payload, err := json.Marshal(inbox.ControlMessage{Mode: inbox.StopWhenIdle})
		if err != nil {
			return
		}
		inputs.Submit(ctx, inbox.Input{
			ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload,
		})
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch {
		case line == "/quit" || line == "/exit":
			stop()
			return
		case line == "/session":
			fmt.Fprintf(output, "session %s\nresume with: heu -resume %s\n", sessionID, sessionID)
		case line == "/help":
			fmt.Fprint(output, "/session — session id and resume command\n/quit, /exit — finish and exit\nanything else — send to the agent\n")
		case strings.HasPrefix(line, "/"):
			fmt.Fprintf(output, "unknown command %s (see /help)\n", line)
		default:
			payload, err := json.Marshal(line)
			if err != nil {
				continue
			}
			inputs.Submit(ctx, inbox.Input{
				ID: inbox.ID(uuid.New().String()), Kind: inbox.InputExternal, Payload: payload,
			})
		}
	}
	stop() // EOF
}

func resolveMaxAttempts(requested *int, getenv func(string) string) (int, error) {
	maxAttempts := responsesapi.DefaultMaxAttempts
	if requested != nil {
		maxAttempts = *requested
	} else if value := strings.TrimSpace(lookupEnv(getenv, llmMaxAttemptsEnvironment)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("parse %s: %w", llmMaxAttemptsEnvironment, err)
		}
		maxAttempts = parsed
	}
	if maxAttempts <= 0 {
		return 0, errors.New("max attempts must be positive")
	}
	return maxAttempts, nil
}

func selectProvider(providers []Provider, name string) (Provider, error) {
	for _, provider := range providers {
		if provider.Name == name {
			if provider.NewClient == nil {
				return Provider{}, fmt.Errorf("provider %q has no client factory", name)
			}
			return provider, nil
		}
	}
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name)
	}
	return Provider{}, fmt.Errorf("unsupported provider %q; available providers: %s", name, strings.Join(names, ", "))
}

func DecodeRequest(input io.Reader, destination any) error {
	raw, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errors.New("empty input")
	}
	if err := json.Unmarshal(raw, destination, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func resolveLogDirectory(workspace, configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return filepath.Join(workspace, "logs")
	}
	return configured
}

func openDatetimeLog(directory string, now time.Time) (*os.File, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	path := filepath.Join(directory, now.UTC().Format("20060102-150405")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}
	return file, nil
}

func loadDotEnv(path string) (*environmentScope, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &environmentScope{}, nil
		}
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	values := make(map[string]string)
	for line := range strings.Lines(string(encoded)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, exists := strings.Cut(line, "=")
		if !exists {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		values[name] = strings.TrimSpace(value)
	}
	scope := &environmentScope{}
	for name, value := range values {
		_, present := os.LookupEnv(name)
		if present && name != "SANDBOX_EGRESS_PROXY" {
			continue
		}
		if err := scope.set(name, value); err != nil {
			return nil, errors.Join(err, scope.Close())
		}
	}
	if proxy := os.Getenv("SANDBOX_EGRESS_PROXY"); proxy != "" {
		if err := scope.set("HTTPS_PROXY", proxy); err != nil {
			return nil, errors.Join(err, scope.Close())
		}
	}
	return scope, nil
}

func (scope *environmentScope) set(name, value string) error {
	previous, present := os.LookupEnv(name)
	if err := os.Setenv(name, value); err != nil {
		return fmt.Errorf("set environment variable %q: %w", name, err)
	}
	scope.changes = append(scope.changes, environmentChange{
		name: name, value: previous, present: present,
	})
	return nil
}

func (scope *environmentScope) Close() error {
	var closeErr error
	for _, change := range slices.Backward(scope.changes) {
		var err error
		if change.present {
			err = os.Setenv(change.name, change.value)
		} else {
			err = os.Unsetenv(change.name)
		}
		if err != nil {
			closeErr = errors.Join(
				closeErr,
				fmt.Errorf("restore environment variable %q: %w", change.name, err),
			)
		}
	}
	scope.changes = nil
	return closeErr
}

func validateRequest(parsed Request) ([]RequestMessage, error) {
	if parsed.SessionID != nil && strings.TrimSpace(*parsed.SessionID) == "" {
		return nil, errors.New("session_id must not be empty")
	}
	if parsed.ThinkingLevel != "" {
		switch parsed.ThinkingLevel {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return nil, errors.New("thinking_level must be one of: low, medium, high, xhigh, max")
		}
	}
	for _, name := range append(parsed.ExtraAllowedTools, parsed.DisallowedTools...) {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("tool names must not be empty")
		}
	}

	if parsed.Messages == nil {
		if parsed.Prompt == nil {
			return nil, errors.New("messages must be set")
		}
		return []RequestMessage{{Content: *parsed.Prompt}}, nil
	}
	if len(parsed.Messages) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	for index, message := range parsed.Messages {
		if message.Role != "" && message.Role != "user" {
			return nil, fmt.Errorf("messages[%d].role must be user", index)
		}
		if message.MessageID != nil && strings.TrimSpace(*message.MessageID) == "" {
			return nil, fmt.Errorf("messages[%d].message_id must not be empty", index)
		}
		if message.MessageID != nil {
			if _, err := uuid.Parse(strings.TrimSpace(*message.MessageID)); err != nil {
				return nil, fmt.Errorf("messages[%d].message_id must be a UUID", index)
			}
		}
	}
	return parsed.Messages, nil
}

func reasoningEffort(level string) llm.ReasoningEffort {
	switch level {
	case "low":
		return llm.ReasoningEffortLow
	case "medium":
		return llm.ReasoningEffortMedium
	case "xhigh":
		return llm.ReasoningEffortXHigh
	case "max":
		return llm.ReasoningEffortMax
	default:
		return llm.ReasoningEffortHigh
	}
}

func openSession(
	ctx context.Context,
	store *localfile.Store,
	requested *string,
) (session.ID, sessionstore.ResumeState, error) {
	id := session.ID(uuid.New().String())
	if requested != nil {
		id = session.ID(strings.TrimSpace(*requested))
	}
	restored, err := store.Resume(ctx, id)
	if err == nil {
		return id, restored, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", sessionstore.ResumeState{}, fmt.Errorf("open session %q: %w", id, err)
	}
	snapshot, err := store.Create(ctx, id)
	if err != nil {
		return "", sessionstore.ResumeState{}, fmt.Errorf("create session %q: %w", id, err)
	}
	return id, sessionstore.ResumeState{Snapshot: snapshot}, nil
}

func (observer *sessionObserver) Observe(sessionID session.ID, item sessionstore.Item) {
	if sessionID != observer.sessionID {
		return
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.err != nil {
		return
	}
	if err := writeSessionItem(observer.output, item); err != nil {
		observer.fail(err)
		return
	}
}

func writeSessionItem(output io.Writer, item sessionstore.Item) error {
	encoded, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode session item %d: %w", item.Sequence, err)
	}
	if _, err := fmt.Fprintf(output, "%s\n", encoded); err != nil {
		return fmt.Errorf("write session item %d: %w", item.Sequence, err)
	}
	return nil
}

func (observer *sessionObserver) fail(err error) {
	observer.err = err
	observer.cancel()
}

func (observer *sessionObserver) Err() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.err
}
