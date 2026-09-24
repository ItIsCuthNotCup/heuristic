package agentrunner

import (
	"bytes"
	"context"
	"encoding/base64"
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
// names fall through to the process environment (wizard reload works).
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

func readConfigEnv(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".config", "heuristic", "config.env"))
	if err != nil {
		t.Fatalf("read config.env: %v", err)
	}
	return string(data)
}

func TestSetupWizardOpenAI(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	// provider 1, model default, api key, judge 3 (off)
	input := strings.NewReader("1\n\nsk-test-secret\n3\n")
	if err := runSetupWizard(context.Background(), input, &out, wizardGetenv(home)); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	config := readConfigEnv(t, home)
	for _, want := range []string{
		"HEURISTIC_LLM_PROVIDER=openai",
		"HEURISTIC_LLM_MODEL=gpt-6-astra",
		"HEURISTIC_LLM_API_KEY=sk-test-secret",
		"HEURISTIC_JUDGE=off",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config.env missing %q:\n%s", want, config)
		}
	}
	info, err := os.Stat(filepath.Join(home, ".config", "heuristic", "config.env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.env perm: %v %v", info, err)
	}
	if strings.Contains(out.String(), "sk-test-secret") {
		t.Fatal("wizard echoed the API key")
	}
}

func TestSetupWizardOllama(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	input := strings.NewReader("5\nqwen3:4b\n3\n")
	if err := runSetupWizard(context.Background(), input, &out, wizardGetenv(home)); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	config := readConfigEnv(t, home)
	if !strings.Contains(config, "HEURISTIC_LLM_PROVIDER=ollama") ||
		!strings.Contains(config, "HEURISTIC_LLM_MODEL=qwen3:4b") {
		t.Fatalf("config.env:\n%s", config)
	}
	if strings.Contains(config, "API_KEY") {
		t.Fatalf("ollama path wrote a key:\n%s", config)
	}
}

func TestSetupWizardCustomEndpoint(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	input := strings.NewReader("6\n\n\ncc-bogus\n2\nhttp://localhost:11434/v1\nqwen3:4b\n")
	if err := runSetupWizard(context.Background(), input, &out, wizardGetenv(home)); err != nil {
		t.Fatalf("wizard: %v", err)
	}
	config := readConfigEnv(t, home)
	for _, want := range []string{
		"HEURISTIC_LLM_PROVIDER=openai",
		"HEURISTIC_LLM_BASE_URL=https://api.commandcode.ai/provider/v1",
		"HEURISTIC_LLM_MODEL=moonshotai/Kimi-K2.5",
		"HEURISTIC_LLM_API_KEY=cc-bogus",
		"HEURISTIC_JUDGE=local",
		"HEURISTIC_JUDGE_URL=http://localhost:11434/v1",
		"HEURISTIC_JUDGE_MODEL=qwen3:4b",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config.env missing %q:\n%s", want, config)
		}
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
