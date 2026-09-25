package agentrunner

import (
	"fmt"
	"io"
	"os"
	"strings"

	"encoding/json/v2"

	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm/metacog"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/session"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

// renderer renders session items as human-readable terminal output for the
// interactive runner. ANSI colors are used only when the destination is a
// character device and NO_COLOR is unset.
type renderer struct {
	out        io.Writer
	color      bool
	showPrompt bool
	thinking   bool
	// ui switches to the full terminal UI: cards, Markdown, live status.
	ui *tui
}

func newRenderer(out io.Writer, getenv func(string) string, showPrompt bool) *renderer {
	color := false
	if file, ok := out.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			color = getenv("NO_COLOR") == ""
		}
	}
	return &renderer{out: out, color: color, showPrompt: showPrompt}
}

func (r *renderer) dim(text string) string {
	if r.color {
		return "\x1b[2m" + text + "\x1b[0m"
	}
	return text
}

func (r *renderer) printf(format string, args ...any) {
	fmt.Fprintf(r.out, format, args...)
}

// Observe renders one persisted session item.
func (r *renderer) Observe(_ session.ID, item sessionstore.Item) {
	if r.ui != nil {
		r.observeUI(item)
		return
	}
	switch item.Kind {
	case sessionstore.ItemModelResponse:
		r.modelResponse(item.Data.(sessionstore.ModelResponse).Response)
	case sessionstore.ItemToolCallStatus:
		r.toolCallStatus(item.Data.(sessionstore.ToolCallStatus))
	}
	// Input, turn, and fork items are not rendered: inputs are what the user
	// just typed.
}

func (r *renderer) modelResponse(resp llm.Response) {
	if resp.Failure != nil {
		r.printf("error: %s\n", resp.Failure.Message)
	}
	r.thinking = false
	for _, item := range resp.Output {
		switch data := item.Data.(type) {
		case llm.Reasoning:
			if !r.thinking {
				r.printf("%s\n", r.dim("thinking…"))
				r.thinking = true
			}
			for _, line := range data.Summary {
				r.printf("%s\n", r.dim("  "+line))
			}
		case llm.Message:
			if text := strings.TrimSpace(data.Text); text != "" {
				r.printf("%s\n", text)
			}
		case llm.ToolCall:
			r.printf("⚙ %s\n", toolCallLine(data))
		}
	}
	// The assistant finished when the response carries no tool call.
	if r.showPrompt && !hasToolCall(resp) {
		r.printf("\n› ")
	}
}

func hasToolCall(resp llm.Response) bool {
	for _, item := range resp.Output {
		if item.Type == llm.ItemToolCall {
			return true
		}
	}
	return false
}

func toolCallLine(call llm.ToolCall) string {
	name := strings.ToLower(call.Name)
	switch name {
	case strings.ToLower(tool.BashName):
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err == nil && args.Command != "" {
			return "bash: " + args.Command
		}
	case strings.ToLower(tool.ViewImageName):
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err == nil && args.Path != "" {
			return "view_image: " + args.Path
		}
	}
	return name + " " + truncateRunes(call.Arguments, 200)
}

func (r *renderer) toolCallStatus(status sessionstore.ToolCallStatus) {
	if status.Status.Error != "" {
		r.printf("✗ %s\n", truncateRunes(status.Status.Error, 500))
		return
	}
	if stillRunning(status) {
		return
	}
	rendered := false
	for _, op := range status.Operations {
		var state operation.ShellState
		if len(op.State) == 0 || json.Unmarshal(op.State, &state) != nil || state.Result == nil {
			continue
		}
		if text := strings.TrimRight(state.Result.Out, "\n"); text != "" {
			lines := strings.Split(text, "\n")
			if len(lines) > 12 {
				lines = lines[len(lines)-12:]
			}
			for _, line := range lines {
				r.printf("%s\n", r.dim("  "+line))
			}
			rendered = true
		}
	}
	if !rendered {
		r.printf("✓ done\n")
	}
}

// Event renders one metacog trace event.
func (r *renderer) Event(event metacog.Event) {
	if r.ui != nil {
		r.ui.metacogEvent(event)
		return
	}
	if event.Stopped {
		if event.Skipped {
			r.printf("%s\n", r.dim(fmt.Sprintf(
				"◆ metacog: simple request, answered directly (%.1fs)",
				float64(event.DurationMs)/1000,
			)))
			return
		}
		r.printf("%s\n", r.dim(fmt.Sprintf(
			"◆ metacog: confident %.2f, no branching (%d judge call(s), %.1fs)",
			event.GreedyScore, event.JudgeCalls, float64(event.DurationMs)/1000,
		)))
		return
	}
	if agree := countTrue(event.Agree); agree > 0 {
		r.printf("%s\n", r.dim(fmt.Sprintf(
			"◆ metacog: %d of %d thought paths agree → picked #%d, %d judge calls, %.1fs",
			agree, len(event.Paths), event.Chosen, event.JudgeCalls, float64(event.DurationMs)/1000,
		)))
		return
	}
	score := 0.0
	if event.Chosen < len(event.Scores) {
		score = event.Scores[event.Chosen]
	}
	r.printf("%s\n", r.dim(fmt.Sprintf(
		"◆ metacog: greedy %.2f → %d more thought paths → picked #%d (score %.2f), %d judge calls, %.1fs",
		event.GreedyScore, event.Branches, event.Chosen, score, event.JudgeCalls, float64(event.DurationMs)/1000,
	)))
}

func truncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}
