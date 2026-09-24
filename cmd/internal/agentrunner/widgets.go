package agentrunner

// Interactive widgets shared by onboarding and the session UI: a searchable
// arrow-key picker, a single-line (optionally masked) input, and a spinner
// for slow checks. lineAsker is the plain-text fallback used when stdin is
// not a terminal (and by tests).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// errBack is returned when the user presses Esc to go back a step.
var errBack = errors.New("back")

// errCancelled is returned when the user presses Ctrl-C inside a widget.
var errCancelled = errors.New("cancelled")

type option struct {
	label  string
	detail string
}

type asker interface {
	// choose returns the index of the picked option.
	choose(title, hint string, options []option, def int) (int, error)
	// input reads one line; secret masks the typed text.
	input(ctx context.Context, label, hint, def string, secret bool) (string, error)
	// note prints a line of text above the widgets.
	note(text string)
	// busy shows a spinner labelled label while fn runs.
	busy(label string, fn func() error) error
	colors() palette
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type ttyAsker struct {
	c *console
	p palette
}

func newTTYAsker(c *console) *ttyAsker { return &ttyAsker{c: c, p: palette{on: c.color}} }

func (a *ttyAsker) colors() palette { return a.p }

func (a *ttyAsker) note(text string) { a.c.Print(text + "\n") }

func (a *ttyAsker) choose(title, hint string, options []option, def int) (int, error) {
	if len(options) == 0 {
		return 0, errors.New("nothing to choose from")
	}
	p := a.p
	selected := def
	if selected < 0 || selected >= len(options) {
		selected = 0
	}
	filter := ""
	searchable := len(options) > 8
	matches := func() []int {
		var out []int
		needle := strings.ToLower(filter)
		for i, opt := range options {
			if needle == "" || strings.Contains(strings.ToLower(opt.label+" "+opt.detail), needle) {
				out = append(out, i)
			}
		}
		return out
	}
	cursor := 0
	for i, index := range matches() {
		if index == selected {
			cursor = i
		}
	}
	view := func(width int) ([]string, int, int) {
		visible := matches()
		lines := []string{"", p.bold(title)}
		for _, line := range wrapWords(hint, width-1) {
			if line != "" {
				lines = append(lines, p.dim(line))
			}
		}
		lines = append(lines, "")
		// Fill the terminal, leaving room for the footer rows below the list.
		window := max(5, a.c.height()-len(lines)-5)
		start := 0
		if cursor >= window {
			start = cursor - window + 1
		}
		labelWidth := 0
		for _, index := range visible {
			labelWidth = max(labelWidth, visibleLen(options[index].label))
		}
		for i := start; i < len(visible) && i < start+window; i++ {
			opt := options[visible[i]]
			pad := strings.Repeat(" ", labelWidth-visibleLen(opt.label)+2)
			line := "   " + opt.label + pad + p.dim(opt.detail)
			if i == cursor {
				line = " " + p.accent("❯") + " " + p.bold(opt.label) + pad + p.dim(opt.detail)
			}
			lines = append(lines, fit(line, width))
		}
		if len(visible) == 0 {
			lines = append(lines, p.dim("   no matches"))
		}
		if len(visible) > window {
			lines = append(lines, p.dim(fmt.Sprintf("   … %d of %d", min(start+window, len(visible)), len(visible))))
		}
		lines = append(lines, "")
		keys := "↑↓ move · enter select · esc back"
		if searchable {
			keys = "type to search · " + keys
		}
		cursorRow, cursorCol := len(lines), 0
		if searchable {
			lines = append(lines, fit(p.dim("search: ")+filter, width))
			cursorRow, cursorCol = len(lines)-1, visibleLen("search: "+filter)
		}
		lines = append(lines, p.dim(keys))
		if !searchable {
			cursorRow = len(lines) - 1
			cursorCol = visibleLen(keys)
		}
		return lines, cursorRow, cursorCol
	}
	a.c.SetView(view)
	defer a.c.SetView(nil)
	for event := range a.c.keys {
		visible := matches()
		switch event.kind {
		case keyUp:
			if cursor > 0 {
				cursor--
			} else if len(visible) > 0 {
				cursor = len(visible) - 1
			}
		case keyDown, keyTab:
			if cursor < len(visible)-1 {
				cursor++
			} else {
				cursor = 0
			}
		case keyEnter:
			if len(visible) > 0 {
				return visible[cursor], nil
			}
		case keyEsc:
			if filter != "" {
				filter, cursor = "", 0
			} else {
				return 0, errBack
			}
		case keyCtrlC:
			return 0, errCancelled
		case keyBackspace:
			if filter != "" {
				filter = string([]rune(filter)[:len([]rune(filter))-1])
				cursor = 0
			}
		case keyRune:
			if searchable {
				filter += string(event.r)
				cursor = 0
			} else if event.r >= '1' && event.r <= '9' && int(event.r-'1') < len(options) {
				return int(event.r - '1'), nil
			}
		case keyPaste:
			if searchable {
				filter += strings.TrimSpace(event.text)
				cursor = 0
			}
		}
		a.c.Redraw()
	}
	return 0, io.EOF
}

func (a *ttyAsker) input(ctx context.Context, label, hint, def string, secret bool) (string, error) {
	p := a.p
	var value []rune
	view := func(width int) ([]string, int, int) {
		lines := []string{"", p.bold(label)}
		for _, line := range wrapWords(hint, width-1) {
			if line != "" {
				lines = append(lines, p.dim(line))
			}
		}
		shown := string(value)
		if secret {
			shown = strings.Repeat("•", len(value))
		}
		prompt := p.accent("❯ ")
		field := shown
		if len(value) == 0 && def != "" {
			field = p.dim(def)
		}
		lines = append(lines, "", fit(prompt+field, width), "")
		footer := "enter confirm · esc back"
		if secret {
			footer = "paste your key (hidden) · " + footer
		}
		lines = append(lines, p.dim(footer))
		col := 2 + visibleLen(shown)
		return lines, len(lines) - 3, min(col, width-1)
	}
	a.c.SetView(view)
	defer a.c.SetView(nil)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case event, ok := <-a.c.keys:
			if !ok {
				return "", io.EOF
			}
			switch event.kind {
			case keyEnter:
				text := strings.TrimSpace(string(value))
				if text == "" {
					text = def
				}
				return text, nil
			case keyEsc:
				return "", errBack
			case keyCtrlC:
				return "", errCancelled
			case keyBackspace:
				if len(value) > 0 {
					value = value[:len(value)-1]
				}
			case keyCtrlU:
				value = value[:0]
			case keyRune:
				value = append(value, event.r)
			case keyPaste:
				value = append(value, []rune(strings.TrimSpace(event.text))...)
			}
			a.c.Redraw()
		}
	}
}

func (a *ttyAsker) busy(label string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	frame := 0
	start := time.Now()
	a.c.SetView(func(width int) ([]string, int, int) {
		line := fmt.Sprintf("%s %s %s", a.p.accent(spinnerFrames[frame%len(spinnerFrames)]), label,
			a.p.dim(fmt.Sprintf("%ds", int(time.Since(start).Seconds()))))
		return []string{"", fit(line, width)}, 1, 0
	})
	defer a.c.SetView(nil)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			frame++
			a.c.Redraw()
		case event, ok := <-a.c.keys:
			if ok && event.kind == keyCtrlC {
				return errCancelled
			}
		}
	}
}

// lineAsker is the non-terminal fallback: numbered choices and plain lines.
type lineAsker struct {
	reader *bufio.Reader
	out    io.Writer
}

func (a *lineAsker) colors() palette { return palette{} }

func (a *lineAsker) note(text string) { fmt.Fprintln(a.out, text) }

func (a *lineAsker) readLine() (string, error) {
	line, err := a.reader.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (a *lineAsker) choose(title, hint string, options []option, def int) (int, error) {
	fmt.Fprintln(a.out, title)
	if hint != "" {
		fmt.Fprintln(a.out, hint)
	}
	for i, opt := range options {
		if opt.detail != "" {
			fmt.Fprintf(a.out, "  %d) %s — %s\n", i+1, opt.label, opt.detail)
		} else {
			fmt.Fprintf(a.out, "  %d) %s\n", i+1, opt.label)
		}
	}
	for {
		fmt.Fprintf(a.out, "Choose [%d]: ", def+1)
		answer, err := a.readLine()
		if err != nil {
			return 0, err
		}
		if answer == "" {
			return def, nil
		}
		if answer == "-" {
			return 0, errBack
		}
		var n int
		if _, err := fmt.Sscanf(answer, "%d", &n); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintln(a.out, "Please enter a number from the list.")
	}
}

func (a *lineAsker) input(_ context.Context, label, hint, def string, _ bool) (string, error) {
	if hint != "" {
		fmt.Fprintln(a.out, hint)
	}
	if def != "" {
		fmt.Fprintf(a.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(a.out, "%s: ", label)
	}
	answer, err := a.readLine()
	if err != nil {
		return "", err
	}
	if answer == "-" {
		return "", errBack
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

func (a *lineAsker) busy(label string, fn func() error) error {
	fmt.Fprintln(a.out, label)
	return fn()
}
