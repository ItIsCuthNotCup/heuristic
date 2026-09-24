package openaicodex

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testToken(account string, expires int64) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, expires, account))) + ".signature"
}

func writeTestAuth(t *testing.T, path, token, account string) {
	t.Helper()
	data := []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":%q,"refresh_token":"never-use-or-write"}}`, token, account))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCredentials(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	for _, test := range []struct {
		name   string
		config Config
		match  string
	}{
		{name: "explicit", config: Config{AccessToken: "opaque", AccountID: "account"}},
		{name: "claims", config: Config{AccessToken: testToken("account", future)}},
		{name: "matching account", config: Config{AccessToken: testToken("account", future), AccountID: "account"}},
		{name: "missing token", match: "access token must be set"},
		{name: "missing account", config: Config{AccessToken: "opaque"}, match: "account ID must be set"},
		{name: "API key", config: Config{AccessToken: "sk-secret", AccountID: "account"}, match: "not an API key"},
		{name: "expired", config: Config{AccessToken: testToken("account", time.Now().Add(-time.Hour).Unix())}, match: "expired"},
		{name: "mismatched account", config: Config{AccessToken: testToken("other", future), AccountID: "account"}, match: "does not match"},
		{name: "invalid claims", config: Config{AccessToken: "e30.invalid.payload", AccountID: "account"}, match: "invalid Codex"},
		{name: "header injection", config: Config{AccessToken: "opaque\r\nX-Bad: value", AccountID: "account"}, match: "invalid header"},
		{name: "account injection", config: Config{AccessToken: "opaque", AccountID: "account\nX-Bad:value"}, match: "account ID"},
		{name: "conflicting sources", config: Config{AccessToken: "opaque", AuthFile: "unused"}, match: "cannot be combined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.config.credentials()
			if test.match != "" {
				if err == nil || !strings.Contains(err.Error(), test.match) {
					t.Fatalf("error = %v", err)
				}
				if strings.Contains(err.Error(), "sk-secret") || strings.Contains(err.Error(), "e30.") {
					t.Fatal("error exposed credential")
				}
				return
			}
			if err != nil || got.accountID != "account" || got.accessToken != test.config.AccessToken {
				t.Fatalf("invalid credentials result: %v", err)
			}
		})
	}
}

func TestReadAuthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeTestAuth(t, path, "opaque", "account")
	before, _ := os.ReadFile(path)
	got, err := (Config{AuthFile: path}).credentials()
	if err != nil || got.accessToken != "opaque" || got.accountID != "account" {
		t.Fatalf("credentials error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("changed external auth file")
	}
	for _, test := range []struct {
		name, body, match string
		mode              os.FileMode
	}{
		{"public", string(before), "private permissions", 0o644},
		{"invalid", "{bad-secret}", "invalid Codex auth file", 0o600},
		{"API-key mode", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-secret","tokens":{"access_token":"opaque","account_id":"account"}}`, "not a ChatGPT", 0o600},
		{"missing tokens", `{"OPENAI_API_KEY":"sk-secret"}`, "access token must be set", 0o600},
		{"too large", strings.Repeat(" ", 1<<20) + "{}", "invalid Codex auth file", 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := (Config{AuthFile: path}).credentials()
			if err == nil || !strings.Contains(err.Error(), test.match) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := (Config{AuthFile: filepath.Join(t.TempDir(), "missing")}).credentials(); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := (Config{AuthFile: t.TempDir()}).credentials(); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestEnvironmentConfig(t *testing.T) {
	for _, test := range []struct {
		name                 string
		env                  map[string]string
		file, token, account string
		invalid              bool
	}{
		{name: "home default", env: map[string]string{"HOME": "/home/example", "OPENAI_API_KEY": "sk-api", "UNREAL_HARNESS_LLM_API_KEY": "sk-generic"}, file: "/home/example/.codex/auth.json"},
		{name: "codex home", env: map[string]string{"CODEX_HOME": "/custom/codex"}, file: "/custom/codex/auth.json"},
		{name: "explicit file", env: map[string]string{"OPENAI_CODEX_AUTH_FILE": "/custom/auth.json"}, file: "/custom/auth.json"},
		{name: "explicit token", env: map[string]string{"OPENAI_CODEX_ACCESS_TOKEN": " token ", "OPENAI_CODEX_ACCOUNT_ID": " account "}, token: "token", account: "account"},
		{name: "partial token configuration", env: map[string]string{"OPENAI_CODEX_ACCOUNT_ID": "account", "HOME": "/unused"}, account: "account"},
		{name: "conflict", env: map[string]string{"OPENAI_CODEX_AUTH_FILE": "/file", "OPENAI_CODEX_ACCESS_TOKEN": "token"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := EnvironmentConfig(func(key string) string { return test.env[key] })
			if (err != nil) != test.invalid {
				t.Fatalf("error = %v", err)
			}
			if !test.invalid && (config.AuthFile != test.file || config.AccessToken != test.token || config.AccountID != test.account) {
				t.Fatal("incorrect credential source selected")
			}
		})
	}
}

func TestRefreshExpiringToken(t *testing.T) {
	var gotForm string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm = string(body)
		fmt.Fprint(w, `{"access_token":"new-token","refresh_token":"new-refresh","id_token":"id"}`)
	}))
	defer server.Close()
	old := refreshTokenURL
	refreshTokenURL = server.URL
	defer func() { refreshTokenURL = old }()

	path := filepath.Join(t.TempDir(), "auth.json")
	expiring := testToken("account", time.Now().Add(2*time.Minute).Unix())
	writeTestAuth(t, path, expiring, "account")

	got, err := (Config{AuthFile: path}).credentials()
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if got.accessToken != "new-token" {
		t.Fatalf("accessToken = %q", got.accessToken)
	}
	if !strings.Contains(gotForm, "grant_type=refresh_token") ||
		!strings.Contains(gotForm, "refresh_token=never-use-or-write") ||
		!strings.Contains(gotForm, "client_id="+oauthClientID) {
		t.Fatalf("refresh form = %q", gotForm)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "new-token") || !strings.Contains(string(data), "new-refresh") {
		t.Fatalf("auth file not updated: %s", data)
	}
}

func TestRefreshNotNeededForFreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("refresh endpoint hit for a fresh token")
	}))
	defer server.Close()
	old := refreshTokenURL
	refreshTokenURL = server.URL
	defer func() { refreshTokenURL = old }()

	path := filepath.Join(t.TempDir(), "auth.json")
	fresh := testToken("account", time.Now().Add(time.Hour).Unix())
	writeTestAuth(t, path, fresh, "account")
	got, err := (Config{AuthFile: path}).credentials()
	if err != nil || got.accessToken != fresh {
		t.Fatalf("credentials = %q %v", got.accessToken, err)
	}
}

func TestExpiredWithoutRefreshStillErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	data := []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":"account"}}`,
		testToken("account", time.Now().Add(-time.Hour).Unix())))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (Config{AuthFile: path}).credentials()
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("error = %v", err)
	}
}
