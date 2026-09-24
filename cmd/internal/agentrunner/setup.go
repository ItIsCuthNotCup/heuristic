package agentrunner

// Interactive onboarding for heu: a line-prompt wizard that writes
// ~/.config/heuristic/config.env (XDG_CONFIG_HOME respected) so first run
// can sign the user in or collect an API key without a manual .env step.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm/clients/openaicodex"
)

const commandCodeBaseURL = "https://api.commandcode.ai/provider/v1"

// configDirectory is ~/.config/heuristic (or $XDG_CONFIG_HOME/heuristic).
func configDirectory(getenv func(string) string) string {
	if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "heuristic")
	}
	home := strings.TrimSpace(getenv("HOME"))
	if home == "" {
		if resolved, err := os.UserHomeDir(); err == nil {
			home = resolved
		}
	}
	return filepath.Join(home, ".config", "heuristic")
}

func userConfigPath(getenv func(string) string) string {
	return filepath.Join(configDirectory(getenv), "config.env")
}

func ensureConfigDirectory(getenv func(string) string) error {
	if err := os.MkdirAll(configDirectory(getenv), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return nil
}

// writeConfigFile merges the given KEY=VALUE pairs into config.env,
// preserving unrelated existing entries, with 0600 permissions.
func writeConfigFile(getenv func(string) string, values map[string]string) error {
	path := userConfigPath(getenv)
	if err := ensureConfigDirectory(getenv); err != nil {
		return err
	}
	order, merged := []string{}, map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			name, value, ok := strings.Cut(line, "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" || strings.HasPrefix(name, "#") {
				continue
			}
			order = append(order, name)
			merged[name] = strings.TrimSpace(value)
		}
	}
	for name, value := range values {
		if _, seen := merged[name]; !seen {
			order = append(order, name)
		}
		merged[name] = value
	}
	var body strings.Builder
	for _, name := range order {
		fmt.Fprintf(&body, "%s=%s\n", name, merged[name])
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

type setupIO struct {
	reader *bufio.Reader
	out    io.Writer
	input  io.Reader
}

// prompt asks a line question; Enter accepts the default.
func (s *setupIO) prompt(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(s.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(s.out, "%s: ", label)
	}
	line, err := s.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// secret reads a key without echo on a real terminal; falls back to a
// normal line read for non-TTY inputs (tests).
func (s *setupIO) secret(label string) (string, error) {
	fmt.Fprintf(s.out, "%s: ", label)
	if file, ok := s.input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		raw, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(s.out)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	line, err := s.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (s *setupIO) choice(label string, options []string, def int) (int, error) {
	fmt.Fprintln(s.out, label)
	for i, option := range options {
		fmt.Fprintf(s.out, "  %d) %s\n", i+1, option)
	}
	for {
		answer, err := s.prompt("Choose", fmt.Sprint(def))
		if err != nil {
			return 0, err
		}
		var n int
		if _, err := fmt.Sscanf(answer, "%d", &n); err == nil && n >= 1 && n <= len(options) {
			return n, nil
		}
		fmt.Fprintln(s.out, "Please enter a number from the list.")
	}
}

// runSetupWizard guides the user through provider/key/model/judge selection
// and writes the user config file. It is also the `heu setup` / `heu login`
// entry point.
func runSetupWizard(ctx context.Context, input io.Reader, output io.Writer, getenv func(string) string) error {
	io_ := &setupIO{reader: bufio.NewReader(input), out: output, input: input}
	fmt.Fprintln(output, "Heuristic setup")
	values := map[string]string{}

	provider, err := io_.choice("Where should Heuristic get its model?", []string{
		"OpenAI (API key)",
		"ChatGPT / Codex subscription (sign in with OpenAI)",
		"OpenRouter (API key)",
		"Fireworks (API key)",
		"Ollama (local, no key)",
		"Other OpenAI-compatible endpoint (e.g. CommandCode)",
	}, 1)
	if err != nil {
		return err
	}

	switch provider {
	case 1: // OpenAI
		values["HEURISTIC_LLM_PROVIDER"] = "openai"
		model, err := io_.prompt("Model", "gpt-6-astra")
		if err != nil {
			return err
		}
		values["HEURISTIC_LLM_MODEL"] = model
		key, err := io_.secret("OpenAI API key")
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("an API key is required for the OpenAI provider")
		}
		values["HEURISTIC_LLM_API_KEY"] = key

	case 2: // ChatGPT / Codex OAuth
		values["HEURISTIC_LLM_PROVIDER"] = "openai-codex"
		model, err := io_.prompt("Model", "gpt-5.1-codex")
		if err != nil {
			return err
		}
		values["HEURISTIC_LLM_MODEL"] = model
		authPath := existingCodexLogin(getenv)
		useExisting := false
		if authPath != "" {
			answer, err := io_.prompt(fmt.Sprintf("Found an existing Codex login at %s — use it? (y/n)", authPath), "y")
			if err != nil {
				return err
			}
			useExisting = strings.HasPrefix(strings.ToLower(answer), "y")
		}
		if useExisting {
			values["OPENAI_CODEX_AUTH_FILE"] = authPath
		} else {
			path, err := codexLogin(ctx, io_.reader, output, getenv)
			if err != nil {
				return err
			}
			values["OPENAI_CODEX_AUTH_FILE"] = path
			fmt.Fprintf(output, "Signed in; credentials saved to %s\n", path)
		}

	case 3: // OpenRouter
		values["HEURISTIC_LLM_PROVIDER"] = "openrouter"
		model, err := io_.prompt("Model", "")
		if err != nil {
			return err
		}
		if model == "" {
			return errors.New("a model is required (e.g. openai/gpt-5.1)")
		}
		values["HEURISTIC_LLM_MODEL"] = model
		key, err := io_.secret("OpenRouter API key")
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("an API key is required for the OpenRouter provider")
		}
		values["HEURISTIC_LLM_API_KEY"] = key

	case 4: // Fireworks
		values["HEURISTIC_LLM_PROVIDER"] = "fireworks"
		model, err := io_.prompt("Model", "")
		if err != nil {
			return err
		}
		if model == "" {
			return errors.New("a model is required (e.g. accounts/fireworks/models/kimi-k2p5)")
		}
		values["HEURISTIC_LLM_MODEL"] = model
		key, err := io_.secret("Fireworks API key")
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("an API key is required for the Fireworks provider")
		}
		values["HEURISTIC_LLM_API_KEY"] = key

	case 5: // Ollama
		values["HEURISTIC_LLM_PROVIDER"] = "ollama"
		model, err := io_.prompt("Model (a tag from `ollama list`)", "qwen3:8b")
		if err != nil {
			return err
		}
		values["HEURISTIC_LLM_MODEL"] = model

	case 6: // Other OpenAI-compatible endpoint
		values["HEURISTIC_LLM_PROVIDER"] = "openai"
		baseURL, err := io_.prompt("Base URL", commandCodeBaseURL)
		if err != nil {
			return err
		}
		values["HEURISTIC_LLM_BASE_URL"] = baseURL
		modelDefault := ""
		if baseURL == commandCodeBaseURL {
			modelDefault = "moonshotai/Kimi-K2.5"
		}
		model, err := io_.prompt("Model", modelDefault)
		if err != nil {
			return err
		}
		if model == "" {
			return errors.New("a model is required")
		}
		values["HEURISTIC_LLM_MODEL"] = model
		key, err := io_.secret("API key")
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("an API key is required")
		}
		values["HEURISTIC_LLM_API_KEY"] = key
	}

	judge, err := io_.choice("Enable MetaCog judging?", []string{
		"Jev (TypeSafe API key, get one at https://typesafe.ai)",
		"Local judge (OpenAI-compatible logprobs endpoint)",
		"Off for now",
	}, 1)
	if err != nil {
		return err
	}
	switch judge {
	case 1:
		key, err := io_.secret("TypeSafe API key")
		if err != nil {
			return err
		}
		if key != "" {
			values["HEURISTIC_JEV_API_KEY"] = key
		} else {
			values["HEURISTIC_JUDGE"] = "off"
			fmt.Fprintln(output, "No key given — judging disabled (re-run `heu setup` to add it).")
		}
	case 2:
		url, err := io_.prompt("Judge endpoint URL (OpenAI-compatible)", "http://localhost:11434/v1")
		if err != nil {
			return err
		}
		model, err := io_.prompt("Judge model", "")
		if err != nil {
			return err
		}
		if model == "" {
			return errors.New("a judge model is required")
		}
		values["HEURISTIC_JUDGE"] = "local"
		values["HEURISTIC_JUDGE_URL"] = url
		values["HEURISTIC_JUDGE_MODEL"] = model
	case 3:
		values["HEURISTIC_JUDGE"] = "off"
	}

	if err := writeConfigFile(getenv, values); err != nil {
		return err
	}
	fmt.Fprintf(output, "Saved to %s. Edit it or run `heu setup` to change.\n", userConfigPath(getenv))
	return nil
}

// existingCodexLogin returns ~/.codex/auth.json when it parses as a ChatGPT
// login, else "".
func existingCodexLogin(getenv func(string) string) string {
	home := strings.TrimSpace(getenv("CODEX_HOME"))
	if home == "" {
		userHome := strings.TrimSpace(getenv("HOME"))
		if userHome == "" {
			return ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	path := filepath.Join(home, "auth.json")
	if _, err := openaicodex.ReadAuthFile(path); err != nil {
		return ""
	}
	return path
}

// isTerminalIO reports whether both streams are character devices.
func isTerminalIO(input io.Reader, output io.Writer) bool {
	in, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return false
	}
	out, ok := output.(*os.File)
	return ok && term.IsTerminal(int(out.Fd()))
}
