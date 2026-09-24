package agentrunner

// Session control for the terminal UI: runs agent sessions one after
// another (new, resumed, or restarted after /model, /login, /metacog or an
// interrupt) and turns keys into edits, messages and commands.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"uuid"

	"encoding/json/v2"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore/localfile"
)

// runTerminalSetup runs onboarding on a raw terminal and returns the saved
// values.
func runTerminalSetup(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	getenv func(string) string,
	providers []Provider,
) (map[string]string, error) {
	in, out := input.(*os.File), output.(*os.File)
	c, err := openConsole(in, out, getenv)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	t := newTUI(c)
	t.banner("")
	values, err := runSetupWizard(ctx, newTTYAsker(c), getenv, defaultSetupDeps(providers, getenv))
	if errors.Is(err, errCancelled) || errors.Is(err, errBack) || errors.Is(err, io.EOF) {
		return nil, errors.New("setup cancelled — run `heu setup` when you're ready")
	}
	return values, err
}

// nextSession says what the UI does once the current session has stopped.
type nextSession struct {
	quit      bool
	sessionID *string
	initial   []RequestMessage
	replay    bool
	// after runs with no session active, e.g. a picker that changes the
	// model before the session restarts.
	after func(ctx context.Context, next *nextSession)
}

type tuiLoop struct {
	t         *tui
	spec      sessionSpec
	base      func(string) string
	overrides map[string]string
	header    string
	lastCtrlC time.Time
	// swaps are thought-path switches, reapplied to every new session.
	swaps map[string]string
}

func runTUI(ctx context.Context, spec sessionSpec) error {
	c, err := openConsole(spec.input.(*os.File), spec.output.(*os.File), spec.getenv)
	if err != nil {
		return err
	}
	defer c.Close()
	m := &tuiLoop{t: newTUI(c), spec: spec, base: spec.getenv, overrides: map[string]string{}, swaps: map[string]string{}}
	m.spec.getenv = overlayEnv(m.base, m.overrides)
	m.t.banner("")
	if resolved, err := resolveClient(m.spec); err == nil {
		_ = resolved.client.Close()
	} else {
		values, err := runSetupWizard(ctx, newTTYAsker(c), m.spec.getenv, m.deps())
		if errors.Is(err, errCancelled) || errors.Is(err, errBack) || errors.Is(err, io.EOF) {
			c.Print("Setup cancelled. Run heu again when you're ready.\n")
			return nil
		}
		if err != nil {
			return err
		}
		maps.Copy(m.overrides, values)
	}
	return m.run(ctx)
}

func (m *tuiLoop) deps() setupDeps {
	return defaultSetupDeps(m.spec.config.Providers, m.spec.getenv)
}

func (m *tuiLoop) run(ctx context.Context) error {
	next := nextSession{sessionID: m.spec.parsed.SessionID, initial: m.spec.messages, replay: m.spec.parsed.SessionID != nil}
	for {
		if next.after != nil {
			after := next.after
			next.after = nil
			after(ctx, &next)
			m.t.c.SetView(m.t.view)
		}
		if next.replay {
			m.replay(ctx, next.sessionID)
		}
		result, err := m.drive(ctx, next)
		if err != nil {
			return err
		}
		if result.quit {
			return nil
		}
		next = result
	}
}

// drive runs one session until the user asks for something that needs a
// different one. A session that ends on its own (an error) leaves the UI
// usable; the next message restarts it.
func (m *tuiLoop) drive(ctx context.Context, start nextSession) (nextSession, error) {
	t, p := m.t, m.t.p
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	spec := m.spec
	spec.ui = t
	spec.parsed.SessionID = start.sessionID
	spec.messages = start.initial
	ready := make(chan sessionHandle, 1)
	spec.onReady = func(h sessionHandle) { ready <- h }
	done := make(chan error, 1)
	go func() { done <- runSession(sessionCtx, spec) }()
	running := true
	var handle *sessionHandle
	currentID := start.sessionID
	var queued []string
	var pending *nextSession
	var forceStop <-chan time.Time
	t.c.SetView(t.view)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	stopThen := func(next nextSession) (nextSession, bool) {
		if !running {
			return next, true
		}
		pending = &next
		if handle != nil {
			submitControl(handle, inbox.StopHard)
		}
		forceStop = time.After(5 * time.Second)
		return nextSession{}, false
	}
	send := func(text string) (nextSession, bool) {
		t.echoUser(text)
		switch {
		case !running:
			return nextSession{sessionID: currentID, initial: []RequestMessage{{Content: text}}}, true
		case handle == nil:
			queued = append(queued, text)
		default:
			submitText(handle, text)
		}
		return nextSession{}, false
	}

	for {
		select {
		case <-ctx.Done():
			return nextSession{quit: true}, nil
		case h := <-ready:
			handle = &h
			id := string(h.id)
			currentID = &id
			m.showHeader(h)
			for original, replacement := range m.t.takeSwitches() {
				m.swaps[original] = replacement
			}
			if h.metacog != nil {
				for original, replacement := range m.swaps {
					h.metacog.UsePath(original, replacement)
				}
			}
			for _, text := range queued {
				submitText(handle, text)
			}
			queued = nil
		case err := <-done:
			running = false
			handle = nil
			t.setIdle()
			if pending != nil {
				return *pending, nil
			}
			if err != nil && ctx.Err() == nil && isAuthError(err) {
				t.c.Print(p.red("✗ ") + friendlyError(err) + "\n" + p.dim("  Let's sign in again.") + "\n")
				return nextSession{sessionID: currentID, after: m.login}, nil
			}
			if err != nil && ctx.Err() == nil {
				t.c.Print(p.red("✗ ") + friendlyError(err) + "\n" + p.dim("  "+errorHint(err, m.base)) + "\n")
			}
		case <-forceStop:
			cancel()
		case <-ticker.C:
			t.tick()
		case event, ok := <-t.c.keys:
			if !ok {
				event = keyEvent{kind: keyCtrlD}
			}
			var next nextSession
			finished := false
			switch event.kind {
			case keyEnter:
				if cmd, open := t.selectedCommand(); open && t.bufferText() != cmd.name {
					if cmd.args != "" {
						t.setBuffer(cmd.name + " ")
						break
					}
					t.setBuffer(cmd.name)
				}
				text := t.bufferText()
				if strings.HasSuffix(text, "\\") {
					t.setBuffer(strings.TrimSuffix(text, "\\") + "\n")
					break
				}
				text = strings.TrimSpace(t.take())
				if text == "" {
					break
				}
				switch {
				case strings.HasPrefix(text, "/"):
					next, finished = m.command(ctx, text, handle, running, stopThen)
				case strings.HasPrefix(text, "!"):
					m.shell(ctx, strings.TrimSpace(strings.TrimPrefix(text, "!")))
				default:
					next, finished = send(text)
				}
			case keyEsc:
				if running && t.isWorking() && handle != nil {
					t.c.Print(p.dim("  ⎿ Interrupted · tell the agent what to do instead") + "\n")
					next, finished = stopThen(nextSession{sessionID: currentID})
				} else {
					t.setBuffer("")
				}
			case keyCtrlC:
				switch {
				case running && t.isWorking() && handle != nil:
					t.c.Print(p.dim("  ⎿ Interrupted · tell the agent what to do instead") + "\n")
					next, finished = stopThen(nextSession{sessionID: currentID})
				case t.bufferText() != "":
					t.setBuffer("")
				case time.Since(m.lastCtrlC) < 2*time.Second:
					next, finished = stopThen(m.quit(currentID))
				default:
					m.lastCtrlC = time.Now()
					t.setNotice("Press Ctrl-C again to exit")
				}
			case keyCtrlD:
				if t.bufferText() == "" {
					next, finished = stopThen(m.quit(currentID))
				}
			case keyCtrlL:
				t.c.Print("\x1b[H\x1b[2J")
			case keyCtrlT:
				if running && t.isWorking() {
					t.setNotice("The agent is working — press Esc to interrupt first")
					break
				}
				m.explorePaths(ctx, handle)
			case keyCtrlO:
				t.mu.Lock()
				last := t.lastTool
				t.mu.Unlock()
				if last == "" {
					t.setNotice("No command output yet")
				} else {
					t.c.Print(p.dim(last) + "\n")
				}
			case keyResize:
			default:
				t.handleEditKey(event)
			}
			if finished {
				return next, nil
			}
			t.c.Redraw()
		}
	}
}

func (m *tuiLoop) quit(id *string) nextSession {
	if id != nil {
		name := m.spec.config.Name
		m.t.c.Print(m.t.p.dim(fmt.Sprintf("\nResume this conversation with: %s -resume %s", name, *id)) + "\n")
	}
	return nextSession{quit: true}
}

func (m *tuiLoop) showHeader(h sessionHandle) {
	st := status{
		model:     modelDisplayName(h.model),
		provider:  providerDisplayName(h.provider, h.baseURL),
		metacog:   metacogLabel(h.judge),
		workspace: m.spec.workspace,
		session:   string(h.id),
	}
	key := st.model + "|" + st.provider + "|" + st.metacog
	if key == m.header {
		m.t.mu.Lock()
		m.t.st.session = st.session
		m.t.mu.Unlock()
		return
	}
	m.header = key
	m.t.sessionHeader(st)
}

func metacogLabel(judge string) string {
	switch judge {
	case "off", "":
		return "MetaCog off"
	case "local":
		return "MetaCog on (local judge)"
	}
	return "MetaCog on"
}

func submitText(h *sessionHandle, text string) {
	payload, err := json.Marshal(text)
	if err != nil {
		return
	}
	_ = h.inputs.Submit(h.context, inbox.Input{
		ID: inbox.ID(uuid.New().String()), Kind: inbox.InputExternal, Payload: payload,
	})
}

func submitControl(h *sessionHandle, mode inbox.ControlMode) {
	payload, err := json.Marshal(inbox.ControlMessage{Mode: mode})
	if err != nil {
		return
	}
	_ = h.inputs.Submit(h.context, inbox.Input{
		ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload,
	})
}

func (m *tuiLoop) command(
	ctx context.Context,
	text string,
	handle *sessionHandle,
	running bool,
	stopThen func(nextSession) (nextSession, bool),
) (nextSession, bool) {
	t, p := m.t, m.t.p
	name, arg, _ := strings.Cut(text, " ")
	arg = strings.TrimSpace(arg)
	var currentID *string
	if handle != nil {
		id := string(handle.id)
		currentID = &id
	} else {
		currentID = m.spec.parsed.SessionID
	}
	busy := running && t.isWorking()
	needsIdle := func() bool {
		if busy {
			t.setNotice("The agent is working — press Esc to interrupt first")
			return true
		}
		return false
	}
	switch name {
	case "/help", "/?":
		t.c.Print(t.helpText())
	case "/quit", "/exit":
		return stopThen(m.quit(currentID))
	case "/session":
		if currentID == nil {
			t.c.Print(p.dim("No conversation yet.") + "\n")
			break
		}
		t.c.Print(fmt.Sprintf("Session %s\n%s\n", *currentID,
			p.dim(fmt.Sprintf("Resume later with: %s -resume %s", m.spec.config.Name, *currentID))))
	case "/clear":
		t.c.Print("\x1b[H\x1b[2J")
	case "/new":
		if needsIdle() {
			break
		}
		t.c.Print(p.dim("── new conversation ──") + "\n")
		return stopThen(nextSession{})
	case "/model", "/models":
		if needsIdle() {
			break
		}
		return stopThen(nextSession{sessionID: currentID, after: m.pickModel})
	case "/login", "/setup", "/provider":
		if needsIdle() {
			break
		}
		return stopThen(nextSession{sessionID: currentID, after: m.login})
	case "/logout":
		if needsIdle() {
			break
		}
		return stopThen(nextSession{sessionID: currentID, after: m.logout})
	case "/metacog":
		if needsIdle() {
			break
		}
		return stopThen(nextSession{sessionID: currentID, after: func(ctx context.Context, next *nextSession) {
			m.setMetaCog(ctx, arg)
		}})
	case "/paths":
		if needsIdle() {
			break
		}
		m.explorePaths(ctx, handle)
	case "/resume":
		if needsIdle() {
			break
		}
		return stopThen(nextSession{sessionID: currentID, after: m.pickSession})
	default:
		t.setNotice("Unknown command " + name + " — type / to see commands")
	}
	return nextSession{}, false
}

func (m *tuiLoop) explorePaths(ctx context.Context, handle *sessionHandle) {
	m.t.explorePaths(ctx, func(original, replacement string) {
		for o, r := range m.t.takeSwitches() {
			m.swaps[o] = r
		}
		m.swaps[original] = replacement
		if handle != nil && handle.metacog != nil {
			handle.metacog.UsePath(original, replacement)
		}
	})
}

// apply makes values take effect for the next session and saves them.
func (m *tuiLoop) apply(values map[string]string) {
	maps.Copy(m.overrides, values)
	if err := writeConfigFile(m.base, values); err != nil {
		m.t.c.Print(m.t.p.red("✗ ") + "Couldn't save settings: " + err.Error() + "\n")
	}
}

func (m *tuiLoop) currentConnection() connection {
	getenv := m.spec.getenv
	provider := strings.TrimSpace(lookupEnv(getenv, llmProviderEnvironment))
	if provider == "" {
		provider = defaultProvider
	}
	baseURL := strings.TrimRight(strings.TrimSpace(lookupEnv(getenv, llmBaseURLEnvironment)), "/")
	conn := connection{baseURL: baseURL, apiKey: lookupEnv(getenv, llmAPIKeyEnvironment)}
	for _, spec := range providerCatalog() {
		if spec.provider != provider || spec.id == "custom" {
			continue
		}
		if spec.baseURL == baseURL || (spec.baseURL == "" && (baseURL == "" || spec.id == "openai" && strings.Contains(baseURL, "api.openai.com"))) {
			conn.spec = spec
			break
		}
	}
	if conn.spec.id == "" {
		conn.spec = providerSpec{id: "custom", provider: provider, label: providerDisplayName(provider, baseURL)}
	}
	for _, p := range m.spec.config.Providers {
		if p.Name == provider && conn.apiKey == "" && p.APIKeyEnvironment != "" {
			conn.apiKey = getenv(p.APIKeyEnvironment)
		}
	}
	if conn.apiKey == "" {
		conn.apiKey = envKey(conn.spec, getenv)
	}
	return conn
}

func (m *tuiLoop) pickModel(ctx context.Context, _ *nextSession) {
	a := newTTYAsker(m.t.c)
	model, err := chooseModel(ctx, a, m.currentConnection(), m.deps())
	if err != nil {
		return
	}
	m.apply(map[string]string{"HEURISTIC_LLM_MODEL": model})
	m.t.c.Print(m.t.p.green("✓") + " Switched to " + modelDisplayName(model) + "\n")
}

func (m *tuiLoop) login(ctx context.Context, _ *nextSession) {
	values, err := runSetupWizard(ctx, newTTYAsker(m.t.c), m.spec.getenv, m.deps())
	if err != nil {
		return
	}
	maps.Copy(m.overrides, values)
}

func (m *tuiLoop) logout(ctx context.Context, next *nextSession) {
	cleared := map[string]string{}
	for _, name := range []string{
		"HEURISTIC_LLM_PROVIDER", "HEURISTIC_LLM_MODEL", "HEURISTIC_LLM_BASE_URL",
		"HEURISTIC_LLM_API_KEY", "OPENAI_CODEX_AUTH_FILE", "HEURISTIC_JEV_API_KEY", "HEURISTIC_JUDGE",
	} {
		cleared[name] = ""
	}
	m.apply(cleared)
	_ = os.Remove(codexAuthFilePath(m.base))
	m.t.c.Print(m.t.p.green("✓") + " Signed out and removed saved keys from " + userConfigPath(m.base) + "\n")
	m.login(ctx, next)
}

func (m *tuiLoop) setMetaCog(ctx context.Context, arg string) {
	p := m.t.p
	switch strings.ToLower(arg) {
	case "off":
		m.apply(map[string]string{"HEURISTIC_JUDGE": "off"})
		m.t.c.Print(p.dim("MetaCog is off — the model answers on its own.") + "\n")
		return
	case "on":
		if jevAPIKey(m.spec.getenv) != "" {
			m.apply(map[string]string{"HEURISTIC_JUDGE": "jev"})
			m.t.c.Print(p.green("✓") + " MetaCog is on\n")
			return
		}
	}
	values := map[string]string{}
	if err := chooseJudge(ctx, newTTYAsker(m.t.c), m.spec.getenv, values); err != nil {
		return
	}
	if values["HEURISTIC_JUDGE"] == "" {
		values["HEURISTIC_JUDGE"] = "jev"
	}
	m.apply(values)
}

type sessionChoice struct {
	id      string
	preview string
	updated time.Time
}

func (m *tuiLoop) pickSession(ctx context.Context, next *nextSession) {
	p := m.t.p
	store, err := m.store()
	if err != nil {
		m.t.c.Print(p.red("✗ ") + err.Error() + "\n")
		return
	}
	infos, err := store.ListSessions(ctx)
	if err != nil {
		m.t.c.Print(p.red("✗ ") + err.Error() + "\n")
		return
	}
	slices.SortFunc(infos, func(x, y sessionstore.SessionInfo) int { return y.LastUpdatedAt.Compare(x.LastUpdatedAt) })
	var choices []sessionChoice
	for _, info := range infos {
		preview := firstUserMessage(ctx, store, string(info.ID))
		if preview == "" {
			continue
		}
		choices = append(choices, sessionChoice{id: string(info.ID), preview: preview, updated: info.LastUpdatedAt})
		if len(choices) == 30 {
			break
		}
	}
	if len(choices) == 0 {
		m.t.c.Print(p.dim("No earlier conversations in this folder.") + "\n")
		return
	}
	options := make([]option, len(choices))
	for i, choice := range choices {
		detail := ago(choice.updated)
		if next.sessionID != nil && *next.sessionID == choice.id {
			detail += " · current"
		}
		options[i] = option{label: truncateRunes(firstLine(choice.preview), 60), detail: detail}
	}
	index, err := newTTYAsker(m.t.c).choose("Resume a conversation", "", options, 0)
	if err != nil {
		return
	}
	id := choices[index].id
	next.sessionID = &id
	next.replay = true
	m.header = ""
}

func (m *tuiLoop) store() (*localfile.Store, error) {
	return openSessionStore(m.spec.sessionDirectory)
}

func openSessionStore(directory string) (*localfile.Store, error) {
	dir, err := filepath.Abs(strings.TrimSpace(directory))
	if err != nil {
		return nil, err
	}
	return localfile.New(dir)
}

// latestSessionID returns the most recently updated session with at least
// one user message.
func latestSessionID(ctx context.Context, directory string) (string, error) {
	store, err := openSessionStore(directory)
	if err != nil {
		return "", err
	}
	infos, err := store.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	slices.SortFunc(infos, func(x, y sessionstore.SessionInfo) int { return y.LastUpdatedAt.Compare(x.LastUpdatedAt) })
	for _, info := range infos {
		if firstUserMessage(ctx, store, string(info.ID)) != "" {
			return string(info.ID), nil
		}
	}
	return "", errors.New("no earlier conversation to continue in this folder")
}

func firstUserMessage(ctx context.Context, store *localfile.Store, id string) string {
	page, err := store.Items(ctx, session.ID(id), 0, 64)
	if err != nil {
		return ""
	}
	for _, item := range page.Items {
		if text, ok := userText(item); ok {
			return text
		}
	}
	return ""
}

func userText(item sessionstore.Item) (string, bool) {
	input, ok := item.Data.(inbox.Input)
	if item.Kind != sessionstore.ItemInput || !ok || input.Kind != inbox.InputExternal {
		return "", false
	}
	var text string
	if json.Unmarshal(input.Payload, &text) != nil {
		return "", false
	}
	return text, true
}

func ago(when time.Time) string {
	d := time.Since(when)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// replay prints an earlier conversation so a resumed session has context
// on screen.
func (m *tuiLoop) replay(ctx context.Context, id *string) {
	if id == nil {
		return
	}
	store, err := m.store()
	if err != nil {
		return
	}
	r := &renderer{ui: m.t}
	var after sessionstore.Sequence
	for {
		page, err := store.Items(ctx, session.ID(*id), after, 500)
		if err != nil {
			return
		}
		for _, item := range page.Items {
			if text, ok := userText(item); ok {
				m.t.echoUser(text)
				continue
			}
			if item.Kind != sessionstore.ItemInput {
				r.observeUI(item)
			}
		}
		if !page.More {
			break
		}
		after = page.NextAfter
	}
	m.t.setIdle()
	m.t.c.Print(m.t.p.dim("── resumed ──") + "\n")
}

// shell runs a `!command` typed by the user in the workspace.
func (m *tuiLoop) shell(ctx context.Context, command string) {
	t, p := m.t, m.t.p
	if command == "" {
		return
	}
	shell := strings.TrimSpace(m.spec.getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	t.c.Print(p.yellow("!") + " " + p.bold(command) + "\n")
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Dir = m.spec.workspace
	out, err := cmd.CombinedOutput()
	text := strings.TrimRight(string(out), "\n")
	t.mu.Lock()
	t.lastTool = text
	t.mu.Unlock()
	var b strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if i == 40 {
			b.WriteString(p.dim("    … (ctrl+o for all)") + "\n")
			break
		}
		prefix := "    "
		if i == 0 {
			prefix = "  ⎿ "
		}
		b.WriteString(p.dim(prefix+line) + "\n")
	}
	if err != nil {
		b.WriteString("    " + p.red(err.Error()) + "\n")
	}
	t.c.Print(b.String())
}
