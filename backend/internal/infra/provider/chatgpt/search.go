package chatgpt

import (
	"fmt"
	"net/url"
	"regexp"
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
	citeOpen   = "<Cite"
)

// Newer answers cite with a Web component instead: <Cite refs={["turn0search8"]}/>,
// where each ref names a search result by its ref_id; other attributes are ignored.
var citeTag = regexp.MustCompile(`^<Cite\b[^>]*?\brefs=\{\[([^\]]*)\]\}[^>]*?/>$`)

// chatSearch is what a reply shows of ChatGPT's own web search, which runs
// server-side: queries and result groups from the search messages, citation
// references from the answer and the search count of server_ste_metadata.
type chatSearch struct {
	count   int
	queries []string
	sources []searchSource
	refs    map[string]citation
	// results maps result ref IDs (turn0search8) to their page, for <Cite/>.
	results map[string]citation
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
	queries := toAnyList(metadata["search_queries"])
	if model, ok := metadata["search_model_queries"].(map[string]any); ok {
		queries = append(queries, toAnyList(model["queries"])...)
	}
	for _, raw := range queries {
		q, ok := raw.(string)
		if query, isObject := raw.(map[string]any); isObject {
			q, ok = stringValue(query["q"]), true
		}
		if q = strings.TrimSpace(q); ok && q != "" && len(s.queries) < maxQueries && !slices.Contains(s.queries, q) {
			s.queries = append(s.queries, q)
		}
	}
	for _, raw := range toAnyList(metadata["search_result_groups"]) {
		group, _ := raw.(map[string]any)
		for _, raw := range toAnyList(group["entries"]) {
			entry, _ := raw.(map[string]any)
			page, title := s.source(stringValue(entry["url"]), stringValue(entry["title"]))
			s.result(entry["ref_id"], page, title, stringValue(entry["attribution"]))
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

// result records the page a result ref ID names, labelled by its site.
func (s *chatSearch) result(rawID any, page, title, site string) {
	id, _ := rawID.(map[string]any)
	turn, hasTurn := id["turn_index"].(float64)
	index, hasIndex := id["ref_index"].(float64)
	kind := stringValue(id["ref_type"])
	if page == "" || !hasTurn || !hasIndex || kind == "" {
		return
	}
	if parsed, err := url.Parse(page); err == nil && site == "" {
		site = strings.TrimPrefix(parsed.Hostname(), "www.")
	}
	label := strings.NewReplacer("[", "", "]", "").Replace(searchresult.NormalizeTitle(site, title))
	if s.results == nil {
		s.results = map[string]citation{}
	}
	s.results[fmt.Sprintf("turn%d%s%d", int(turn), kind, int(index))] = citation{alt: "[" + label + "](" + page + ")", url: page, title: title}
}

// cite renders a <Cite/> component as Markdown links to the results it
// names; a component naming no known result comes back unchanged.
func (s *chatSearch) cite(tag string) (string, []citation) {
	match := citeTag.FindStringSubmatch(tag)
	if s == nil || match == nil {
		return tag, nil
	}
	var cited []citation
	var links []string
	for _, raw := range strings.Split(match[1], ",") {
		c, ok := s.results[strings.Trim(strings.TrimSpace(raw), `"'`)]
		if !ok || s.unsafe[c.url] || slices.ContainsFunc(cited, func(v citation) bool { return v.url == c.url }) {
			continue
		}
		cited, links = append(cited, c), append(links, c.alt)
	}
	if len(cited) == 0 {
		return tag, nil
	}
	return "(" + strings.Join(links, ", ") + ")", cited
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

// citer replaces citation markers and <Cite/> components in the visible
// answer with Markdown links and returns the citations it resolved.
// ponytail: a marker whose reference has not arrived when it completes stays
// as is; hold it until the next snapshot if ChatGPT sends references late.
type citer struct{ pending string }

func (c *citer) Feed(text string, search *chatSearch) (string, []citation) {
	c.pending += text
	var out strings.Builder
	var cited []citation
	for {
		start := strings.IndexRune(c.pending, citeStart)
		if tag := strings.Index(c.pending, citeOpen); tag >= 0 && (start < 0 || tag < start) {
			start = tag
		}
		if start < 0 {
			// Hold a tail that may begin a <Cite/> component.
			keep := 0
			for size := len(citeOpen) - 1; size > 0 && keep == 0; size-- {
				if strings.HasSuffix(c.pending, citeOpen[:size]) {
					keep = size
				}
			}
			out.WriteString(c.pending[:len(c.pending)-keep])
			c.pending = c.pending[len(c.pending)-keep:]
			break
		}
		out.WriteString(c.pending[:start])
		c.pending = c.pending[start:]
		if strings.HasPrefix(c.pending, citeOpen) {
			end := strings.Index(c.pending, "/>")
			if end < 0 && len(c.pending) <= maxMarker {
				break // the component may still complete
			}
			size := len(citeOpen)
			if end >= 0 {
				size = end + len("/>")
			}
			text, refs := search.cite(c.pending[:size])
			c.pending = c.pending[size:]
			out.WriteString(text)
			cited = append(cited, refs...)
			continue
		}
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
