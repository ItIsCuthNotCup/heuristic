package agentrunner

import (
	"context"
	"strings"
	"testing"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
)

func TestPathSummary(t *testing.T) {
	cases := map[string]string{
		"## Plan\nmore":                      "Plan",
		"```go\ncode\n```\n**Use** `x` here": "Use x here",
		"- first   bullet\n- second":         "first bullet",
		"\n\n":                               "(empty)",
	}
	for in, want := range cases {
		if got := pathSummary(in, 40); got != want {
			t.Errorf("pathSummary(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pathSummary(strings.Repeat("word ", 40), 20); len([]rune(got)) != 21 || !strings.HasSuffix(got, "…") {
		t.Errorf("long summary = %q", got)
	}
}

func pathEvent() metacog.Event {
	return metacog.Event{
		GreedyScore: 0.4, Branches: 2, Chosen: 1, JudgeCalls: 4, Final: true,
		Scores: []float64{0.4, 0.8, 0.3},
		Paths:  []string{"Use a map.", "Use a slice.", "Use a channel."},
	}
}

func TestMetaCogEventListsThoughtPaths(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	ui.metacogEvent(pathEvent())
	out := read()
	for _, want := range []string{"Thought Path 1  0.40  Use a map.", "❯ Thought Path 2  0.80  Use a slice.", "Thought Path 3  0.30  Use a channel.", "ctrl+t"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	tool := pathEvent()
	tool.Final = false
	c2, read2 := testConsole(t)
	ui2 := newTUI(c2)
	ui2.metacogEvent(tool)
	if strings.Contains(read2(), "Thought Path") || ui2.paths != nil {
		t.Fatal("listed paths for a tool-call turn")
	}
}

func TestExplorePathsSwitchesPath(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	ui.metacogEvent(pathEvent())
	// Open Thought Path 3, then "Continue from this path".
	for _, k := range []keyEvent{{kind: keyDown}, {kind: keyEnter}, {kind: keyEnter}} {
		c.keys <- k
	}
	var from, to string
	ui.explorePaths(context.Background(), func(original, replacement string) { from, to = original, replacement })
	if from != "Use a slice." || to != "Use a channel." {
		t.Fatalf("use(%q, %q)", from, to)
	}
	if ui.paths.inUse != 2 || ui.paths.picked != 1 {
		t.Fatalf("paths = %+v", ui.paths)
	}
	if out := read(); !strings.Contains(out, "Continuing from Thought Path 3") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestBackgroundCheckShowsBetterAnswer(t *testing.T) {
	c, read := testConsole(t)
	ui := newTUI(c)
	event := pathEvent()
	event.Background = true
	ui.metacogEvent(event)
	out := read()
	for _, want := range []string{"found a better answer: Thought Path 2", "❯ Thought Path 2", "your next message continues from Thought Path 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if got := ui.takeSwitches()["Use a map."]; got != "Use a slice." {
		t.Fatalf("switch = %q", got)
	}
	// Going back to the first answer swaps from the transcript's text.
	for _, k := range []keyEvent{{kind: keyUp}, {kind: keyEnter}, {kind: keyEnter}} {
		c.keys <- k
	}
	var from, to string
	ui.explorePaths(context.Background(), func(original, replacement string) { from, to = original, replacement })
	if from != "Use a map." || to != "Use a map." {
		t.Fatalf("use(%q, %q)", from, to)
	}
}

func TestMetaCogStageShowsInFooter(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	ui.st.metacog = "MetaCog on"
	ui.setMetacogStage("trying 3 more thought paths")
	if got := ui.footerLocked(120); !strings.Contains(got, "MetaCog is trying 3 more thought paths…") {
		t.Fatalf("footer = %q", got)
	}
	ui.setMetacogStage("")
	if got := ui.footerLocked(120); !strings.Contains(got, "MetaCog on") {
		t.Fatalf("footer = %q", got)
	}
}

func TestExplorePathsBackKeepsPath(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	ui.metacogEvent(pathEvent())
	// Open the path in use (no switch offered), then leave the list.
	for _, k := range []keyEvent{{kind: keyEnter}, {kind: keyEsc}} {
		c.keys <- k
	}
	ui.explorePaths(context.Background(), func(string, string) { t.Fatal("switched path") })
	if ui.paths.inUse != 1 {
		t.Fatalf("in use = %d", ui.paths.inUse)
	}
}

func TestExplorePathsWithoutPaths(t *testing.T) {
	c, _ := testConsole(t)
	ui := newTUI(c)
	ui.explorePaths(context.Background(), func(string, string) { t.Fatal("switched path") })
	if !strings.Contains(ui.notice, "No thought paths yet") {
		t.Fatalf("notice = %q", ui.notice)
	}
}

func TestLiveRowsAboveAfterResize(t *testing.T) {
	if got := liveRowsAbove([]int{80, 80, 10}, 5, 80); got != 3 {
		t.Fatalf("same width: %d", got)
	}
	if got := liveRowsAbove([]int{80, 80, 10}, 5, 50); got != 5 {
		t.Fatalf("narrower: %d", got)
	}
	var d keyDecoder
	if events := d.decode([]byte{0x14}); len(events) != 1 || events[0].kind != keyCtrlT {
		t.Fatalf("ctrl+t decoded as %+v", events)
	}
}
