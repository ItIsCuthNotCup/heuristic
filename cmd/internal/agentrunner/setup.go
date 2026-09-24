package agentrunner

// User configuration for heu: ~/.config/heuristic/config.env
// (XDG_CONFIG_HOME respected), written by onboarding with 0600 permissions.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm/clients/openaicodex"
)

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
// preserving unrelated existing entries, with 0600 permissions. An empty
// value removes the entry.
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
		if merged[name] == "" {
			continue
		}
		fmt.Fprintf(&body, "%s=%s\n", name, merged[name])
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// existingCodexLogin returns Heuristic's own saved ChatGPT sign-in, else
// ~/.codex/auth.json when it parses as a ChatGPT login, else "".
func existingCodexLogin(getenv func(string) string) string {
	if own := codexAuthFilePath(getenv); fileParsesAsCodexAuth(own) {
		return own
	}
	home := strings.TrimSpace(getenv("CODEX_HOME"))
	if home == "" {
		userHome := strings.TrimSpace(getenv("HOME"))
		if userHome == "" {
			return ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	path := filepath.Join(home, "auth.json")
	if !fileParsesAsCodexAuth(path) {
		return ""
	}
	return path
}

func fileParsesAsCodexAuth(path string) bool {
	_, err := openaicodex.ReadAuthFile(path)
	return err == nil
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
