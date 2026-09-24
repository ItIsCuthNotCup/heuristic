package agentrunner

// ChatGPT/Codex OAuth sign-in, mirroring codex-rs login/src/server.rs and
// pkce.rs: PKCE authorization-code flow on a localhost callback listener
// (port 1455, fallback 1457), token exchange at {issuer}/oauth/token, and
// the Codex auth.json file format.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm/clients/openaicodex"
)

// Mirror codex-rs constants (login/src/server.rs, auth/manager.rs,
// auth/default_client.rs).
const (
	codexOAuthClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthOriginator = "codex_cli_rs"
	codexOAuthScopes     = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

// codexOAuthIssuer is a package var so tests can point the flow at an
// httptest server (the token endpoint is {issuer}/oauth/token).
var codexOAuthIssuer = "https://auth.openai.com"

var codexOAuthPorts = []int{1455, 1457}

type pkceCodes struct {
	verifier  string
	challenge string
}

// generatePKCE mirrors codex-rs pkce.rs: 64 random bytes base64url-nopad
// verifier; challenge = base64url-nopad(sha256(verifier)).
func generatePKCE() pkceCodes {
	raw := make([]byte, 64)
	_, _ = rand.Read(raw)
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return pkceCodes{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}
}

func randomState() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// authorizeURL mirrors codex-rs build_authorize_url: {issuer}/oauth/authorize
// with the simplified-CLI-flow parameters.
func authorizeURL(issuer, redirectURI string, pkce pkceCodes, state string) string {
	values := url.Values{
		"response_type":              {"code"},
		"client_id":                  {codexOAuthClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {codexOAuthScopes},
		"code_challenge":             {pkce.challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {codexOAuthOriginator},
	}
	return strings.TrimRight(issuer, "/") + "/oauth/authorize?" + values.Encode()
}

type exchangedTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// exchangeCode mirrors codex-rs exchange_code_for_tokens: form POST of the
// authorization_code grant to {issuer}/oauth/token.
func exchangeCode(ctx context.Context, issuer, redirectURI string, pkce pkceCodes, code string) (exchangedTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {codexOAuthClientID},
		"code_verifier": {pkce.verifier},
	}
	var tokens exchangedTokens
	if err := postOAuthForm(ctx, strings.TrimRight(issuer, "/")+"/oauth/token", form, &tokens); err != nil {
		return exchangedTokens{}, err
	}
	return tokens, nil
}

func postOAuthForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("invalid token response: %w", err)
	}
	return nil
}

// accountIDFromIDToken mirrors token_data.rs: the ChatGPT account id is the
// "https://api.openai.com/auth".chatgpt_account_id claim of the id_token.
func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// codexAuthFilePath is where the wizard stores the new login.
func codexAuthFilePath(getenv func(string) string) string {
	return filepath.Join(configDirectory(getenv), "codex-auth.json")
}

// openBrowser best-effort launches a browser; failure is ignored (SSH).
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// codexLogin runs the PKCE sign-in: serves the localhost callback while also
// accepting a pasted redirect URL or bare code on stdin (headless/SSH).
// Whichever code arrives first wins. Returns the written auth file path.
func codexLogin(ctx context.Context, reader *bufio.Reader, out io.Writer, getenv func(string) string) (string, error) {
	pkce := generatePKCE()
	state := randomState()

	var listener net.Listener
	var port int
	for _, candidate := range codexOAuthPorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err == nil {
			listener, port = l, candidate
			break
		}
	}
	if listener == nil {
		return "", errors.New("cannot listen on localhost ports 1455/1457 for the sign-in callback")
	}
	defer func() { _ = listener.Close() }()

	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	authURL := authorizeURL(codexOAuthIssuer, redirectURI, pkce, state)

	fmt.Fprintf(out, "\nSign in with OpenAI in your browser:\n\n  %s\n\n", authURL)
	fmt.Fprintln(out, "If the browser can't reach this machine (e.g. SSH), paste the")
	fmt.Fprintln(out, "full URL you were redirected to here:")
	openBrowser(authURL)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	// Local callback server.
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("state") != state {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if msg := query.Get("error"); msg != "" {
			http.Error(w, "Sign-in error: "+msg, http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("authorization error: %s", msg):
			default:
			}
			return
		}
		code := query.Get("code")
		if code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h2>Heuristic sign-in complete.</h2>You can close this tab.</body></html>")
		select {
		case codeCh <- code:
		default:
		}
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// Pasted-URL fallback.
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			select {
			case errCh <- errors.New("no sign-in response received"):
			default:
			}
			return
		}
		code, err := codeFromPasted(line, state)
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		select {
		case codeCh <- code:
		default:
		}
	}()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(10 * time.Minute):
		return "", errors.New("sign-in timed out")
	}

	tokens, err := exchangeCode(ctx, codexOAuthIssuer, redirectURI, pkce, code)
	if err != nil {
		return "", fmt.Errorf("exchange sign-in code: %w", err)
	}
	if tokens.AccessToken == "" {
		return "", errors.New("token exchange returned no access token")
	}

	path := codexAuthFilePath(getenv)
	if err := ensureConfigDirectory(getenv); err != nil {
		return "", err
	}
	auth := openaicodex.AuthFile{
		Mode:        "chatgpt",
		LastRefresh: time.Now().UTC().Format(time.RFC3339),
	}
	auth.Tokens.IDToken = tokens.IDToken
	auth.Tokens.AccessToken = tokens.AccessToken
	auth.Tokens.RefreshToken = tokens.RefreshToken
	auth.Tokens.AccountID = accountIDFromIDToken(tokens.IDToken)
	if err := openaicodex.WriteAuthFile(path, auth); err != nil {
		return "", fmt.Errorf("save codex credentials: %w", err)
	}
	return path, nil
}

// codeFromPasted accepts a full redirect URL or a bare authorization code.
func codeFromPasted(line, expectedState string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("empty sign-in response")
	}
	if strings.HasPrefix(line, "http") {
		parsed, err := url.Parse(line)
		if err != nil {
			return "", fmt.Errorf("could not parse pasted URL: %w", err)
		}
		query := parsed.Query()
		if st := query.Get("state"); st != "" && st != expectedState {
			return "", errors.New("pasted URL state does not match this sign-in")
		}
		if msg := query.Get("error"); msg != "" {
			return "", fmt.Errorf("authorization error: %s", msg)
		}
		if code := query.Get("code"); code != "" {
			return code, nil
		}
		return "", errors.New("pasted URL contains no authorization code")
	}
	return line, nil
}
