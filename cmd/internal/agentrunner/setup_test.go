package agentrunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wizardGetenv fakes a HOME so config writes land in t.TempDir(); other
// names fall through to the process environment.
func wizardGetenv(home string) func(string) string {
	return func(name string) string {
		if name == "HOME" || name == "XDG_CONFIG_HOME" {
			if name == "HOME" {
				return home
			}
			return ""
		}
		return os.Getenv(name)
	}
}

// isolatedGetenv sees only HOME, so no real keys leak into a test.
func isolatedGetenv(home string) func(string) string {
	return func(name string) string {
		if name == "HOME" {
			return home
		}
		return ""
	}
}

func readConfigEnv(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".config", "heuristic", "config.env"))
	if err != nil {
		t.Fatalf("read config.env: %v", err)
	}
	return string(data)
}

type fakeSetup struct {
	models     []modelChoice
	listErr    error
	checkErrs  []error
	checks     []connection
	loginPath  string
	loginErrs  []error
	loginCalls int
}

func (f *fakeSetup) deps() setupDeps {
	return setupDeps{
		listModels: func(context.Context, connection) ([]modelChoice, error) { return f.models, f.listErr },
		check: func(_ context.Context, c connection) error {
			f.checks = append(f.checks, c)
			if len(f.checkErrs) > 0 {
				err := f.checkErrs[0]
				f.checkErrs = f.checkErrs[1:]
				return err
			}
			return nil
		},
		browserLogin: f.login,
		deviceLogin:  f.login,
	}
}

func (f *fakeSetup) login(context.Context, asker, func(string) string) (string, error) {
	f.loginCalls++
	if len(f.loginErrs) > 0 {
		err := f.loginErrs[0]
		f.loginErrs = f.loginErrs[1:]
		return "", err
	}
	return f.loginPath, nil
}

func runWizard(t *testing.T, home, script string, fake *fakeSetup) (map[string]string, string, error) {
	t.Helper()
	var out bytes.Buffer
	a := &lineAsker{reader: bufio.NewReader(strings.NewReader(script)), out: &out}
	values, err := runSetupWizard(context.Background(), a, isolatedGetenv(home), fake.deps())
	return values, out.String(), err
}

func assertConfig(t *testing.T, home string, want ...string) string {
	t.Helper()
	config := readConfigEnv(t, home)
	for _, line := range want {
		if !strings.Contains(config, line+"\n") {
			t.Fatalf("config.env missing %q:\n%s", line, config)
		}
	}
	return config
}

func TestSetupWizardOpenAIKey(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{models: []modelChoice{{id: "gpt-6-luna"}, {id: "gpt-6-sol"}}}
	// OpenAI API key, paste key, recommended model, MetaCog not now.
	_, out, err := runWizard(t, home, "3\nsk-test-secret\n\n3\n", fake)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	assertConfig(t, home,
		"HEURISTIC_LLM_PROVIDER=openai",
		"HEURISTIC_LLM_MODEL=gpt-6-sol",
		"HEURISTIC_LLM_API_KEY=sk-test-secret",
		"HEURISTIC_JUDGE=off",
	)
	info, err := os.Stat(filepath.Join(home, ".config", "heuristic", "config.env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.env perm: %v %v", info, err)
	}
	if strings.Contains(out, "sk-test-secret") {
		t.Fatal("wizard echoed the API key")
	}
	for _, jargon := range []string{"Base URL", "base URL", "OpenAI-compatible"} {
		if strings.Contains(out, jargon) {
			t.Fatalf("normal path shows jargon %q:\n%s", jargon, out)
		}
	}
	if !strings.Contains(out, "Connected to") || !strings.Contains(out, "You're set up.") {
		t.Fatalf("missing success copy:\n%s", out)
	}
	if len(fake.checks) != 1 || fake.checks[0].apiKey != "sk-test-secret" || fake.checks[0].model != "gpt-6-sol" {
		t.Fatalf("checks = %+v", fake.checks)
	}
}

func TestSetupWizardRetriesRejectedKey(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{
		models:    []modelChoice{{id: "gpt-6-sol"}},
		checkErrs: []error{errors.New("status 401: invalid api key")},
	}
	// bad key fails the check → "Try a different key" → good key.
	_, out, err := runWizard(t, home, "3\nbad-key\n\n2\ngood-key\n\n3\n", fake)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rejected that key") {
		t.Fatalf("no friendly error:\n%s", out)
	}
	config := assertConfig(t, home, "HEURISTIC_LLM_API_KEY=good-key")
	if strings.Contains(config, "bad-key") {
		t.Fatalf("saved the rejected key:\n%s", config)
	}
}

func TestSetupWizardUnavailableModelPicksAnother(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{
		models:    []modelChoice{{id: "a"}, {id: "b"}},
		checkErrs: []error{errors.New(`unsupported_model: Model "a" is not supported on this endpoint.`)},
	}
	_, out, err := runWizard(t, home, "3\nkey\n1\n1\n2\n3\n", fake)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "isn't available here") {
		t.Fatalf("no friendly error:\n%s", out)
	}
	assertConfig(t, home, "HEURISTIC_LLM_MODEL=b")
}

func TestSetupWizardSaveAnyway(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{models: []modelChoice{{id: "m"}}, checkErrs: []error{errors.New("status 500")}}
	if _, out, err := runWizard(t, home, "3\nkey\n\n3\n3\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	assertConfig(t, home, "HEURISTIC_LLM_MODEL=m")
}

func TestSetupWizardOllama(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{models: []modelChoice{{id: "qwen3:8b"}}}
	if _, out, err := runWizard(t, home, "6\n\n3\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	config := assertConfig(t, home, "HEURISTIC_LLM_PROVIDER=ollama", "HEURISTIC_LLM_MODEL=qwen3:8b")
	if strings.Contains(config, "API_KEY") || strings.Contains(config, "BASE_URL") {
		t.Fatalf("ollama path wrote a key or URL:\n%s", config)
	}
}

func TestSetupWizardOllamaNotRunning(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{listErr: errors.New("connection refused")}
	// Ollama down → "Pick a different option" → Command Code.
	fake2 := []modelChoice{{id: "moonshotai/Kimi-K2.5"}}
	a := &lineAsker{reader: bufio.NewReader(strings.NewReader("6\n2\n2\ncc-key\n\n3\n")), out: &bytes.Buffer{}}
	deps := fake.deps()
	calls := 0
	deps.listModels = func(context.Context, connection) ([]modelChoice, error) {
		calls++
		if calls == 1 {
			return nil, fake.listErr
		}
		return fake2, nil
	}
	if _, err := runSetupWizard(context.Background(), a, isolatedGetenv(home), deps); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	assertConfig(t, home, "HEURISTIC_LLM_BASE_URL="+commandCodeBaseURL)
}

func TestSetupWizardCommandCodeWithLocalJudge(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{models: []modelChoice{{id: "zai-org/GLM-5.3"}, {id: "moonshotai/Kimi-K2.5"}, {id: "other/x"}}}
	if _, out, err := runWizard(t, home, "2\ncc-bogus\n\n2\n\nqwen3:4b\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	assertConfig(t, home,
		"HEURISTIC_LLM_PROVIDER=openai",
		"HEURISTIC_LLM_BASE_URL=https://api.commandcode.ai/provider/v1",
		"HEURISTIC_LLM_MODEL=moonshotai/Kimi-K2.5",
		"HEURISTIC_LLM_API_KEY=cc-bogus",
		"HEURISTIC_JUDGE=local",
		"HEURISTIC_JUDGE_URL=http://localhost:8080/v1",
		"HEURISTIC_JUDGE_MODEL=qwen3:4b",
	)
}

func TestSetupWizardUsesEnvironmentKey(t *testing.T) {
	home := t.TempDir()
	getenv := func(name string) string {
		switch name {
		case "HOME":
			return home
		case "OPENROUTER_API_KEY":
			return "sk-or-from-env-123456"
		}
		return ""
	}
	var out bytes.Buffer
	a := &lineAsker{reader: bufio.NewReader(strings.NewReader("4\n1\n\n3\n")), out: &out}
	fake := &fakeSetup{models: []modelChoice{{id: "some/model"}}}
	if _, err := runSetupWizard(context.Background(), a, getenv, fake.deps()); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	if strings.Contains(out.String(), "sk-or-from-env-123456") {
		t.Fatal("printed the environment key")
	}
	if fake.checks[0].apiKey != "sk-or-from-env-123456" {
		t.Fatalf("check used %q", fake.checks[0].apiKey)
	}
}

func TestSetupWizardCustomServerTypedModel(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{models: []modelChoice{{id: "listed"}}}
	if _, out, err := runWizard(t, home, "7\nhttp://localhost:8000/v1/\n\n2\nmy-model\n3\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	config := assertConfig(t, home, "HEURISTIC_LLM_BASE_URL=http://localhost:8000/v1", "HEURISTIC_LLM_MODEL=my-model")
	if strings.Contains(config, "API_KEY") {
		t.Fatalf("empty key saved:\n%s", config)
	}
}

func TestSetupWizardChatGPTLogin(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{loginPath: filepath.Join(home, "auth.json")}
	if _, out, err := runWizard(t, home, "1\n1\n\n3\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	assertConfig(t, home,
		"HEURISTIC_LLM_PROVIDER=openai-codex",
		"HEURISTIC_LLM_MODEL=gpt-6-sol",
		"OPENAI_CODEX_AUTH_FILE="+fake.loginPath,
	)
}

func TestSetupWizardChatGPTLoginFailureGoesBack(t *testing.T) {
	home := t.TempDir()
	fake := &fakeSetup{
		loginPath: filepath.Join(home, "auth.json"),
		loginErrs: []error{errors.New("sign-in was cancelled")},
	}
	// Failed browser sign-in → back to the sign-in choice → device code.
	if _, out, err := runWizard(t, home, "1\n1\n2\n\n3\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if fake.loginCalls != 2 {
		t.Fatalf("login calls = %d", fake.loginCalls)
	}
}

func TestSetupWizardReusesCodexLogin(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(codexDir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"id_token":"h.e30.s","access_token":"a","refresh_token":"r","account_id":"acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSetup{}
	if _, out, err := runWizard(t, home, "1\n1\n\n3\n", fake); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out)
	}
	if fake.loginCalls != 0 {
		t.Fatal("started a new sign-in despite an existing one")
	}
	assertConfig(t, home, "OPENAI_CODEX_AUTH_FILE="+auth)
}

func TestSetupWizardCancelWritesNothing(t *testing.T) {
	home := t.TempDir()
	for _, script := range []string{"-\n", "3\nkey\n", ""} {
		_, _, err := runWizard(t, home, script, &fakeSetup{models: []modelChoice{{id: "m"}}})
		if err == nil {
			t.Fatalf("script %q: expected an error", script)
		}
		if _, statErr := os.Stat(filepath.Join(home, ".config", "heuristic", "config.env")); statErr == nil {
			t.Fatalf("script %q wrote config", script)
		}
	}
}

func TestFriendlyErrors(t *testing.T) {
	for raw, want := range map[string]string{
		"status 401: Unauthorized":                            "rejected that key",
		"unsupported_model: Model \"gpt-5\" is not supported": "isn't available",
		"dial tcp: connection refused":                        "Couldn't reach",
		"status 429: rate limit":                              "over your limit",
	} {
		if got := friendlyError(errors.New(raw)); !strings.Contains(got, want) {
			t.Errorf("friendlyError(%q) = %q", raw, got)
		}
	}
}

func TestRankModels(t *testing.T) {
	got := rankModels([]modelChoice{{id: "c"}, {id: "b"}, {id: "a"}}, []string{"a", "b"})
	if got[0].id != "a" || got[1].id != "b" || got[2].id != "c" {
		t.Fatalf("rank = %+v", got)
	}
}

func TestFetchModelsFiltersNonResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"a","supported_endpoints":["/responses"],"context_length":128000},`+
			`{"id":"claude","supported_endpoints":["/messages"]},{"id":"plain"}]}`)
	}))
	defer server.Close()
	models, err := fetchModels(t.Context(), server.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].id != "a" || models[0].detail != "128k context" || models[1].id != "plain" {
		t.Fatalf("models = %+v", models)
	}
	if _, err := fetchModels(t.Context(), server.URL, "wrong"); err == nil || !strings.Contains(friendlyError(err), "rejected") {
		t.Fatalf("bad key err = %v", err)
	}
}

func TestEnvPrecedenceUserThenWorkspace(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	getenv := wizardGetenv(home)
	if err := os.MkdirAll(configDirectory(getenv), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfigPath(getenv), []byte("TEST_SCOPE_VAR=user\nTEST_SCOPE_USERONLY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte("TEST_SCOPE_VAR=workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("TEST_SCOPE_VAR")
	defer os.Unsetenv("TEST_SCOPE_USERONLY")

	user, err := loadDotEnv(userConfigPath(getenv))
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	ws, err := loadDotEnvInto(filepath.Join(workspace, ".env"), user.setNames())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	if got := os.Getenv("TEST_SCOPE_VAR"); got != "workspace" {
		t.Fatalf("TEST_SCOPE_VAR = %q, want workspace", got)
	}
	if got := os.Getenv("TEST_SCOPE_USERONLY"); got != "1" {
		t.Fatalf("TEST_SCOPE_USERONLY = %q", got)
	}

	// Process env still wins over both files.
	t.Setenv("TEST_SCOPE_ENV", "process")
	if err := os.WriteFile(userConfigPath(getenv), []byte("TEST_SCOPE_ENV=user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u2, _ := loadDotEnv(userConfigPath(getenv))
	defer u2.Close()
	if got := os.Getenv("TEST_SCOPE_ENV"); got != "process" {
		t.Fatalf("TEST_SCOPE_ENV = %q, want process", got)
	}
}

func TestSetupRequiresTerminal(t *testing.T) {
	var stderr bytes.Buffer
	code := RunMain(
		t.Context(),
		[]string{"setup"},
		func(string) string { return "" },
		func() []string { return nil },
		strings.NewReader(""),
		&bytes.Buffer{},
		&stderr,
		interactiveConfig(&fakeClient{}),
	)
	if code != 1 || !strings.Contains(stderr.String(), "requires a terminal") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestMissingCredsHintsSetup(t *testing.T) {
	var stderr bytes.Buffer
	code := RunMain(
		t.Context(),
		[]string{"-p", "hi"},
		func(string) string { return "" }, // no HOME/user config, no keys
		func() []string { return nil },
		strings.NewReader(""),
		&bytes.Buffer{},
		&stderr,
		interactiveConfig(&fakeClient{}),
	)
	if code != 1 || !strings.Contains(stderr.String(), "run `heu setup` in a terminal") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestCodeFromPasted(t *testing.T) {
	code, err := codeFromPasted("http://localhost:1455/auth/callback?code=abc&state=s1\n", "s1")
	if err != nil || code != "abc" {
		t.Fatalf("url paste: %q %v", code, err)
	}
	if _, err := codeFromPasted("http://localhost:1455/auth/callback?code=abc&state=other", "s1"); err == nil {
		t.Fatal("expected state mismatch")
	}
	code, err = codeFromPasted("raw-code-123\n", "s1")
	if err != nil || code != "raw-code-123" {
		t.Fatalf("bare code: %q %v", code, err)
	}
}

func TestExchangeCodeAgainstTestServer(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id_token":"id","access_token":"at","refresh_token":"rt"}`)
	}))
	defer server.Close()
	old := codexOAuthIssuer
	codexOAuthIssuer = server.URL
	defer func() { codexOAuthIssuer = old }()

	pkce := generatePKCE()
	tokens, err := exchangeCode(t.Context(), codexOAuthIssuer, "http://localhost:1455/auth/callback", pkce, "the-code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tokens.AccessToken != "at" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if gotForm.Get("grant_type") != "authorization_code" ||
		gotForm.Get("client_id") != codexOAuthClientID ||
		gotForm.Get("code_verifier") != pkce.verifier ||
		gotForm.Get("redirect_uri") != "http://localhost:1455/auth/callback" ||
		gotForm.Get("code") != "the-code" {
		t.Fatalf("form = %v", gotForm)
	}
	// Authorize URL mirrors codex-rs params.
	u := authorizeURL(server.URL, "http://localhost:1455/auth/callback", pkce, "st")
	for _, want := range []string{
		"response_type=code", "client_id=" + codexOAuthClientID,
		"code_challenge_method=S256", "state=st",
		"id_token_add_organizations=true", "codex_cli_simplified_flow=true",
		"originator=codex_cli_rs", "redirect_uri=" + url.QueryEscape("http://localhost:1455/auth/callback"),
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("authorize URL missing %q: %s", want, u)
		}
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	token := "h." + base64.RawURLEncoding.EncodeToString([]byte(
		`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`)) + ".s"
	if got := accountIDFromIDToken(token); got != "acct-1" {
		t.Fatalf("accountID = %q", got)
	}
	if got := accountIDFromIDToken("notajwt"); got != "" {
		t.Fatalf("accountID = %q", got)
	}
}

func TestIsAuthError(t *testing.T) {
	for raw, want := range map[string]bool{
		`responses API error UNAUTHORIZED: Invalid 'Authorization' header or token.`: true,
		"status 401: invalid api key":  true,
		"status 429: rate limit":       false,
		"dial tcp: connection refused": false,
	} {
		if got := isAuthError(errors.New(raw)); got != want {
			t.Errorf("isAuthError(%q) = %v, want %v", raw, got, want)
		}
	}
}
