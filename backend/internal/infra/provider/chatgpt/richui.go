package chatgpt

import (
	"regexp"
	"strings"
)

// ChatGPT Web answers may embed generative UI markup (JSX-like <box>, <row>,
// <title>, <AsyncImage/>, <Cite/> …) that only the Web app renders. API
// clients get plain Markdown: containers are unwrapped, decorative
// components are dropped and their content is dedented. Code blocks are kept.
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

func (f *richUIFilter) mayHoldMarkup(text string) bool {
	if f.fence {
		return strings.HasPrefix(strings.TrimSpace(text), "`")
	}
	return f.depth > 0 || strings.Contains(text, "<") || (!f.midLine && strings.HasPrefix(strings.TrimSpace(text), "`"))
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
	opens := len(richUIOpenTag.FindAllStringIndex(line, -1))
	closes := len(richUICloseTag.FindAllStringIndex(line, -1))
	text := richUIOpenTag.ReplaceAllString(line, "")
	text = richUICloseTag.ReplaceAllString(text, "")
	text = richUITextTag.ReplaceAllString(text, "")
	text = richUISelfClosing.ReplaceAllString(text, "")
	hadMarkup := text != line
	f.depth = max(0, f.depth+opens-closes)
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
