package agentrunner

import (
	"fmt"
	"strings"

	"encoding/json/v2"

	"github.com/ItIsCuthNotCup/heuristic/harness/inbox"
	"github.com/ItIsCuthNotCup/heuristic/harness/llm"
	"github.com/ItIsCuthNotCup/heuristic/harness/operation"
	"github.com/ItIsCuthNotCup/heuristic/harness/sessionstore"
	"github.com/ItIsCuthNotCup/heuristic/harness/tool"
)

const toolPreviewLines = 4

func (r *renderer) observeUI(item sessionstore.Item) {
	switch item.Kind {
	case sessionstore.ItemInput:
		if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputExternal {
			r.ui.setWorking("Thinking")
		}
	case sessionstore.ItemModelResponse:
		r.modelResponseUI(item.Data.(sessionstore.ModelResponse).Response)
	case sessionstore.ItemToolCallStatus:
		r.toolCallStatusUI(item.Data.(sessionstore.ToolCallStatus))
	}
}

func (r *renderer) modelResponseUI(resp llm.Response) {
	ui, p := r.ui, r.ui.p
	ui.addUsage(resp.Usage)
	var b strings.Builder
	if resp.Failure != nil {
		fmt.Fprintf(&b, "%s %s\n", p.red("✗"), friendlyError(fmt.Errorf("%s", resp.Failure.Message)))
	}
	calling := ""
	for _, item := range resp.Output {
		switch data := item.Data.(type) {
		case llm.Reasoning:
			for i, line := range data.Summary {
				if i >= 3 {
					break
				}
				if line = strings.TrimSpace(line); line != "" {
					fmt.Fprintf(&b, "%s\n", p.dim(p.italic("✻ "+truncateRunes(line, 200))))
				}
			}
		case llm.Message:
			if text := strings.TrimSpace(data.Text); text != "" {
				rendered := strings.Split(renderMarkdown(text, p), "\n")
				for i, line := range rendered {
					prefix := "  "
					if i == 0 {
						prefix = p.accent("●") + " "
					}
					rendered[i] = prefix + line
				}
				b.WriteString(strings.Join(rendered, "\n") + "\n")
			}
		case llm.ToolCall:
			name, arg := toolCallParts(data)
			fmt.Fprintf(&b, "%s %s%s\n", p.green("●"), p.bold(name), p.dim("("+truncateRunes(arg, 160)+")"))
			calling = arg
		}
	}
	if b.Len() > 0 {
		ui.c.Print(b.String())
	}
	if hasToolCall(resp) {
		ui.setWorking("Running " + truncateRunes(firstLine(calling), 50))
		return
	}
	ui.setIdle()
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// toolCallParts returns a card title and argument for a tool call.
func toolCallParts(call llm.ToolCall) (string, string) {
	switch strings.ToLower(call.Name) {
	case strings.ToLower(tool.BashName):
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) == nil && args.Command != "" {
			return "Bash", args.Command
		}
	case strings.ToLower(tool.ViewImageName):
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(call.Arguments), &args) == nil && args.Path != "" {
			return "View image", args.Path
		}
	}
	return call.Name, call.Arguments
}

func (r *renderer) toolCallStatusUI(status sessionstore.ToolCallStatus) {
	ui, p := r.ui, r.ui.p
	if status.Status.Error != "" {
		ui.c.Print(p.dim("  ⎿ ") + p.red(truncateRunes(status.Status.Error, 500)) + "\n")
		ui.setWorking("Thinking")
		return
	}
	if stillRunning(status) {
		return
	}
	var out strings.Builder
	exit := 0
	canceled := len(status.Operations) > 0
	for _, op := range status.Operations {
		if op.Status != operation.StatusCanceled {
			canceled = false
		}
		var state operation.ShellState
		if len(op.State) == 0 || json.Unmarshal(op.State, &state) != nil || state.Result == nil {
			continue
		}
		out.WriteString(state.Result.Out)
		if state.Result.Err != "" {
			out.WriteString(state.Result.Err)
		}
		if state.Result.ExitCode != 0 {
			exit = state.Result.ExitCode
		}
	}
	text := strings.TrimRight(out.String(), "\n")
	ui.mu.Lock()
	ui.lastTool = text
	ui.mu.Unlock()
	if canceled && text == "" {
		ui.setWorking("Thinking")
		return
	}
	var b strings.Builder
	if text == "" {
		b.WriteString(p.dim("  ⎿ (no output)") + "\n")
	} else {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if i == toolPreviewLines {
				fmt.Fprintf(&b, "%s\n", p.dim(fmt.Sprintf("    … +%d lines (ctrl+o to expand)", len(lines)-i)))
				break
			}
			prefix := "    "
			if i == 0 {
				prefix = "  ⎿ "
			}
			fmt.Fprintf(&b, "%s\n", p.dim(prefix+truncateRunes(line, 400)))
		}
	}
	if exit != 0 {
		fmt.Fprintf(&b, "    %s\n", p.red(fmt.Sprintf("exit %d", exit)))
	}
	ui.c.Print(b.String())
	ui.setWorking("Thinking")
}

// stillRunning reports whether a tool call is waiting on operations that
// have not reached a terminal state yet.
func stillRunning(status sessionstore.ToolCallStatus) bool {
	if len(status.Status.WaitingFor) == 0 {
		return false
	}
	if len(status.Operations) < len(status.Status.WaitingFor) {
		return true
	}
	for _, op := range status.Operations {
		switch op.Status {
		case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
		default:
			return true
		}
	}
	return false
}
