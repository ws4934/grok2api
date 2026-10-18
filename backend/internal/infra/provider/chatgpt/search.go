package chatgpt

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
)

// ChatGPT marks citations in the answer with private-use runes:
// U+E200 cite U+E202 turn0search3 U+E201. The answer's content_references
// map each marker (matched_text) to the Markdown the Web app shows when the
// text is copied (alt, e.g. "([Example](https://example.com))") and to the
// cited pages (items).
const (
	citeStart = ''
	citeEnd   = ''
	maxMarker = 1024
	// maxQueries bounds the search queries kept from the stream.
	maxQueries = 16
)

// chatSearch is what a reply shows of ChatGPT's own web search, which runs
// server-side: queries and result groups from the search messages, citation
// references from the answer and the search count of server_ste_metadata.
type chatSearch struct {
	count   int
	queries []string
	sources []searchSource
	refs    map[string]citation
	unsafe  map[string]bool
}

type searchSource struct{ url, title string }

// citation is one content reference: the Markdown shown for its marker and
// the first page it cites.
type citation struct{ alt, url, title string }

func (s *chatSearch) used() bool {
	return s != nil && (s.count > 0 || len(s.queries) > 0 || len(s.sources) > 0)
}

// note reads one message snapshot; answer marks the visible merged answer,
// whose latest content_references replace the earlier ones.
func (s *chatSearch) note(message map[string]any, answer bool) {
	metadata, _ := message["metadata"].(map[string]any)
	for _, raw := range toAnyList(metadata["search_queries"]) {
		query, _ := raw.(map[string]any)
		if q := strings.TrimSpace(stringValue(query["q"])); q != "" && len(s.queries) < maxQueries && !slices.Contains(s.queries, q) {
			s.queries = append(s.queries, q)
		}
	}
	for _, raw := range toAnyList(metadata["search_result_groups"]) {
		group, _ := raw.(map[string]any)
		for _, raw := range toAnyList(group["entries"]) {
			entry, _ := raw.(map[string]any)
			s.source(stringValue(entry["url"]), stringValue(entry["title"]))
		}
	}
	if !answer {
		return
	}
	refs := map[string]citation{}
	for _, raw := range toAnyList(metadata["content_references"]) {
		ref, _ := raw.(map[string]any)
		marker := stringValue(ref["matched_text"])
		if !strings.HasPrefix(marker, string(citeStart)) {
			continue
		}
		c := citation{alt: stringValue(ref["alt"])}
		items := toAnyList(ref["items"])
		if len(items) == 0 {
			items = []any{ref}
		}
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if url, title := s.source(stringValue(item["url"]), stringValue(item["title"])); c.url == "" {
				c.url, c.title = url, title
			}
		}
		refs[marker] = c
	}
	if len(refs) > 0 {
		s.refs = refs
	}
}

// source normalizes a result URL ("" when invalid) and records it once
// unless url_moderation flagged it.
func (s *chatSearch) source(raw, title string) (string, string) {
	url, ok := searchresult.NormalizeURL(raw)
	if !ok {
		return "", ""
	}
	for _, existing := range s.sources {
		if existing.url == url {
			return url, existing.title
		}
	}
	title = searchresult.NormalizeTitle(title, url)
	if !s.unsafe[url] && len(s.sources) < searchresult.MaxResults {
		s.sources = append(s.sources, searchSource{url: url, title: title})
	}
	return url, title
}

// block drops a URL that url_moderation flagged as unsafe.
func (s *chatSearch) block(raw string) {
	url, ok := searchresult.NormalizeURL(raw)
	if !ok {
		return
	}
	if s.unsafe == nil {
		s.unsafe = map[string]bool{}
	}
	s.unsafe[url] = true
	s.sources = slices.DeleteFunc(s.sources, func(v searchSource) bool { return v.url == url })
}

// ref resolves a marker; an unsafe page removes the marker altogether.
func (s *chatSearch) ref(marker string) (citation, bool) {
	if s == nil {
		return citation{}, false
	}
	c, ok := s.refs[marker]
	if !ok || c.alt == "" {
		return citation{}, false
	}
	if s.unsafe[c.url] {
		return citation{}, true
	}
	return c, true
}

// action is the web_search_call action: the first query and the sources.
func (s *chatSearch) action() map[string]any {
	query := ""
	if len(s.queries) > 0 {
		query = s.queries[0]
	}
	sources := make([]any, 0, len(s.sources))
	for _, source := range s.sources {
		sources = append(sources, map[string]any{"type": "url", "url": source.url, "title": source.title})
	}
	return map[string]any{"type": "search", "query": query, "sources": sources}
}

// citer replaces citation markers in the visible answer with their alt
// Markdown and returns the citations it resolved.
// ponytail: a marker whose reference has not arrived when it completes stays
// as is; hold it until the next snapshot if ChatGPT sends references late.
type citer struct{ pending string }

func (c *citer) Feed(text string, search *chatSearch) (string, []citation) {
	c.pending += text
	var out strings.Builder
	var cited []citation
	for {
		start := strings.IndexRune(c.pending, citeStart)
		if start < 0 {
			out.WriteString(c.pending)
			c.pending = ""
			break
		}
		out.WriteString(c.pending[:start])
		c.pending = c.pending[start:]
		end := strings.IndexRune(c.pending, citeEnd)
		if end < 0 && len(c.pending) <= maxMarker {
			break // the marker may still complete
		}
		size := utf8.RuneLen(citeStart)
		if end >= 0 {
			size = end + utf8.RuneLen(citeEnd)
		}
		marker := c.pending[:size]
		c.pending = c.pending[size:]
		ref, ok := search.ref(marker)
		if !ok {
			out.WriteString(marker)
			continue
		}
		out.WriteString(ref.alt)
		if ref.url != "" {
			cited = append(cited, ref)
		}
	}
	return out.String(), cited
}

func (c *citer) Flush() string {
	text := c.pending
	c.pending = ""
	return text
}

// annotate finds each cited alt in the final text, in order, and returns
// url_citation annotations with rune offsets. Text cut by the output limit
// loses its annotations instead of getting guessed offsets.
func annotate(text string, cited []citation) []any {
	annotations := []any{}
	cursor, runes := 0, 0
	for _, c := range cited {
		index := strings.Index(text[cursor:], c.alt)
		if index < 0 {
			continue
		}
		runes += utf8.RuneCountInString(text[cursor : cursor+index])
		size := utf8.RuneCountInString(c.alt)
		annotations = append(annotations, map[string]any{"type": "url_citation", "start_index": runes, "end_index": runes + size, "url": c.url, "title": c.title})
		cursor += index + len(c.alt)
		runes += size
	}
	return annotations
}
