package agentrunner

// Thought paths: when MetaCog branches, heu lists every path it tried with a
// one-line summary and the judge's score, and lets the user open any path in
// full and continue the conversation from it instead of the judge's pick.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
)

// thoughtPaths is the most recent MetaCog decision that tried more than one
// path.
type thoughtPaths struct {
	texts  []string
	scores []float64 // the judge's scores; empty when a vote decided
	agree  []bool    // paths that reached the chosen answer, after a vote
	picked int       // the judge's pick
	inUse  int       // the path the conversation continues from
	shown  int       // the path whose text is in the transcript
	tree   bool      // approach sketches, read-only: nothing to continue from
}

var mdNoise = regexp.MustCompile("[*_`#>]+")

// pathSummary is the first line of prose in a thought path, without
// Markdown markup, cut to width runes.
func pathSummary(text string, width int) string {
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		trimmed = strings.TrimSpace(mdNoise.ReplaceAllString(trimmed, ""))
		trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "-+|"))
		if trimmed != "" {
			return truncateRunes(strings.Join(strings.Fields(trimmed), " "), max(width, 10))
		}
	}
	return "(empty)"
}

func (t *tui) recordPaths(event metacog.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if event.Tree {
		// Sketch-tree events carry the level-1 approach sketches; they can
		// be opened and read but the conversation cannot continue from a
		// sketch, so they never replace t.paths' switchable set.
		if len(event.Paths) < 2 || len(event.Paths) != len(event.Scores) {
			return
		}
		t.paths = &thoughtPaths{texts: event.Paths, scores: event.Scores, picked: event.Chosen, inUse: event.Chosen, shown: event.Chosen, tree: true}
		return
	}
	if !event.Final || len(event.Paths) < 2 || (len(event.Paths) != len(event.Scores) && len(event.Paths) != len(event.Agree)) {
		return
	}
	shown := event.Chosen
	if event.Background {
		shown = 0
		if event.Chosen != 0 {
			if t.switched == nil {
				t.switched = map[string]string{}
			}
			t.switched[event.Paths[0]] = event.Paths[event.Chosen]
		}
	}
	t.paths = &thoughtPaths{texts: event.Paths, scores: event.Scores, agree: event.Agree, picked: event.Chosen, inUse: event.Chosen, shown: shown}
}

// pathList renders the compact list printed under a MetaCog line.
func (t *tui) pathList(paths *thoughtPaths, width int) string {
	p := t.p
	var b strings.Builder
	for i, text := range paths.texts {
		label := fmt.Sprintf("Thought Path %d", i+1)
		score := paths.mark(i)
		summary := pathSummary(text, width-len(label)-14)
		if i == paths.inUse {
			fmt.Fprintf(&b, "%s %s  %s  %s\n", p.accent("❯"), p.bold(label), score, summary)
		} else {
			fmt.Fprintf(&b, "  %s  %s  %s\n", p.dim(label), p.dim(score), p.dim(summary))
		}
	}
	if paths.tree {
		b.WriteString(p.dim("  ctrl+t to open a path") + "\n")
	} else {
		b.WriteString(p.dim("  ctrl+t to open a path or continue from a different one") + "\n")
	}
	return b.String()
}

// explorePaths lets the user read any thought path in full and switch the
// conversation onto it. use applies the switch to the running session.
func (t *tui) explorePaths(ctx context.Context, use func(original, replacement string)) {
	p := t.p
	t.mu.Lock()
	paths := t.paths
	t.mu.Unlock()
	if paths == nil {
		t.setNotice("No thought paths yet — MetaCog lists them when it isn't sure of an answer")
		return
	}
	a := newTTYAsker(t.c)
	defer t.c.SetView(t.view)
	selected := paths.inUse
	for ctx.Err() == nil {
		options := make([]option, len(paths.texts))
		for i, text := range paths.texts {
			detail := paths.mark(i)
			switch {
			case i == paths.picked && paths.tree:
				detail += " · expanded"
			case i == paths.inUse && i == paths.picked:
				detail += " · picked, in use"
			case i == paths.inUse:
				detail += " · in use"
			case i == paths.picked && len(paths.scores) > 0:
				detail += " · judge's pick"
			case i == paths.picked:
				detail += " · picked"
			}
			options[i] = option{label: fmt.Sprintf("Thought Path %d", i+1), detail: detail + " · " + pathSummary(text, 60)}
		}
		hint := "Enter opens a path. The score is how likely the judge thinks it's right."
		if paths.tree {
			hint = "Enter opens a path. The score is how promising the judge thought the idea was."
		} else if len(paths.scores) == 0 {
			hint = "Enter opens a path. MetaCog kept the answer that two paths agreed on."
		}
		index, err := a.choose("Thought paths", hint, options, selected)
		if err != nil {
			return
		}
		selected = index
		label := fmt.Sprintf("Thought Path %d", index+1)
		t.c.Print("\n" + p.accent("◆ ") + p.bold(label) + p.dim(" · "+paths.mark(index)) + "\n" +
			renderMarkdown(strings.TrimSpace(paths.texts[index]), p) + "\n")
		if index == paths.inUse || paths.tree {
			continue
		}
		choice, err := a.choose(label, "", []option{
			{label: "Continue from this path", detail: "your next message builds on it"},
			{label: "Back to the list"},
		}, 0)
		if err != nil || choice != 0 {
			continue
		}
		use(paths.texts[paths.shown], paths.texts[index])
		t.mu.Lock()
		paths.inUse = index
		t.mu.Unlock()
		t.c.Print(p.green("✓") + " Continuing from " + label + "\n")
		return
	}
}

// mark is the short label next to a path: its judge score, or whether it
// agreed with the chosen answer when a vote decided.
func (paths *thoughtPaths) mark(i int) string {
	switch {
	case i < len(paths.scores):
		return fmt.Sprintf("%.2f", paths.scores[i])
	case i < len(paths.agree) && paths.agree[i]:
		return "agrees"
	}
	return "differs"
}

func countTrue(values []bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}
