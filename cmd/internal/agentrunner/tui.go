package agentrunner

// The heu terminal UI: transcript in normal scrollback, and a live region
// with a spinner, a multi-line editor, a slash-command menu and a footer.
// Layout and keys follow Claude Code / pi conventions: Enter sends,
// Shift/Alt+Enter or Ctrl-J adds a line, Esc interrupts, Ctrl-C twice exits.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
)

type slashCommand struct {
	name string
	args string
	help string
}

var slashCommands = []slashCommand{
	{name: "/model", help: "switch model"},
	{name: "/login", help: "sign in or change provider"},
	{name: "/paths", help: "see MetaCog's thought paths and switch between them"},
	{name: "/metacog", args: "[on|off]", help: "turn MetaCog on or off"},
	{name: "/new", help: "start a fresh conversation"},
	{name: "/resume", help: "pick up an earlier conversation"},
	{name: "/session", help: "show this session's id"},
	{name: "/clear", help: "clear the screen"},
	{name: "/logout", help: "forget saved sign-in and keys"},
	{name: "/help", help: "commands and shortcuts"},
	{name: "/quit", help: "exit"},
}

// status is what the footer shows.
type status struct {
	model     string
	provider  string
	metacog   string
	workspace string
	session   string
	tokens    int64
}

type tui struct {
	c *console
	p palette

	mu       sync.Mutex
	buf      []rune
	cur      int
	history  []string
	histPos  int
	draft    string
	menuSel  int
	working  bool
	since    time.Time
	activity string
	frame    int
	notice   string
	st       status
	lastTool string
	paths    *thoughtPaths
	mcStage  string            // what MetaCog is doing now, "" when idle
	switched map[string]string // thought-path switches MetaCog made itself
}

func newTUI(c *console) *tui {
	return &tui{c: c, p: palette{on: c.color}}
}

// logoLines is the terminal mark (docs/logo.svg): an H whose stems fork
// into paths at both ends and meet in one node.
func logoLines(p palette) []string {
	fork := p.dim("• ") + "│" + p.dim(" •")
	return []string{
		"  ●       ●",
		fork + "   " + fork,
		"╰─┼─╯   ╰─┼─╯",
		"  │       │",
		"  ├───" + p.accent("●") + "───┤",
		"  │       │",
		"╭─┼─╮   ╭─┼─╮",
		fork + "   " + fork,
		"  ●       ●",
	}
}

func (t *tui) banner(version string) {
	p := t.p
	logo := logoLines(p)
	logo[3] += "    " + p.bold("Heuristic") + p.dim(version)
	logo[5] += "    " + p.dim("an agent that doesn't overthink")
	lines := []string{""}
	for _, line := range logo {
		lines = append(lines, "  "+line)
	}
	t.c.Print(strings.Join(append(lines, ""), "\n") + "\n")
}

func (t *tui) sessionHeader(st status) {
	p := t.p
	t.mu.Lock()
	t.st.model, t.st.provider, t.st.metacog, t.st.workspace, t.st.session = st.model, st.provider, st.metacog, st.workspace, st.session
	t.mu.Unlock()
	t.c.Print("  " + p.bold(st.model) + p.dim(" · "+st.provider+" · "+st.metacog) + "\n" +
		"  " + p.dim(shortPath(st.workspace)+" · runs commands here without asking · /help") + "\n\n")
}

func shortPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return "~/" + rel
		}
	}
	return path
}

// view renders the live region: spinner, editor, menu, footer.
func (t *tui) view(width int) ([]string, int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.p
	var lines []string
	if t.working {
		elapsed := int(time.Since(t.since).Seconds())
		spin := p.accent(spinnerFrames[t.frame%len(spinnerFrames)])
		activity := t.activity
		if t.mcStage != "" {
			activity += p.dim(" · MetaCog is " + t.mcStage + "…")
		}
		lines = append(lines, fit(fmt.Sprintf("%s %s %s", spin, activity,
			p.dim(fmt.Sprintf("(%ds · esc to interrupt)", elapsed))), width))
	}
	rule := p.dim(strings.Repeat("─", max(width, 1)))
	lines = append(lines, rule)
	editorLines, cursorRow, cursorCol := t.editorLines(width)
	cursorRow += len(lines)
	lines = append(lines, editorLines...)
	lines = append(lines, rule)
	if menu := t.menuMatches(); len(menu) > 0 {
		for i, cmd := range menu {
			name := cmd.name
			if cmd.args != "" {
				name += " " + cmd.args
			}
			line := "  " + fmt.Sprintf("%-20s", name) + p.dim(cmd.help)
			if i == t.menuSel {
				line = "  " + p.accent(fmt.Sprintf("%-20s", name)) + cmd.help
			}
			lines = append(lines, fit(line, width))
		}
	} else {
		lines = append(lines, fit(t.footerLocked(width), width))
	}
	return lines, cursorRow, cursorCol
}

func (t *tui) footerLocked(width int) string {
	p := t.p
	if t.notice != "" {
		return "  " + p.yellow(t.notice)
	}
	mc := t.st.metacog
	if t.mcStage != "" && !t.working {
		mc = "MetaCog is " + t.mcStage + "…"
	}
	parts := []string{t.st.model, t.st.provider, mc}
	if t.st.tokens > 0 {
		parts = append(parts, formatTokens(t.st.tokens)+" tokens")
	}
	left := "  " + strings.Join(parts, " · ")
	right := "? /help"
	if t.working {
		right = "esc interrupt"
	}
	pad := width - visibleLen(left) - visibleLen(right) - 1
	if pad < 2 {
		return p.dim(left)
	}
	return p.dim(left + strings.Repeat(" ", pad) + right)
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// editorLines wraps the buffer at width-2 with a "❯ " / "  " gutter and
// returns the cursor's row and column within those lines.
func (t *tui) editorLines(width int) ([]string, int, int) {
	p := t.p
	avail := max(width-3, 10)
	var lines []string
	row, col := 0, 2
	current := []rune{}
	currentWidth := 0
	flush := func() {
		prefix := "  "
		if len(lines) == 0 {
			prefix = p.accent("❯") + " "
		}
		lines = append(lines, prefix+string(current))
		current, currentWidth = current[:0:0], 0
	}
	placeholder := len(t.buf) == 0
	for i, r := range t.buf {
		if i == t.cur {
			row, col = len(lines), 2+currentWidth
		}
		if r == '\n' {
			flush()
			continue
		}
		w := runeWidth(r)
		if currentWidth+w > avail {
			flush()
		}
		current = append(current, r)
		currentWidth += w
	}
	if t.cur >= len(t.buf) {
		if currentWidth >= avail {
			flush()
		}
		row, col = len(lines), 2+currentWidth
	}
	flush()
	if placeholder {
		hint := "Ask anything, or / for commands"
		if t.working {
			hint = "Type to queue a follow-up"
		}
		lines[0] = p.accent("❯") + " " + p.dim(hint)
	}
	return lines, row, col
}

func (t *tui) menuMatches() []slashCommand {
	text := string(t.buf)
	if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \n") {
		return nil
	}
	var out []slashCommand
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd.name, text) {
			out = append(out, cmd)
		}
	}
	if len(out) == 1 && out[0].name == text {
		return out
	}
	return out
}

func (t *tui) setWorking(activity string) {
	t.mu.Lock()
	if !t.working {
		t.since = time.Now()
	}
	t.working = true
	t.activity = activity
	t.mu.Unlock()
	t.c.Redraw()
}

func (t *tui) setIdle() {
	t.mu.Lock()
	t.working = false
	t.mu.Unlock()
	t.c.Redraw()
}

func (t *tui) isWorking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.working
}

func (t *tui) addUsage(usage llm.Usage) {
	t.mu.Lock()
	t.st.tokens += usage.InputTokens + usage.OutputTokens
	t.mu.Unlock()
}

func (t *tui) setNotice(text string) {
	t.mu.Lock()
	t.notice = text
	t.mu.Unlock()
	t.c.Redraw()
}

func (t *tui) setMetacogStage(stage string) {
	t.mu.Lock()
	t.mcStage = stage
	t.mu.Unlock()
	t.c.Redraw()
}

// takeSwitches returns and clears the thought-path switches MetaCog made on
// its own, so they can be reapplied to a new session.
func (t *tui) takeSwitches() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.switched
	t.switched = nil
	return out
}

func (t *tui) tick() {
	t.mu.Lock()
	working := t.working
	t.frame++
	t.mu.Unlock()
	if working {
		t.c.Redraw()
	}
}

func (t *tui) metacogEvent(event metacog.Event) {
	p := t.p
	secs := float64(event.DurationMs) / 1000
	var line string
	if event.Stopped && event.Skipped {
		line = fmt.Sprintf("%s %s %s", p.accent("◆"), p.bold("MetaCog"),
			p.dim(fmt.Sprintf("simple request — answered directly · %.1fs", secs)))
	} else if event.Stopped {
		line = fmt.Sprintf("%s %s %s", p.accent("◆"), p.bold("MetaCog"),
			p.dim(fmt.Sprintf("confident (%.2f) — kept the first answer · %.1fs", event.GreedyScore, secs)))
	} else if agree := countTrue(event.Agree); agree > 0 {
		line = fmt.Sprintf("%s %s %s", p.accent("◆"), p.bold("MetaCog"),
			p.dim(fmt.Sprintf("%d of %d thought paths agree → Thought Path %d · %.1fs",
				agree, len(event.Paths), event.Chosen+1, secs)))
	} else if event.Tree {
		outcome := "kept the first answer"
		switch {
		case event.Answer != "":
			outcome = fmt.Sprintf("merged the best of %d paths into the answer", len(event.Paths))
		case event.Chosen >= 0 && event.Chosen < len(event.Scores):
			outcome = fmt.Sprintf("Thought Path %d (%.2f)", event.Chosen+1, event.Scores[event.Chosen])
		}
		line = fmt.Sprintf("%s %s %s", p.accent("◆"), p.bold("MetaCog"),
			p.dim(fmt.Sprintf("unsure (%.2f) → %d concise thought paths → %s · %.1fs",
				event.GreedyScore, len(event.Paths), outcome, secs)))
	} else if len(event.Scores) == 0 {
		line = fmt.Sprintf("%s %s %s", p.accent("◆"), p.bold("MetaCog"),
			p.dim(fmt.Sprintf("unsure (%.2f) → tried %d more thought paths → kept the first answer · %.1fs",
				event.GreedyScore, event.Branches, secs)))
	} else {
		score := 0.0
		if event.Chosen < len(event.Scores) {
			score = event.Scores[event.Chosen]
		}
		line = fmt.Sprintf("%s %s %s", p.accent("◆"), p.bold("MetaCog"),
			p.dim(fmt.Sprintf("unsure (%.2f) → tried %d more thought paths → picked #%d (%.2f) · %d judge calls · %.1fs",
				event.GreedyScore, event.Branches, event.Chosen+1, score, event.JudgeCalls, secs)))
	}
	if event.Background {
		t.addUsage(event.ExtraUsage)
		line = t.backgroundLine(event)
	}
	t.recordPaths(event)
	t.mu.Lock()
	paths := t.paths
	t.mu.Unlock()
	if (event.Final || event.Tree) && len(event.Paths) > 1 && paths != nil {
		line += "\n" + strings.TrimSuffix(t.pathList(paths, t.c.width()), "\n")
		if event.Background && event.Chosen != 0 && !event.Tree {
			label := fmt.Sprintf("Thought Path %d", event.Chosen+1)
			line += "\n\n" + p.accent("◆ ") + p.bold(label) + "\n" +
				renderMarkdown(strings.TrimSpace(event.Paths[event.Chosen]), p) + "\n" +
				p.dim("  your next message continues from "+label)
		}
	}
	if event.Tree && event.Answer != "" {
		line += "\n\n" + p.accent("◆ ") + p.bold("MetaCog merged a better answer") + "\n" +
			renderMarkdown(strings.TrimSpace(event.Answer), p) + "\n" +
			p.dim("  your next message continues from it")
	}
	t.c.Print(line + "\n")
}

// Editing primitives; callers hold no locks.

func (t *tui) insert(text string) {
	t.mu.Lock()
	runes := []rune(text)
	t.buf = append(t.buf[:t.cur], append(runes, t.buf[t.cur:]...)...)
	t.cur += len(runes)
	t.menuSel = 0
	t.notice = ""
	t.mu.Unlock()
}

func (t *tui) take() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := string(t.buf)
	t.buf, t.cur, t.menuSel = nil, 0, 0
	if strings.TrimSpace(text) != "" && (len(t.history) == 0 || t.history[len(t.history)-1] != text) {
		t.history = append(t.history, text)
	}
	t.histPos = len(t.history)
	return text
}

func (t *tui) setBuffer(text string) {
	t.mu.Lock()
	t.buf = []rune(text)
	t.cur = len(t.buf)
	t.menuSel = 0
	t.mu.Unlock()
}

func (t *tui) bufferText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// handleEditKey applies an editing key. It reports false for keys the
// caller must handle (Enter, Esc, Ctrl-C, Ctrl-D, Ctrl-L, Ctrl-O).
func (t *tui) handleEditKey(event keyEvent) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notice = ""
	menu := t.menuMatches()
	switch event.kind {
	case keyRune:
		t.buf = append(t.buf[:t.cur], append([]rune{event.r}, t.buf[t.cur:]...)...)
		t.cur++
		t.menuSel = 0
	case keyPaste:
		runes := []rune(event.text)
		t.buf = append(t.buf[:t.cur], append(runes, t.buf[t.cur:]...)...)
		t.cur += len(runes)
	case keyNewline:
		t.buf = append(t.buf[:t.cur], append([]rune{'\n'}, t.buf[t.cur:]...)...)
		t.cur++
	case keyBackspace:
		if t.cur > 0 {
			t.buf = append(t.buf[:t.cur-1], t.buf[t.cur:]...)
			t.cur--
			t.menuSel = 0
		}
	case keyDelete:
		if t.cur < len(t.buf) {
			t.buf = append(t.buf[:t.cur], t.buf[t.cur+1:]...)
		}
	case keyLeft:
		if t.cur > 0 {
			t.cur--
		}
	case keyRight:
		if t.cur < len(t.buf) {
			t.cur++
		}
	case keyHome:
		for t.cur > 0 && t.buf[t.cur-1] != '\n' {
			t.cur--
		}
	case keyEnd:
		for t.cur < len(t.buf) && t.buf[t.cur] != '\n' {
			t.cur++
		}
	case keyCtrlU:
		start := t.cur
		for start > 0 && t.buf[start-1] != '\n' {
			start--
		}
		t.buf = append(t.buf[:start], t.buf[t.cur:]...)
		t.cur = start
	case keyCtrlW:
		start := t.cur
		for start > 0 && t.buf[start-1] == ' ' {
			start--
		}
		for start > 0 && t.buf[start-1] != ' ' && t.buf[start-1] != '\n' {
			start--
		}
		t.buf = append(t.buf[:start], t.buf[t.cur:]...)
		t.cur = start
	case keyTab:
		if len(menu) > 0 {
			t.buf = []rune(menu[t.menuSel].name + " ")
			t.cur = len(t.buf)
			t.menuSel = 0
		}
	case keyUp:
		switch {
		case len(menu) > 0:
			t.menuSel = (t.menuSel - 1 + len(menu)) % len(menu)
		case !containsRune(t.buf[:t.cur], '\n') && t.histPos > 0:
			if t.histPos == len(t.history) {
				t.draft = string(t.buf)
			}
			t.histPos--
			t.buf = []rune(t.history[t.histPos])
			t.cur = len(t.buf)
		default:
			t.moveVertical(-1)
		}
	case keyDown:
		switch {
		case len(menu) > 0:
			t.menuSel = (t.menuSel + 1) % len(menu)
		case !containsRune(t.buf[t.cur:], '\n') && t.histPos < len(t.history):
			t.histPos++
			if t.histPos == len(t.history) {
				t.buf = []rune(t.draft)
			} else {
				t.buf = []rune(t.history[t.histPos])
			}
			t.cur = len(t.buf)
		default:
			t.moveVertical(1)
		}
	default:
		return false
	}
	return true
}

func containsRune(runes []rune, target rune) bool {
	for _, r := range runes {
		if r == target {
			return true
		}
	}
	return false
}

// moveVertical moves the cursor one logical line up or down, keeping the
// column where possible.
func (t *tui) moveVertical(direction int) {
	lineStart := t.cur
	for lineStart > 0 && t.buf[lineStart-1] != '\n' {
		lineStart--
	}
	column := t.cur - lineStart
	if direction < 0 {
		if lineStart == 0 {
			return
		}
		prevStart := lineStart - 1
		for prevStart > 0 && t.buf[prevStart-1] != '\n' {
			prevStart--
		}
		t.cur = min(prevStart+column, lineStart-1)
		return
	}
	next := t.cur
	for next < len(t.buf) && t.buf[next] != '\n' {
		next++
	}
	if next >= len(t.buf) {
		return
	}
	nextStart := next + 1
	nextEnd := nextStart
	for nextEnd < len(t.buf) && t.buf[nextEnd] != '\n' {
		nextEnd++
	}
	t.cur = min(nextStart+column, nextEnd)
}

// selectedCommand returns the highlighted menu command, if the menu is open.
func (t *tui) selectedCommand() (slashCommand, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	menu := t.menuMatches()
	if len(menu) == 0 {
		return slashCommand{}, false
	}
	return menu[min(t.menuSel, len(menu)-1)], true
}

func (t *tui) echoUser(text string) {
	p := t.p
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		prefix := "  "
		if i == 0 {
			prefix = p.dim("❯ ")
		}
		lines[i] = prefix + p.bold(line)
	}
	t.c.Print("\n" + strings.Join(lines, "\n") + "\n\n")
}

func (t *tui) helpText() string {
	p := t.p
	var b strings.Builder
	b.WriteString(p.bold("Commands") + "\n")
	for _, cmd := range slashCommands {
		name := cmd.name
		if cmd.args != "" {
			name += " " + cmd.args
		}
		fmt.Fprintf(&b, "  %-20s %s\n", name, p.dim(cmd.help))
	}
	fmt.Fprintf(&b, "  %-20s %s\n", "!<command>", p.dim("run a shell command yourself"))
	b.WriteString("\n" + p.bold("Keys") + "\n")
	for _, pair := range [][2]string{
		{"enter", "send"},
		{"shift+enter · ctrl+j", "new line (or end a line with \\)"},
		{"↑ ↓", "history"},
		{"esc", "interrupt the agent · clear the input"},
		{"ctrl+c twice", "exit"},
		{"ctrl+o", "show the full output of the last command"},
		{"ctrl+t", "open MetaCog's thought paths"},
		{"ctrl+l", "clear the screen"},
	} {
		fmt.Fprintf(&b, "  %-20s %s\n", pair[0], p.dim(pair[1]))
	}
	return b.String()
}

// backgroundLine is the MetaCog line for a check that ran after the first
// answer was already shown.
func (t *tui) backgroundLine(event metacog.Event) string {
	p := t.p
	head := p.accent("◆") + " " + p.bold("MetaCog") + " "
	secs := float64(event.DurationMs) / 1000
	switch {
	case event.Stopped && event.Skipped:
		return head + p.dim(fmt.Sprintf("simple request — answered directly · %.1fs", secs))
	case event.Stopped:
		return head + p.dim(fmt.Sprintf("checked the answer: confident (%.2f) · %.1fs", event.GreedyScore, secs))
	case event.Tree && event.Answer != "":
		return head + fmt.Sprintf("found a better answer — merged the best of %d concise thought paths",
			len(event.Paths)) + p.dim(fmt.Sprintf(" · %.1fs", secs))
	case event.Tree && event.Chosen >= 0:
		return head + fmt.Sprintf("found a better answer: Thought Path %d (%.2f)",
			event.Chosen+1, event.Scores[event.Chosen]) + p.dim(fmt.Sprintf(" · %.1fs", secs))
	case event.Tree:
		return head + p.dim(fmt.Sprintf("tried %d concise thought paths — the first answer holds · %.1fs",
			len(event.Paths), secs))
	case len(event.Agree) > 0 && event.Chosen == 0:
		return head + p.dim(fmt.Sprintf("double-checked: %d of %d thought paths agree with the first answer · %.1fs",
			countTrue(event.Agree), len(event.Paths), secs))
	case len(event.Agree) > 0:
		return head + fmt.Sprintf("found a better answer: Thought Path %d (%d of %d thought paths agree)", event.Chosen+1,
			countTrue(event.Agree), len(event.Paths)) + p.dim(fmt.Sprintf(" · %.1fs", secs))
	case len(event.Scores) == 0 || event.Chosen == 0:
		return head + p.dim(fmt.Sprintf("double-checked with %d more thought paths: the first answer holds · %.1fs",
			event.Branches, secs))
	}
	return head + fmt.Sprintf("found a better answer: Thought Path %d (%.2f vs %.2f)", event.Chosen+1,
		event.Scores[event.Chosen], event.Scores[0]) + p.dim(fmt.Sprintf(" · %.1fs", secs))
}
