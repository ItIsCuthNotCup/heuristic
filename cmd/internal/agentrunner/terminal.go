package agentrunner

// Raw-mode terminal primitives for heu: key decoding (arrows, bracketed
// paste, Esc vs escape sequences) and a "live region" console that keeps a
// redrawable block (spinner, editor, menus, footer) pinned below normal
// scrollback output, the way Claude Code and pi's regular mode behave.

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyNewline
	keyBackspace
	keyDelete
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyEsc
	keyTab
	keyCtrlC
	keyCtrlD
	keyCtrlL
	keyCtrlO
	keyCtrlU
	keyCtrlW
	keyPaste
	keyResize
)

type keyEvent struct {
	kind keyKind
	r    rune
	text string
}

// decodeKeys turns one read chunk into key events. pasting carries
// bracketed-paste state across chunks; pasteBuf accumulates pasted text.
type keyDecoder struct {
	pasting  bool
	pasteBuf strings.Builder
}

func (d *keyDecoder) decode(chunk []byte) []keyEvent {
	var events []keyEvent
	s := string(chunk)
	for len(s) > 0 {
		if d.pasting {
			end := strings.Index(s, "\x1b[201~")
			if end < 0 {
				d.pasteBuf.WriteString(s)
				return events
			}
			d.pasteBuf.WriteString(s[:end])
			text := strings.ReplaceAll(d.pasteBuf.String(), "\r\n", "\n")
			text = strings.ReplaceAll(text, "\r", "\n")
			events = append(events, keyEvent{kind: keyPaste, text: text})
			d.pasteBuf.Reset()
			d.pasting = false
			s = s[end+len("\x1b[201~"):]
			continue
		}
		if strings.HasPrefix(s, "\x1b[200~") {
			d.pasting = true
			s = s[len("\x1b[200~"):]
			continue
		}
		if s[0] == 0x1b {
			if len(s) == 1 {
				events = append(events, keyEvent{kind: keyEsc})
				return events
			}
			if s[1] == '\r' || s[1] == '\n' {
				events = append(events, keyEvent{kind: keyNewline})
				s = s[2:]
				continue
			}
			if s[1] == '[' || s[1] == 'O' {
				// CSI / SS3: parameters then one final byte in 0x40–0x7e.
				i := 2
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
				if i >= len(s) {
					return events
				}
				seq := s[2:i]
				switch s[i] {
				case 'A':
					events = append(events, keyEvent{kind: keyUp})
				case 'B':
					events = append(events, keyEvent{kind: keyDown})
				case 'C':
					events = append(events, keyEvent{kind: keyRight})
				case 'D':
					events = append(events, keyEvent{kind: keyLeft})
				case 'H':
					events = append(events, keyEvent{kind: keyHome})
				case 'F':
					events = append(events, keyEvent{kind: keyEnd})
				case 'u':
					// kitty/CSI-u: 13;2u is Shift+Enter.
					if strings.HasPrefix(seq, "13;") {
						events = append(events, keyEvent{kind: keyNewline})
					}
				case '~':
					switch seq {
					case "1", "7":
						events = append(events, keyEvent{kind: keyHome})
					case "4", "8":
						events = append(events, keyEvent{kind: keyEnd})
					case "3":
						events = append(events, keyEvent{kind: keyDelete})
					case "13;2":
						events = append(events, keyEvent{kind: keyNewline})
					}
				}
				s = s[i+1:]
				continue
			}
			// Alt+key: treat as the key itself.
			s = s[1:]
			continue
		}
		b := s[0]
		switch {
		case b == '\r':
			events = append(events, keyEvent{kind: keyEnter})
		case b == '\n':
			events = append(events, keyEvent{kind: keyNewline})
		case b == 0x7f || b == 0x08:
			events = append(events, keyEvent{kind: keyBackspace})
		case b == '\t':
			events = append(events, keyEvent{kind: keyTab})
		case b == 0x03:
			events = append(events, keyEvent{kind: keyCtrlC})
		case b == 0x04:
			events = append(events, keyEvent{kind: keyCtrlD})
		case b == 0x01:
			events = append(events, keyEvent{kind: keyHome})
		case b == 0x05:
			events = append(events, keyEvent{kind: keyEnd})
		case b == 0x0c:
			events = append(events, keyEvent{kind: keyCtrlL})
		case b == 0x0f:
			events = append(events, keyEvent{kind: keyCtrlO})
		case b == 0x15:
			events = append(events, keyEvent{kind: keyCtrlU})
		case b == 0x17:
			events = append(events, keyEvent{kind: keyCtrlW})
		case b == 0x10:
			events = append(events, keyEvent{kind: keyUp})
		case b == 0x0e:
			events = append(events, keyEvent{kind: keyDown})
		case b < 0x20:
			// other control bytes ignored
		default:
			r, size := utf8.DecodeRuneInString(s)
			if r == utf8.RuneError && size <= 1 && !utf8.FullRuneInString(s) {
				return events
			}
			events = append(events, keyEvent{kind: keyRune, r: r})
			s = s[size:]
			continue
		}
		s = s[1:]
	}
	return events
}

// console owns a raw-mode terminal. Output written through Print lands in
// normal scrollback above the live region, which is re-rendered by view.
type console struct {
	in    *os.File
	out   *os.File
	fd    int
	state *term.State
	color bool

	keys chan keyEvent

	mu        sync.Mutex
	view      func(width int) (lines []string, cursorRow, cursorCol int)
	cursorRow int
	closed    bool
	stopSig   func()
}

func openConsole(in, out *os.File, getenv func(string) string) (*console, error) {
	fd := int(in.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	c := &console{
		in: in, out: out, fd: fd, state: state,
		color: getenv("NO_COLOR") == "" && getenv("TERM") != "dumb",
		keys:  make(chan keyEvent, 64),
	}
	io.WriteString(out, "\x1b[?2004h")
	go c.readKeys()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	done := make(chan struct{})
	c.stopSig = func() {
		signal.Stop(resize)
		close(done)
	}
	go func() {
		for {
			select {
			case <-resize:
				c.keys <- keyEvent{kind: keyResize}
			case <-done:
				return
			}
		}
	}()
	return c, nil
}

func (c *console) readKeys() {
	var decoder keyDecoder
	buf := make([]byte, 4096)
	for {
		n, err := c.in.Read(buf)
		if n > 0 {
			for _, event := range decoder.decode(buf[:n]) {
				c.keys <- event
			}
		}
		if err != nil {
			close(c.keys)
			return
		}
	}
}

// Close clears the live region and restores the terminal.
func (c *console) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.clearLocked()
	io.WriteString(c.out, "\x1b[?2004l\x1b[?25h")
	c.stopSig()
	_ = term.Restore(c.fd, c.state)
}

func (c *console) width() int {
	w, _, err := term.GetSize(int(c.out.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func (c *console) SetView(view func(width int) ([]string, int, int)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	c.view = view
	c.drawLocked()
}

func (c *console) Redraw() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	c.drawLocked()
}

// Print writes text into scrollback above the live region.
func (c *console) Print(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		io.WriteString(c.out, text)
		return
	}
	c.clearLocked()
	width := c.width()
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(wrapLine(line, width), "\r\n")
	}
	io.WriteString(c.out, strings.Join(lines, "\r\n"))
	c.drawLocked()
}

func (c *console) clearLocked() {
	if c.cursorRow > 0 {
		io.WriteString(c.out, "\x1b["+itoa(c.cursorRow)+"A")
	}
	io.WriteString(c.out, "\r\x1b[J")
	c.cursorRow = 0
}

func (c *console) drawLocked() {
	if c.view == nil || c.closed {
		return
	}
	width := c.width()
	lines, row, col := c.view(width)
	if len(lines) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(fit(line, width))
	}
	// Cursor is now at the end of the last line; move to (row, col).
	if up := len(lines) - 1 - row; up > 0 {
		b.WriteString("\x1b[" + itoa(up) + "A")
	}
	b.WriteString("\r")
	if col > 0 {
		b.WriteString("\x1b[" + itoa(col) + "C")
	}
	b.WriteString("\x1b[?25h")
	io.WriteString(c.out, b.String())
	c.cursorRow = row
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// style helpers; all no-ops without color.
type palette struct{ on bool }

func (p palette) wrap(code, text string) string {
	if !p.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
func (p palette) bold(s string) string   { return p.wrap("1", s) }
func (p palette) dim(s string) string    { return p.wrap("2", s) }
func (p palette) italic(s string) string { return p.wrap("3", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }
func (p palette) cyan(s string) string   { return p.wrap("36", s) }
func (p palette) accent(s string) string { return p.wrap("38;5;208", s) }
func (p palette) inverse(s string) string {
	return p.wrap("7", s)
}

// visibleLen is the printed width of s ignoring ANSI escapes. Wide runes are
// counted as two cells.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				j++
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		n += runeWidth(r)
		i += size
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe30 && r <= 0xfe4f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x3fffd)):
		return 2
	}
	return 1
}

// fit truncates s (which may contain ANSI escapes) to width cells.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if visibleLen(s) <= width {
		return s
	}
	var b strings.Builder
	n := 0
	styled := strings.Contains(s, "\x1b")
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				j++
			}
			if j > len(s) {
				j = len(s)
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w := runeWidth(r)
		if n+w > width-1 {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n += w
		i += size
	}
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// wrapWords word-wraps plain text to width, keeping explicit line breaks.
func wrapWords(text string, width int) []string {
	width = max(width, 10)
	var out []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case visibleLen(line)+1+visibleLen(word) > width:
				out = append(out, line)
				line = word
			default:
				line += " " + word
			}
		}
		out = append(out, line)
	}
	return out
}

// wrapLine word-wraps one line (which may contain ANSI escapes) to width,
// indenting continuation lines to match the line's leading spaces. Words
// wider than the line are left for the terminal to break.
func wrapLine(line string, width int) []string {
	if width < 20 || visibleLen(line) <= width {
		return []string{line}
	}
	plain := stripANSI(line)
	rest := strings.TrimLeft(plain, " ")
	indent := strings.Repeat(" ", len(plain)-len(rest))
	if r, size := utf8.DecodeRuneInString(rest); size > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && strings.HasPrefix(rest[size:], " ") {
		indent += "  "
	}
	if len(indent) > width/2 {
		indent = ""
	}
	var out []string
	current, currentWidth := "", 0
	for i, word := range strings.Split(line, " ") {
		w := visibleLen(word)
		if i > 0 && currentWidth+1+w > width && strings.TrimSpace(stripANSI(current)) != "" {
			out = append(out, current)
			current, currentWidth = indent+word, len(indent)+w
			continue
		}
		if i > 0 {
			current += " "
			currentWidth++
		}
		current += word
		currentWidth += w
	}
	return append(out, current)
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
