package metacog

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ItIsCuthNotCup/MetaCog-Agent/harness/llm"
)

// BuildProblem renders the request input for the judge: system messages
// first, then user messages, then a tail of tool results. The result is
// head/tail truncated to maxChars.
func BuildProblem(req llm.Request, maxChars int) string {
	var systems, users, results []string
	for _, item := range req.Input {
		switch data := item.Data.(type) {
		case llm.Message:
			switch data.Role {
			case llm.RoleSystem:
				systems = append(systems, data.Text)
			case llm.RoleUser:
				users = append(users, data.Text)
			}
		case llm.ToolResult:
			for _, out := range data.Output {
				if out.Kind == llm.ToolResultText {
					results = append(results, out.Value)
				}
			}
		}
	}
	var b strings.Builder
	for _, text := range systems {
		b.WriteString("system:\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	for _, text := range users {
		b.WriteString("user:\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	for _, text := range results {
		b.WriteString("tool result:\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	return headTailTruncate(strings.TrimSpace(b.String()), maxChars)
}

// headTailTruncate keeps the first and last maxChars/2 runes with an ellipsis
// between them when text exceeds maxChars.
func headTailTruncate(text string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	runes := []rune(text)
	half := maxChars / 2
	return string(runes[:half]) + "\n…\n" + string(runes[len(runes)-half:])
}

var (
	boxedPattern  = regexp.MustCompile(`\\boxed\{([^{}]*)\}`)
	answerPattern = regexp.MustCompile(`(?im)final answer\s*[:\-]\s*(.+)$`)
)

// ExtractAnswer finds a candidate's bare final answer for the answer-prior
// readout: the last \boxed{...} or the last "final answer: ..." marker, else
// the last non-empty line. "" means none found.
func ExtractAnswer(text string) string {
	var answer string
	if m := boxedPattern.FindAllStringSubmatch(text, -1); len(m) > 0 {
		answer = m[len(m)-1][1]
	} else if m := answerPattern.FindAllStringSubmatch(text, -1); len(m) > 0 {
		answer = m[len(m)-1][1]
	} else {
		for line := range strings.Lines(text) {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				answer = trimmed
			}
		}
	}
	answer = strings.TrimSpace(strings.Trim(strings.TrimSpace(answer), "$"))
	// Overly long "answers" are still returned; the judge treats them like
	// any other stated final answer.
	return answer
}
