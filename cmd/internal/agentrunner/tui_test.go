package agentrunner

import (
	"os"
	"strings"
	"testing"

	"encoding/json/v2"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

// testConsole is a console whose output goes to a file instead of a TTY.
func testConsole(t *testing.T) (*console, func() string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "console")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	c := &console{out: out, keys: make(chan keyEvent, 8), stopSig: func() {}}
	return c, func() string {
		data, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

func typeText(t *tui, text string) {
	for _, r := range text {
		t.handleEditKey(keyEvent{kind: keyRune, r: r})
	}
}

func TestEditorEditing(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	typeText(ui, "hello world")
	ui.handleEditKey(keyEvent{kind: keyCtrlW})
	if got := ui.bufferText(); got != "hello " {
		t.Fatalf("ctrl-w: %q", got)
	}
	ui.handleEditKey(keyEvent{kind: keyHome})
	typeText(ui, ">")
	ui.handleEditKey(keyEvent{kind: keyEnd})
	ui.handleEditKey(keyEvent{kind: keyNewline})
	typeText(ui, "two")
	if got := ui.bufferText(); got != ">hello \ntwo" {
		t.Fatalf("multiline: %q", got)
	}
	ui.handleEditKey(keyEvent{kind: keyCtrlU})
	ui.handleEditKey(keyEvent{kind: keyPaste, text: "pasted\nblock"})
	if got := ui.bufferText(); got != ">hello \npasted\nblock" {
		t.Fatalf("paste: %q", got)
	}
	ui.handleEditKey(keyEvent{kind: keyBackspace})
	ui.handleEditKey(keyEvent{kind: keyLeft})
	ui.handleEditKey(keyEvent{kind: keyDelete})
	if got := ui.bufferText(); got != ">hello \npasted\nblo" {
		t.Fatalf("backspace/delete: %q", got)
	}
}

func TestEditorHistoryKeepsDraft(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	typeText(ui, "first")
	ui.take()
	typeText(ui, "second")
	ui.take()
	typeText(ui, "draft")
	ui.handleEditKey(keyEvent{kind: keyUp})
	if got := ui.bufferText(); got != "second" {
		t.Fatalf("up: %q", got)
	}
	ui.handleEditKey(keyEvent{kind: keyUp})
	if got := ui.bufferText(); got != "first" {
		t.Fatalf("up twice: %q", got)
	}
	ui.handleEditKey(keyEvent{kind: keyDown})
	ui.handleEditKey(keyEvent{kind: keyDown})
	if got := ui.bufferText(); got != "draft" {
		t.Fatalf("draft restored: %q", got)
	}
}

func TestSlashMenu(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	typeText(ui, "/m")
	lines, _, _ := ui.view(80)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "/model") || !strings.Contains(joined, "/metacog") || strings.Contains(joined, "/quit") {
		t.Fatalf("menu:\n%s", joined)
	}
	ui.handleEditKey(keyEvent{kind: keyDown})
	if cmd, open := ui.selectedCommand(); !open || cmd.name != "/metacog" {
		t.Fatalf("selected %+v %v", cmd, open)
	}
	ui.handleEditKey(keyEvent{kind: keyTab})
	if got := ui.bufferText(); got != "/metacog " {
		t.Fatalf("tab: %q", got)
	}
	if _, open := ui.selectedCommand(); open {
		t.Fatal("menu stays open after the command is complete")
	}
}

func TestViewFitsNarrowWidths(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	ui.st = status{model: "Kimi-K2.5", provider: "Command Code", metacog: "MetaCog on", tokens: 12345}
	typeText(ui, strings.Repeat("long input ", 20))
	ui.setWorking("Running a very long command name that will not fit")
	for _, width := range []int{20, 40, 120} {
		lines, row, col := ui.view(width)
		for _, line := range lines {
			if visibleLen(line) > width {
				t.Fatalf("width %d: line %q is %d wide", width, line, visibleLen(line))
			}
		}
		if row >= len(lines) || col > width {
			t.Fatalf("width %d: cursor (%d,%d) outside %d lines", width, row, col, len(lines))
		}
	}
	ui.setIdle()
	ui.setBuffer("")
	lines, _, _ := ui.view(120)
	footer := lines[len(lines)-1]
	for _, want := range []string{"Kimi-K2.5", "Command Code", "MetaCog on", "12.3k tokens"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer %q missing %q", footer, want)
		}
	}
}

func TestNoColorOutputHasNoEscapes(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	ui.banner("")
	ui.echoUser("hi")
	r := &renderer{ui: ui}
	r.modelResponseUI(llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: "**done**"}}}})
	out := read()
	// Only cursor/clear control sequences from the live region, no SGR colors.
	if strings.Contains(out, "\x1b[1m") || strings.Contains(out, "\x1b[2m") || strings.Contains(out, "\x1b[3") {
		t.Fatalf("color escapes with color off: %q", out)
	}
	if !strings.Contains(out, "Heuristic") || !strings.Contains(out, "done") || strings.Contains(out, "**") {
		t.Fatalf("output: %q", out)
	}
}

func TestToolCardCollapsesOutput(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	r := &renderer{ui: ui}
	r.modelResponseUI(llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{
		Name: tool.BashName, Arguments: `{"command":"ls -la"}`,
	}}}})
	if !ui.isWorking() {
		t.Fatal("not working while a tool runs")
	}
	state, err := json.Marshal(operation.ShellState{Result: &operation.ShellResult{
		Out: strings.Repeat("line\n", 10), ExitCode: 2,
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.toolCallStatusUI(sessionstore.ToolCallStatus{
		Status:     tool.CallStatus{WaitingFor: []operation.ID{"op"}},
		Operations: []operation.Operation{{ID: "op", Status: operation.StatusCompleted, State: state}},
	})
	out := read()
	for _, want := range []string{"Bash", "(ls -la)", "⎿ line", "+6 lines (ctrl+o to expand)", "exit 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("card missing %q:\n%s", want, out)
		}
	}
	if ui.lastTool != strings.TrimRight(strings.Repeat("line\n", 10), "\n") {
		t.Fatalf("ctrl+o buffer = %q", ui.lastTool)
	}
}

func TestToolCardWaitingPrintsNothing(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	r := &renderer{ui: ui}
	r.toolCallStatusUI(sessionstore.ToolCallStatus{Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}})
	if strings.Contains(read(), "⎿") {
		t.Fatal("printed a result for a running tool")
	}
}

func TestMetaCogTrace(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	ui.metacogEvent(metacog.Event{GreedyScore: 0.4, Branches: 3, Chosen: 1, Scores: []float64{0.4, 0.91}, JudgeCalls: 8, DurationMs: 2500})
	ui.metacogEvent(metacog.Event{Stopped: true, GreedyScore: 0.97})
	out := read()
	if !strings.Contains(out, "tried 3 more thought paths → picked #2 (0.91)") || !strings.Contains(out, "confident (0.97)") {
		t.Fatalf("trace:\n%s", out)
	}
}

func TestKeyDecoder(t *testing.T) {
	var d keyDecoder
	events := d.decode([]byte("a\x1b[A\x03\x1b\r\x1b[200~x\ny\x1b[201~\r"))
	kinds := []keyKind{}
	for _, e := range events {
		kinds = append(kinds, e.kind)
	}
	want := []keyKind{keyRune, keyUp, keyCtrlC, keyNewline, keyPaste, keyEnter}
	if len(kinds) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d = %v, want %v (%+v)", i, kinds[i], want[i], events)
		}
	}
	if events[4].text != "x\ny" {
		t.Fatalf("paste = %q", events[4].text)
	}
}

func TestMarkdown(t *testing.T) {
	got := renderMarkdown("# Title\nSome **bold** and `code`.\n- item\n```go\nx := 1\n```", palette{})
	for _, want := range []string{"Title", "Some bold and code.", "• item", "x := 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "```") {
		t.Fatalf("raw markers left:\n%s", got)
	}
}

func TestMetacogLabel(t *testing.T) {
	for judge, want := range map[string]string{"off": "MetaCog off", "jev": "MetaCog on", "local": "MetaCog on (local judge)"} {
		if got := metacogLabel(judge); got != want {
			t.Errorf("metacogLabel(%q) = %q", judge, got)
		}
	}
}

func TestWrapLine(t *testing.T) {
	got := wrapLine("  ⎿ the quick brown fox jumps over the lazy dog again", 24)
	want := []string{"  ⎿ the quick brown fox", "    jumps over the lazy", "    dog again"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrapLine = %q", got)
	}
	colored := palette{on: true}.dim("alpha beta gamma delta epsilon zeta eta theta")
	for _, line := range wrapLine(colored, 20) {
		if visibleLen(line) > 20 {
			t.Fatalf("colored line too wide: %q", line)
		}
	}
}
