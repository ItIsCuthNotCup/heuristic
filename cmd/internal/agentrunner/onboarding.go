package agentrunner

// First-run onboarding (`heu setup`, `/login`): plain-language provider
// choices, sign-in or key entry, a model list fetched from the provider, a
// live connection check, and an optional MetaCog judge. Esc goes back one
// step; nothing is saved until the whole flow succeeds.

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
)

const commandCodeBaseURL = "https://api.commandcode.ai/provider/v1"

type providerSpec struct {
	id          string
	label       string
	detail      string
	provider    string // runner provider name
	baseURL     string // "" keeps the provider default
	keyName     string // shown as "Paste your <keyName>"
	keyURL      string
	keyEnv      []string
	models      []modelChoice // static list; nil means ask the provider
	recommended []string
}

type modelChoice struct {
	id     string
	name   string
	detail string
}

// codexModels mirrors the listed models in codex-rs models-manager/models.json.
var codexModels = []modelChoice{
	{id: "gpt-6-sol", name: "GPT-6 Sol", detail: "workhorse for coding and everyday work"},
	{id: "gpt-6-astra", name: "GPT-6 Astra", detail: "frontier intelligence, slower"},
	{id: "gpt-6-luna", name: "GPT-6 Luna", detail: "fast and affordable"},
	{id: "gpt-5.6-sol", name: "GPT-5.6 Sol", detail: "older coding model"},
	{id: "gpt-5.5", name: "GPT-5.5", detail: "legacy"},
}

func providerCatalog() []providerSpec {
	return []providerSpec{
		{
			id: "chatgpt", label: "Sign in with ChatGPT", detail: "use your Plus, Pro or Business plan · no API key",
			provider: "openai-codex", models: codexModels,
		},
		{
			id: "commandcode", label: "Command Code", detail: "one key for Kimi, GLM, DeepSeek, Qwen, GPT and more",
			provider: "openai", baseURL: commandCodeBaseURL, keyName: "Command Code API key",
			keyURL: "https://commandcode.ai", keyEnv: []string{"COMMANDCODE_API_KEY"},
			recommended: []string{"moonshotai/Kimi-K2.5", "moonshotai/Kimi-K2.7-Code", "zai-org/GLM-5.3", "deepseek/deepseek-v4-pro"},
		},
		{
			id: "openai", label: "OpenAI API key", detail: "pay per use from platform.openai.com",
			provider: "openai", keyName: "OpenAI API key", keyURL: "https://platform.openai.com/api-keys",
			keyEnv: []string{"OPENAI_API_KEY"}, recommended: []string{"gpt-6-sol", "gpt-6-astra", "gpt-6-luna"},
		},
		{
			id: "openrouter", label: "OpenRouter", detail: "one key for hundreds of models",
			provider: "openrouter", keyName: "OpenRouter API key", keyURL: "https://openrouter.ai/keys",
			keyEnv: []string{"OPENROUTER_API_KEY"},
		},
		{
			id: "fireworks", label: "Fireworks", detail: "fast hosted open models",
			provider: "fireworks", keyName: "Fireworks API key", keyURL: "https://fireworks.ai/account/api-keys",
			keyEnv: []string{"FIREWORKS_API_KEY"},
		},
		{
			id: "ollama", label: "Ollama on this computer", detail: "local and free · no key",
			provider: "ollama",
		},
		{
			id: "custom", label: "Another server (advanced)", detail: "any OpenAI Responses-compatible URL",
			provider: "openai", keyName: "API key",
		},
	}
}

// connection is one provider/model choice as it will be saved.
type connection struct {
	spec     providerSpec
	baseURL  string
	apiKey   string
	model    string
	authFile string
}

func (c connection) effectiveBaseURL(providers []Provider) string {
	if c.baseURL != "" {
		return c.baseURL
	}
	for _, provider := range providers {
		if provider.Name == c.spec.provider {
			return provider.BaseURL
		}
	}
	return ""
}

type setupDeps struct {
	providers    []Provider
	listModels   func(ctx context.Context, c connection) ([]modelChoice, error)
	check        func(ctx context.Context, c connection) error
	browserLogin func(ctx context.Context, a asker, getenv func(string) string) (string, error)
	deviceLogin  func(ctx context.Context, a asker, getenv func(string) string) (string, error)
}

func defaultSetupDeps(providers []Provider, getenv func(string) string) setupDeps {
	return setupDeps{
		providers: providers,
		listModels: func(ctx context.Context, c connection) ([]modelChoice, error) {
			return fetchModels(ctx, c.effectiveBaseURL(providers), c.apiKey)
		},
		check: func(ctx context.Context, c connection) error {
			return checkConnection(ctx, providers, c, getenv)
		},
		browserLogin: codexLogin,
		deviceLogin:  codexDeviceLogin,
	}
}

// runSetupWizard walks the user through onboarding and writes config.env.
// It returns the saved values so a running session can apply them.
func runSetupWizard(ctx context.Context, a asker, getenv func(string) string, deps setupDeps) (map[string]string, error) {
	p := a.colors()
	catalog := providerCatalog()
	var conn connection
	values := map[string]string{}
	step := 0
	for step < 5 {
		var err error
		switch step {
		case 0:
			conn, err = chooseProvider(a, catalog, getenv)
		case 1:
			conn, err = authenticate(ctx, a, conn, getenv, deps)
		case 2:
			conn.model, err = chooseModel(ctx, a, conn, deps)
		case 3:
			err = verify(ctx, a, &conn, deps)
		case 4:
			err = chooseJudge(ctx, a, getenv, values)
		}
		switch {
		case errors.Is(err, errBack):
			if step > 0 {
				step--
			}
			if step == 3 {
				step = 2
			}
			continue
		case errors.Is(err, errRetryKey):
			step = 1
			continue
		case err != nil:
			return nil, err
		}
		step++
	}
	for name, value := range connectionValues(conn) {
		values[name] = value
	}
	if err := writeConfigFile(getenv, values); err != nil {
		return nil, err
	}
	summary := modelDisplayName(conn.model) + " via " + conn.spec.label
	if conn.spec.id == "chatgpt" {
		summary = modelDisplayName(conn.model) + " via your ChatGPT plan"
	}
	judge := "MetaCog off"
	switch values["HEURISTIC_JUDGE"] {
	case "":
		judge = "MetaCog on (Jev)"
	case "local":
		judge = "MetaCog on (local judge)"
	}
	a.note("\n" + p.green("✓") + " " + p.bold("You're set up.") + " " + summary + " · " + judge + "\n" +
		p.dim("  Saved to "+shortPath(userConfigPath(getenv))+". Change it any time with /login or /model.") + "\n")
	return values, nil
}

var errRetryKey = errors.New("retry key")

func chooseProvider(a asker, catalog []providerSpec, getenv func(string) string) (connection, error) {
	options := make([]option, len(catalog))
	def := -1
	for i, spec := range catalog {
		detail := spec.detail
		if spec.id == "chatgpt" && existingCodexLogin(getenv) != "" {
			detail = "found your existing ChatGPT sign-in on this machine"
		}
		if envKey(spec, getenv) != "" {
			detail = "key found in your environment · " + detail
			if def < 0 {
				def = i
			}
		}
		options[i] = option{label: spec.label, detail: detail}
	}
	if configured := strings.TrimSpace(lookupEnv(getenv, llmProviderEnvironment)); configured != "" {
		baseURL := strings.TrimRight(strings.TrimSpace(lookupEnv(getenv, llmBaseURLEnvironment)), "/")
		spec, ok := matchProvider(catalog, configured, baseURL)
		for i := range catalog {
			if ok && catalog[i].id == spec.id || !ok && catalog[i].id == "custom" {
				def = i
			}
		}
	}
	index, err := a.choose("How should Heuristic reach a model?",
		"Pick one — you can switch any time with /login.", options, max(def, 0))
	if err != nil {
		if errors.Is(err, errBack) {
			return connection{}, errCancelled
		}
		return connection{}, err
	}
	return connection{spec: catalog[index], baseURL: catalog[index].baseURL}, nil
}

// matchProvider finds the catalog entry for a configured provider and base URL.
func matchProvider(catalog []providerSpec, provider, baseURL string) (providerSpec, bool) {
	for _, spec := range catalog {
		if spec.provider != provider || spec.id == "custom" {
			continue
		}
		if spec.baseURL == baseURL || (spec.baseURL == "" && (baseURL == "" || spec.id == "openai" && strings.Contains(baseURL, "api.openai.com"))) {
			return spec, true
		}
	}
	return providerSpec{}, false
}

func envKey(spec providerSpec, getenv func(string) string) string {
	for _, name := range spec.keyEnv {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func authenticate(ctx context.Context, a asker, conn connection, getenv func(string) string, deps setupDeps) (connection, error) {
	p := a.colors()
	switch conn.spec.id {
	case "chatgpt":
		existing := existingCodexLogin(getenv)
		options := []option{}
		if existing != "" {
			options = append(options, option{label: "Use my existing sign-in", detail: "from the Codex CLI on this machine"})
		}
		options = append(options,
			option{label: "Open a browser on this computer", detail: "best when you're sitting at this machine"},
			option{label: "Sign in from another device", detail: "get a code, enter it on your phone or laptop · best over SSH"},
		)
		def := 0
		if existing == "" && remoteSession(getenv) {
			def = 1
		}
		for {
			index, err := a.choose("Sign in with ChatGPT", "", options, def)
			if err != nil {
				return conn, err
			}
			if existing == "" {
				index++
			}
			switch index {
			case 0:
				conn.authFile = existing
			case 1:
				conn.authFile, err = deps.browserLogin(ctx, a, getenv)
			case 2:
				conn.authFile, err = deps.deviceLogin(ctx, a, getenv)
			}
			if err == nil {
				return conn, nil
			}
			if errors.Is(err, errCancelled) {
				return conn, err
			}
			a.note(p.red("✗ ") + friendlyError(err))
		}
	case "ollama":
		for {
			up := false
			_ = a.busy("Looking for Ollama on this computer…", func() error {
				_, err := deps.listModels(ctx, conn)
				up = err == nil
				return nil
			})
			if up {
				return conn, nil
			}
			index, err := a.choose("Ollama isn't running",
				"Install it from https://ollama.com, then run `ollama serve` and `ollama pull qwen3:8b`.",
				[]option{{label: "Try again"}, {label: "Pick a different option"}}, 0)
			if err != nil || index == 1 {
				return conn, errBack
			}
		}
	case "custom":
		url, err := a.input(ctx, "Server address", "The base URL of an OpenAI Responses-compatible server, e.g. http://localhost:8000/v1",
			conn.baseURL, false)
		if err != nil {
			return conn, err
		}
		if url == "" {
			return conn, errBack
		}
		conn.baseURL = strings.TrimRight(url, "/")
		key, err := a.input(ctx, "API key (optional)", "Leave empty if the server doesn't need one.", "", true)
		if err != nil {
			return conn, err
		}
		conn.apiKey = key
		return conn, nil
	}
	if found := envKey(conn.spec, getenv); found != "" {
		index, err := a.choose("Use the "+conn.spec.keyName+" from your environment?",
			"Found $"+conn.spec.keyEnv[0]+" ("+maskKey(found)+").",
			[]option{{label: "Yes, use it"}, {label: "No, paste a different key"}}, 0)
		if err != nil {
			return conn, err
		}
		if index == 0 {
			conn.apiKey = found
			return conn, nil
		}
	}
	for {
		key, err := a.input(ctx, "Paste your "+conn.spec.keyName, "Get one at "+conn.spec.keyURL, "", true)
		if err != nil {
			return conn, err
		}
		if key != "" {
			conn.apiKey = key
			return conn, nil
		}
		a.note(p.yellow("!") + " A key is needed for " + conn.spec.label + ". Press Esc to pick another option.")
	}
}

func remoteSession(getenv func(string) string) bool {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return true
	}
	return runtime.GOOS == "linux" && getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == ""
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "••••"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func chooseModel(ctx context.Context, a asker, conn connection, deps setupDeps) (string, error) {
	models := conn.spec.models
	var listErr error
	if models == nil {
		_ = a.busy("Loading models from "+conn.spec.label+"…", func() error {
			models, listErr = deps.listModels(ctx, conn)
			return nil
		})
	}
	models = rankModels(models, conn.spec.recommended)
	const typeOwn = "Type a model name…"
	options := make([]option, 0, len(models)+1)
	for i, model := range models {
		detail := model.detail
		if i == 0 && len(conn.spec.recommended) > 0 && model.id == conn.spec.recommended[0] {
			detail = strings.TrimPrefix(strings.Join([]string{"recommended", detail}, " · "), " · ")
			detail = strings.TrimSuffix(detail, " · ")
		}
		label := model.name
		if label == "" {
			label = model.id
		}
		if label != model.id && detail == "" {
			detail = model.id
		}
		options = append(options, option{label: label, detail: detail})
	}
	options = append(options, option{label: typeOwn, detail: "if your model isn't listed"})
	hint := ""
	if listErr != nil {
		hint = "Couldn't load the model list (" + friendlyError(listErr) + ")."
	}
	index, err := a.choose("Which model?", hint, options, 0)
	if err != nil {
		return "", err
	}
	if index < len(models) {
		return models[index].id, nil
	}
	name, err := a.input(ctx, "Model name", "Exactly as the provider spells it, e.g. moonshotai/Kimi-K2.5", "", false)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", errBack
	}
	return name, nil
}

// rankModels puts recommended models first, in the recommended order.
func rankModels(models []modelChoice, recommended []string) []modelChoice {
	out := slices.Clone(models)
	rank := func(id string) int {
		if i := slices.Index(recommended, id); i >= 0 {
			return i
		}
		return len(recommended)
	}
	slices.SortStableFunc(out, func(x, y modelChoice) int { return rank(x.id) - rank(y.id) })
	return out
}

func verify(ctx context.Context, a asker, conn *connection, deps setupDeps) error {
	p := a.colors()
	var err error
	label := "Checking " + modelDisplayName(conn.model) + "…"
	busyErr := a.busy(label, func() error {
		err = deps.check(ctx, *conn)
		return nil
	})
	if busyErr != nil {
		return busyErr
	}
	if err == nil {
		a.note(p.green("✓") + " Connected to " + modelDisplayName(conn.model))
		return nil
	}
	a.note(p.red("✗ ") + friendlyError(err))
	options := []option{{label: "Pick another model"}}
	if conn.spec.keyName != "" {
		options = append(options, option{label: "Try a different key"})
	}
	options = append(options, option{label: "Save anyway", detail: "if you know it works"})
	index, err := a.choose("That didn't work", "", options, 0)
	if err != nil {
		return err
	}
	switch options[index].label {
	case "Pick another model":
		return errBack
	case "Try a different key":
		return errRetryKey
	}
	return nil
}

func chooseJudge(ctx context.Context, a asker, getenv func(string) string, values map[string]string) error {
	found := jevAPIKey(getenv)
	jev := option{label: "Yes, with Jev", detail: "most accurate judge · free key at typesafe.ai"}
	if found != "" {
		jev.detail = "key found in your environment"
	}
	index, err := a.choose("Turn on MetaCog?",
		"When the model isn't sure of an answer, MetaCog tries a few more thought paths and a judge picks the best.\n"+
			"It only spends extra calls on hard answers; routine steps run as normal.",
		[]option{
			jev,
			{label: "Yes, with a local judge", detail: "an open model on this machine that returns logprobs · advanced"},
			{label: "Not now", detail: "plain model; turn it on later with /metacog"},
		}, 0)
	if err != nil {
		return err
	}
	switch index {
	case 0:
		values["HEURISTIC_JUDGE"] = ""
		if found != "" {
			return nil
		}
		key, err := a.input(ctx, "Paste your TypeSafe API key", "Get one at https://typesafe.ai", "", true)
		if err != nil {
			return err
		}
		if key == "" {
			return errBack
		}
		values["HEURISTIC_JEV_API_KEY"] = key
	case 1:
		url, err := a.input(ctx, "Judge server address", "OpenAI-compatible server with logprobs (llama-server, vLLM).",
			"http://localhost:8080/v1", false)
		if err != nil {
			return err
		}
		model, err := a.input(ctx, "Judge model name", "", "", false)
		if err != nil {
			return err
		}
		if model == "" {
			return errBack
		}
		values["HEURISTIC_JUDGE"] = "local"
		values["HEURISTIC_JUDGE_URL"] = url
		values["HEURISTIC_JUDGE_MODEL"] = model
	case 2:
		values["HEURISTIC_JUDGE"] = "off"
	}
	return nil
}

// connectionValues are the config.env entries for conn; empty values clear
// stale entries from an earlier setup.
func connectionValues(conn connection) map[string]string {
	values := map[string]string{
		"HEURISTIC_LLM_PROVIDER": conn.spec.provider,
		"HEURISTIC_LLM_MODEL":    conn.model,
		"HEURISTIC_LLM_BASE_URL": conn.baseURL,
		"HEURISTIC_LLM_API_KEY":  conn.apiKey,
		"OPENAI_CODEX_AUTH_FILE": conn.authFile,
	}
	return values
}

// overlayEnv makes values visible through getenv without touching the
// process environment.
func overlayEnv(getenv func(string) string, values map[string]string) func(string) string {
	return func(name string) string {
		if value, ok := values[name]; ok {
			return value
		}
		return getenv(name)
	}
}

func fetchModels(ctx context.Context, baseURL, apiKey string) ([]modelChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(body)), 200))
	}
	var listing struct {
		Data []struct {
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			ContextLength      int      `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("unexpected model list: %w", err)
	}
	models := make([]modelChoice, 0, len(listing.Data))
	for _, entry := range listing.Data {
		if entry.ID == "" {
			continue
		}
		// Heuristic speaks the Responses API; skip models a gateway says
		// it only serves elsewhere (e.g. Anthropic /messages).
		if len(entry.SupportedEndpoints) > 0 && !slices.Contains(entry.SupportedEndpoints, "/responses") {
			continue
		}
		detail := ""
		if entry.ContextLength > 0 {
			detail = fmt.Sprintf("%dk context", entry.ContextLength/1000)
		}
		models = append(models, modelChoice{id: entry.ID, name: entry.Name, detail: detail})
	}
	if len(models) == 0 {
		return nil, errors.New("the provider returned no usable models")
	}
	return models, nil
}

// checkConnection sends one tiny request through the real client so a bad
// key, model or sign-in fails here rather than on the first prompt.
func checkConnection(ctx context.Context, providers []Provider, conn connection, getenv func(string) string) error {
	selected, err := selectProvider(providers, conn.spec.provider)
	if err != nil {
		return err
	}
	env := overlayEnv(getenv, map[string]string{"OPENAI_CODEX_AUTH_FILE": conn.authFile})
	if conn.authFile == "" {
		env = getenv
	}
	client, err := selected.NewClient(conn.apiKey, conn.effectiveBaseURL(providers), 1, env)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := client.Respond(ctx, llm.Request{
		Model: llm.Model{ID: conn.model},
		Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Reply with the single word: ready"}}},
	}, llm.RequestOptions{})
	if err != nil {
		return err
	}
	if resp.Failure != nil {
		return errors.New(resp.Failure.Message)
	}
	return nil
}

// isAuthError reports whether the provider rejected the credentials.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"401", "unauthorized", "invalid api key", "invalid_api_key", "incorrect api key",
		"authentication", "invalid 'authorization'",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// friendlyError turns provider/transport errors into one plain sentence.
func friendlyError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	lower := strings.ToLower(message)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(lower, "timeout"):
		return "The provider didn't answer in time. Check your connection and try again."
	case isAuthError(err):
		return "The provider rejected that key. Double-check it and try again."
	case strings.Contains(lower, "unsupported_model") || strings.Contains(lower, "model_not_found") ||
		(strings.Contains(lower, "model") && strings.Contains(lower, "not") &&
			(strings.Contains(lower, "support") || strings.Contains(lower, "found") || strings.Contains(lower, "exist"))):
		return "That model isn't available here. Pick another one."
	case strings.Contains(lower, "429") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota"):
		return "The provider says you're over your limit or out of credit."
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network is unreachable"):
		return "Couldn't reach the server. Is the address right and the server running?"
	}
	return truncateRunes(message, 300)
}

// modelDisplayName is the short model name shown in the UI.
func modelDisplayName(model string) string {
	for _, known := range codexModels {
		if known.id == model {
			return known.name
		}
	}
	if index := strings.LastIndex(model, "/"); index >= 0 {
		return model[index+1:]
	}
	return model
}

// providerDisplayName is the plain name for the configured provider.
func providerDisplayName(provider, baseURL string) string {
	if strings.TrimRight(baseURL, "/") == commandCodeBaseURL {
		return "Command Code"
	}
	switch provider {
	case "openai-codex":
		return "ChatGPT"
	case "openai":
		if baseURL == "" || strings.Contains(baseURL, "api.openai.com") {
			return "OpenAI"
		}
		return strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	case "openrouter":
		return "OpenRouter"
	case "fireworks":
		return "Fireworks"
	case "ollama":
		return "Ollama"
	}
	return provider
}
