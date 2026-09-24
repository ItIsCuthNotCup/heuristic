package openaicodex

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OAuth refresh, mirroring codex-rs login/src/auth/manager.rs.
const oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// refreshTokenURL is the ChatGPT OAuth token endpoint; a package var so
// tests can point it at an httptest server.
var refreshTokenURL = "https://auth.openai.com/oauth/token"

// refreshSkew refreshes access tokens within this window of expiry.
const refreshSkew = 5 * time.Minute

// AuthFile mirrors the Codex CLI auth.json shape.
type AuthFile = authFile

// authFile mirrors the Codex CLI auth.json shape.
type authFile struct {
	Mode   string `json:"auth_mode"`
	Tokens struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

type credentials struct {
	accessToken string
	accountID   string
}

// EnvironmentConfig selects a source without reading files or consulting API keys.
func EnvironmentConfig(getenv func(string) string) (Config, error) {
	config := Config{
		AccessToken: strings.TrimSpace(getenv("OPENAI_CODEX_ACCESS_TOKEN")),
		AccountID:   strings.TrimSpace(getenv("OPENAI_CODEX_ACCOUNT_ID")),
		AuthFile:    strings.TrimSpace(getenv("OPENAI_CODEX_AUTH_FILE")),
	}
	if config.AuthFile != "" && (config.AccessToken != "" || config.AccountID != "") {
		return Config{}, errors.New("choose OPENAI_CODEX_AUTH_FILE or OPENAI_CODEX_ACCESS_TOKEN/OPENAI_CODEX_ACCOUNT_ID, not both")
	}
	if config.AccessToken != "" || config.AccountID != "" || config.AuthFile != "" {
		return config, nil
	}
	home := strings.TrimSpace(getenv("CODEX_HOME"))
	if home == "" {
		userHome := strings.TrimSpace(getenv("HOME"))
		if userHome == "" {
			var err error
			userHome, err = os.UserHomeDir()
			if err != nil {
				return Config{}, fmt.Errorf("find Codex home: %w", err)
			}
		}
		home = filepath.Join(userHome, ".codex")
	}
	config.AuthFile = filepath.Join(home, "auth.json")
	return config, nil
}

func (config Config) credentials() (credentials, error) {
	token, accountID := strings.TrimSpace(config.AccessToken), strings.TrimSpace(config.AccountID)
	if config.AuthFile != "" {
		if token != "" || accountID != "" {
			return credentials{}, errors.New("codex AuthFile cannot be combined with AccessToken or AccountID")
		}
		auth, err := readAuthFile(config.AuthFile)
		if err != nil {
			return credentials{}, err
		}
		token, accountID = auth.Tokens.AccessToken, auth.Tokens.AccountID
		if token != "" {
			token, err = refreshIfNeeded(config.AuthFile, auth)
			if err != nil {
				return credentials{}, err
			}
		}
	}
	if token == "" {
		return credentials{}, errors.New("codex access token must be set; use OPENAI_CODEX_ACCESS_TOKEN or a ChatGPT-authenticated Codex auth file")
	}
	if strings.HasPrefix(token, "sk-") || !headerValue(token) {
		return credentials{}, errors.New("codex requires a subscription access token, not an API key or invalid header value")
	}
	claimAccount, expires, err := tokenClaims(token)
	if err != nil {
		return credentials{}, err
	}
	if expires != 0 && time.Now().Unix() >= expires {
		return credentials{}, errors.New("codex access token has expired and the auth file has no refresh token; run `heu login`")
	}
	if accountID == "" {
		accountID = claimAccount
	} else if claimAccount != "" && accountID != claimAccount {
		return credentials{}, errors.New("codex account ID does not match the access token")
	}
	if accountID == "" || !headerValue(accountID) {
		return credentials{}, errors.New("codex account ID must be set in OPENAI_CODEX_ACCOUNT_ID, the auth file, or the access token")
	}
	return credentials{accessToken: token, accountID: accountID}, nil
}

// ReadAuthFile loads a Codex auth.json for inspection by callers such as
// the interactive setup wizard.
func ReadAuthFile(path string) (authFile, error) {
	return readAuthFile(path)
}

func readAuthFile(path string) (authFile, error) {
	var auth authFile
	file, err := os.Open(path)
	if err != nil {
		return auth, fmt.Errorf("open Codex auth file (run `heu login` to sign in): %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return auth, fmt.Errorf("inspect Codex auth file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return auth, errors.New("codex auth file must be a regular file with private permissions (chmod 600)")
	}
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return auth, fmt.Errorf("read Codex auth file: %w", err)
	}
	if len(data) > limit || json.Unmarshal(data, &auth) != nil {
		return auth, errors.New("invalid Codex auth file; expected a JSON object with tokens.access_token and tokens.account_id")
	}
	if auth.Mode != "" && auth.Mode != "chatgpt" {
		return auth, errors.New("codex auth file is not a ChatGPT subscription login")
	}
	return auth, nil
}

// WriteAuthFile persists a Codex auth.json atomically with 0600
// permissions. Exported for the interactive login flow.
func WriteAuthFile(path string, auth authFile) error {
	data, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-auth-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err == nil {
		err = tmp.Chmod(0o600)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// refreshIfNeeded exchanges the stored refresh token for a new access token
// when the current one expires within refreshSkew, rewriting the auth file.
func refreshIfNeeded(path string, auth authFile) (string, error) {
	token := strings.TrimSpace(auth.Tokens.AccessToken)
	_, expires, err := tokenClaims(token)
	if err != nil || expires == 0 || time.Now().Unix() < expires-int64(refreshSkew/time.Second) {
		return token, err
	}
	refresh := strings.TrimSpace(auth.Tokens.RefreshToken)
	if refresh == "" {
		return token, nil // expired-without-refresh is reported by the caller
	}
	form := url.Values{
		"client_id":     {oauthClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}
	resp, err := http.PostForm(refreshTokenURL, form)
	if err != nil {
		return "", fmt.Errorf("refresh codex token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh codex token: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var refreshed struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(body, &refreshed) != nil || refreshed.AccessToken == "" {
		return "", errors.New("refresh codex token: invalid token response")
	}
	auth.Tokens.AccessToken = refreshed.AccessToken
	if refreshed.IDToken != "" {
		auth.Tokens.IDToken = refreshed.IDToken
	}
	if refreshed.RefreshToken != "" {
		auth.Tokens.RefreshToken = refreshed.RefreshToken
	}
	auth.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	if err := WriteAuthFile(path, auth); err != nil {
		return "", fmt.Errorf("store refreshed codex token: %w", err)
	}
	return refreshed.AccessToken, nil
}

// Unverified claims are routing/expiry hints; the server authenticates the token.
func tokenClaims(token string) (string, int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", 0, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", 0, errors.New("invalid Codex access token claims")
	}
	var claims struct {
		Expires int64 `json:"exp"`
		Auth    struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", 0, errors.New("invalid Codex access token claims")
	}
	return claims.Auth.AccountID, claims.Expires, nil
}

func headerValue(value string) bool {
	for _, char := range value {
		if char <= ' ' || char > '~' {
			return false
		}
	}
	return value != ""
}
