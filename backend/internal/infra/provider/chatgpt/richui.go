package chatgpt

import (
	"regexp"
	"strings"
)

// ChatGPT Web answers may embed generative UI markup (JSX-like <box>, <row>,
// <title>, <AsyncImage/>, <Cite/> …) that only the Web app renders. API
// clients get plain Markdown: containers are unwrapped, decorative
// components are dropped and their content is dedented. Code blocks and
// inline code are kept, and a lone container tag in prose stays text.
var (
	richUIContainer   = `(?:box|row|column|col|stack|grid|card|list|item)(?:-[a-z]+)*`
	richUIText        = `title|text|badge|chip|label|caption`
	richUIOpenTag     = regexp.MustCompile(`<(` + richUIContainer + `)(\s[^<>]*)?>`)
	richUICloseTag    = regexp.MustCompile(`</(` + richUIContainer + `)\s*>`)
	richUITextTag     = regexp.MustCompile(`</?(` + richUIText + `)(\s[^<>]*)?>`)
	richUISelfClosing = regexp.MustCompile(`<(icon|divider|spacer|image|img|[A-Z][A-Za-z0-9]*)(\s[^<>]*)?/>`)
)

// richUIFilter rewrites streamed answer text. Lines that may hold markup are
// held back until complete; other text passes through immediately.
type richUIFilter struct {
	pending string
	depth   int
	fence   bool
	midLine bool
}

// Feed returns the client-visible text for the next chunk of the answer.
func (f *richUIFilter) Feed(chunk string) string {
	f.pending += chunk
	var out strings.Builder
	for {
		index := strings.IndexByte(f.pending, '\n')
		if index < 0 {
			break
		}
		line := f.pending[:index]
		f.pending = f.pending[index+1:]
		if text, keep := f.line(line); keep {
			out.WriteString(text)
			out.WriteByte('\n')
		}
		f.midLine = false
	}
	if f.pending != "" && !f.mayHoldMarkup(f.pending) {
		out.WriteString(f.pending)
		f.pending, f.midLine = "", true
	}
	return out.String()
}

// Flush returns the held-back remainder at the end of the answer.
func (f *richUIFilter) Flush() string {
	line := f.pending
	f.pending = ""
	if line == "" {
		return ""
	}
	text, _ := f.line(line)
	return text
}

// mayHoldMarkup holds text that may hold markup or an inline code span, so
// the line is rewritten whole.
func (f *richUIFilter) mayHoldMarkup(text string) bool {
	if f.fence {
		return strings.HasPrefix(strings.TrimSpace(text), "`")
	}
	return f.depth > 0 || strings.ContainsAny(text, "<`")
}

// line rewrites one complete line; keep is false for lines that held only markup.
func (f *richUIFilter) line(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !f.midLine && strings.HasPrefix(trimmed, "```") {
		f.fence = !f.fence
		return line, true
	}
	if f.fence {
		return line, true
	}
	inside := f.depth > 0
	prose := mapOutsideCode(line, func(s string) string { return s }, func(string) string { return "" })
	opens := len(richUIOpenTag.FindAllStringIndex(prose, -1))
	closes := len(richUICloseTag.FindAllStringIndex(prose, -1))
	markupOnly := opens+closes > 0 && strings.TrimSpace(stripRichUI(prose, true)) == ""
	// Containers open and close only on markup lines; on a prose line they
	// are unwrapped when balanced ("use the <list> tag" stays as written).
	containers := markupOnly || opens == closes
	text := mapOutsideCode(line, func(s string) string { return stripRichUI(s, containers) }, func(code string) string { return code })
	hadMarkup := text != line
	if markupOnly {
		f.depth = max(0, f.depth+opens-closes)
	}
	if hadMarkup && strings.TrimSpace(text) == "" {
		return "", false
	}
	if (inside || hadMarkup) && !f.midLine {
		text = strings.TrimLeft(text, " \t")
	}
	if inside && strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// stripRichUI drops text tags and self-closing components, and container tags
// when containers is set.
func stripRichUI(text string, containers bool) string {
	if containers {
		text = richUIOpenTag.ReplaceAllString(text, "")
		text = richUICloseTag.ReplaceAllString(text, "")
	}
	text = richUITextTag.ReplaceAllString(text, "")
	return richUISelfClosing.ReplaceAllString(text, "")
}

// mapOutsideCode rewrites a line with prose for the text outside inline code
// spans and code for each span, backticks included. An unmatched backtick run
// is prose.
func mapOutsideCode(line string, prose, code func(string) string) string {
	var out strings.Builder
	for line != "" {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			out.WriteString(prose(line))
			break
		}
		run := start
		for run < len(line) && line[run] == '`' {
			run++
		}
		ticks := line[start:run]
		end := closingTicks(line[run:], len(ticks))
		if end < 0 {
			out.WriteString(prose(line[:run]))
			line = line[run:]
			continue
		}
		out.WriteString(prose(line[:start]))
		out.WriteString(code(line[start : run+end+len(ticks)]))
		line = line[run+end+len(ticks):]
	}
	return out.String()
}

// closingTicks finds a backtick run of exactly n in text, or -1.
func closingTicks(text string, n int) int {
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == '`' {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	return -1
}
