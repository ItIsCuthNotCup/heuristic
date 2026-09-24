package agentrunner

// A small terminal Markdown renderer for assistant replies: headings,
// emphasis, inline code, fenced code blocks, lists, quotes and links. It is
// line-based and never fails; unknown syntax passes through unchanged.

import (
	"regexp"
	"strings"
)

var (
	mdBold       = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	mdItalic     = regexp.MustCompile(`(^|[^*\w])\*([^*\s][^*\n]*?)\*([^*\w]|$)`)
	mdCode       = regexp.MustCompile("`([^`\n]+)`")
	mdLink       = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	mdHeading    = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdBullet     = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdNumbered   = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	mdRule       = regexp.MustCompile(`^\s*([-*_])(\s*[-*_]){2,}\s*$`)
	mdTableSplit = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
)

func renderMarkdown(text string, p palette) string {
	var out []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			if inFence {
				lang := strings.TrimLeft(trimmed, "`~")
				if lang != "" {
					out = append(out, p.dim("  "+lang))
				}
			}
			continue
		}
		if inFence {
			out = append(out, p.dim("│ ")+p.cyan(line))
			continue
		}
		switch {
		case mdHeading.MatchString(line):
			m := mdHeading.FindStringSubmatch(line)
			out = append(out, p.bold(inlineMarkdown(m[2], p)))
		case mdRule.MatchString(line):
			out = append(out, p.dim(strings.Repeat("─", 40)))
		case mdTableSplit.MatchString(line) && strings.Contains(line, "-"):
			out = append(out, p.dim(strings.Repeat("─", min(visibleLen(line), 60))))
		case mdBullet.MatchString(line):
			m := mdBullet.FindStringSubmatch(line)
			out = append(out, m[1]+p.dim("•")+" "+inlineMarkdown(m[2], p))
		case mdNumbered.MatchString(line):
			m := mdNumbered.FindStringSubmatch(line)
			out = append(out, m[1]+p.dim(m[2]+".")+" "+inlineMarkdown(m[3], p))
		case strings.HasPrefix(trimmed, ">"):
			out = append(out, p.dim("▎ ")+p.italic(inlineMarkdown(strings.TrimSpace(strings.TrimPrefix(trimmed, ">")), p)))
		default:
			out = append(out, inlineMarkdown(line, p))
		}
	}
	return strings.Join(out, "\n")
}

func inlineMarkdown(line string, p palette) string {
	// Protect inline code from the emphasis rules.
	var codes []string
	line = mdCode.ReplaceAllStringFunc(line, func(match string) string {
		codes = append(codes, mdCode.FindStringSubmatch(match)[1])
		return "\x00" + itoa(len(codes)-1) + "\x00"
	})
	line = mdLink.ReplaceAllStringFunc(line, func(match string) string {
		m := mdLink.FindStringSubmatch(match)
		if m[1] == m[2] {
			return p.cyan(m[2])
		}
		return m[1] + " " + p.dim("("+m[2]+")")
	})
	line = mdBold.ReplaceAllStringFunc(line, func(match string) string {
		m := mdBold.FindStringSubmatch(match)
		return p.bold(m[1] + m[2])
	})
	line = mdItalic.ReplaceAllStringFunc(line, func(match string) string {
		m := mdItalic.FindStringSubmatch(match)
		return m[1] + p.italic(m[2]) + m[3]
	})
	for i, code := range codes {
		line = strings.Replace(line, "\x00"+itoa(i)+"\x00", p.cyan(code), 1)
	}
	return line
}
