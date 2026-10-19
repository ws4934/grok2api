package web

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
)

func cleanChatToken(parsed *parsedChat, token string) string {
	if !strings.Contains(token, "<grok:render") {
		if token != "" {
			// Visible assistant text separates two citations. Only truly adjacent
			// duplicate render frames should be collapsed.
			parsed.lastCitation = 0
		}
		return token
	}
	matches := grokRenderPattern.FindAllStringSubmatchIndex(token, -1)
	if len(matches) == 0 {
		return token
	}
	var builder strings.Builder
	builderCharacters := 0
	cursor := 0
	for _, match := range matches {
		prefix := token[cursor:match[0]]
		builder.WriteString(prefix)
		builderCharacters += utf8.RuneCountInString(prefix)
		if prefix != "" {
			parsed.lastCitation = 0
		}
		cardID := token[match[2]:match[3]]
		renderType := token[match[6]:match[7]]
		replacement, annotation := renderChatCard(parsed, cardID, renderType)
		if annotation != nil {
			if replacement != "" {
				start := parsed.textCharacterLen() + builderCharacters
				annotation["start_index"] = start
				annotation["end_index"] = start + utf8.RuneCountInString(replacement)
			}
			parsed.Annotations = append(parsed.Annotations, annotation)
		}
		builder.WriteString(replacement)
		builderCharacters += utf8.RuneCountInString(replacement)
		cursor = match[1]
	}
	suffix := token[cursor:]
	builder.WriteString(suffix)
	if suffix != "" {
		parsed.lastCitation = 0
	}
	return builder.String()
}

func renderChatCard(parsed *parsedChat, cardID, renderType string) (string, map[string]any) {
	if parsed.cardCache == nil {
		return "", nil
	}
	card := parsed.cardCache[cardID]
	if card == nil {
		return "", nil
	}
	switch renderType {
	case "render_generated_image", "render_file":
		return "", nil
	case "render_searched_image":
		image, _ := card["image"].(map[string]any)
		if image == nil {
			return "", nil
		}
		title, _ := image["title"].(string)
		thumbnail := firstString(image, "thumbnail", "original")
		link, _ := image["link"].(string)
		if thumbnail == "" {
			return "", nil
		}
		if title == "" {
			title = "image"
		}
		if link != "" {
			return fmt.Sprintf("[![%s](%s)](%s)", title, thumbnail, link), nil
		}
		return fmt.Sprintf("![%s](%s)", title, thumbnail), nil
	case "render_inline_citation":
		value, _ := card["url"].(string)
		value, valid := searchresult.NormalizeURL(value)
		if !valid {
			return "", nil
		}
		if parsed.citationIndex == nil {
			parsed.citationIndex = make(map[string]int)
		}
		index, exists := parsed.citationIndex[value]
		if !exists {
			if len(parsed.citationIndex) >= maxTrackedCitationSources {
				return "", nil
			}
			index = len(parsed.citationIndex) + 1
			parsed.citationIndex[value] = index
		}
		if parsed.lastCitation == index {
			return "", nil
		}
		parsed.lastCitation = index
		annotation := citationAnnotation(parsed, value, index)
		if parsed.DisableInlineCitations {
			if len(parsed.Annotations) >= maxTrackedAnnotations {
				return "", nil
			}
			return "", annotation
		}
		// Inline marker keeps the numeric label. When the structured annotation
		// cap is reached, keep the visible text without growing retained state.
		replacement := fmt.Sprintf("[[%d]](%s)", index, value)
		if len(parsed.Annotations) >= maxTrackedAnnotations {
			return replacement, nil
		}
		return replacement, annotation
	default:
		return "", nil
	}
}

// citationAnnotation keeps the xAI citation label in title while retaining the
// page title separately for the OpenAI Chat Completions nested citation shape.
func citationAnnotation(parsed *parsedChat, rawURL string, index int) map[string]any {
	if index < 1 {
		index = 1
	}
	annotation := map[string]any{
		"type": "url_citation", "url": rawURL, "title": fmt.Sprintf("%d", index),
	}
	if parsed != nil {
		if title := lookupSourcePageTitle(parsed.SearchSources, rawURL); title != "" {
			annotation["source_title"] = title
			return annotation
		}
		for _, call := range parsed.HostedSearchCalls {
			if title := lookupSourcePageTitle(call.Sources, rawURL); title != "" {
				annotation["source_title"] = title
				return annotation
			}
		}
	}
	return annotation
}

// lookupSourcePageTitle returns a non-empty page title for rawURL, or "" if unknown.
// Unlike searchSourceTitle, it does not fall back to the URL itself.
func lookupSourcePageTitle(sources []map[string]any, rawURL string) string {
	if normalized, valid := searchresult.NormalizeURL(rawURL); valid {
		rawURL = normalized
	}
	for _, source := range sources {
		if value, _ := source["url"].(string); value == rawURL {
			if title, _ := source["title"].(string); title != "" {
				return title
			}
			return ""
		}
	}
	return ""
}

func searchSourceTitle(sources []map[string]any, rawURL string) string {
	if title := lookupSourcePageTitle(sources, rawURL); title != "" {
		return title
	}
	if normalized, valid := searchresult.NormalizeURL(rawURL); valid {
		return normalized
	}
	return rawURL
}

func chatAnnotations(annotations []map[string]any) []any {
	values := make([]any, 0, len(annotations))
	for _, annotation := range annotations {
		values = append(values, chatURLCitation(annotation))
	}
	return values
}

// chatURLCitation is Chat Completions nested url_citation (OpenAI-compatible wire).
// OpenAI Chat Completions uses the page title; fall back to the numeric label.
func chatURLCitation(annotation map[string]any) map[string]any {
	title := annotation["title"]
	if sourceTitle, _ := annotation["source_title"].(string); sourceTitle != "" {
		title = sourceTitle
	}
	inner := map[string]any{
		"url": annotation["url"], "title": title,
	}
	if _, ok := annotation["start_index"]; ok {
		inner["start_index"] = annotation["start_index"]
		inner["end_index"] = annotation["end_index"]
	}
	return map[string]any{"type": "url_citation", "url_citation": inner}
}

// responsesURLCitation is the xAI/OpenAI Responses flat url_citation on output_text.
func responsesURLCitation(annotation map[string]any) map[string]any {
	out := map[string]any{
		"type":  "url_citation",
		"url":   annotation["url"],
		"title": annotation["title"],
	}
	if _, ok := annotation["start_index"]; ok {
		out["start_index"] = annotation["start_index"]
		out["end_index"] = annotation["end_index"]
	}
	return out
}

func responsesAnnotations(annotations []map[string]any) []any {
	values := make([]any, 0, len(annotations))
	for _, annotation := range annotations {
		values = append(values, responsesURLCitation(annotation))
	}
	return values
}

// xaiCitationURLs builds response.citations: all source URLs from search tool results.
func xaiCitationURLs(parsed parsedChat) []string {
	out := make([]string, 0, len(parsed.SearchSources)+len(parsed.Annotations))
	seen := make(map[string]struct{}, cap(out))
	appendURL := func(u string) {
		if u == "" {
			return
		}
		if _, exists := seen[u]; exists {
			return
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	for _, source := range parsed.SearchSources {
		u, _ := source["url"].(string)
		appendURL(u)
	}
	// render_citation can arrive for a source missing from tool_result (for
	// example a truncated result pool); it must not disappear from citations.
	for _, annotation := range parsed.Annotations {
		u, _ := annotation["url"].(string)
		appendURL(u)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// finalizeXAIAnnotations ensures annotations exist for no_inline mode from search pool
// when render_citation did not emit per-hit markers.
func finalizeXAIAnnotations(parsed *parsedChat) {
	if parsed == nil {
		return
	}
	if !parsed.DisableInlineCitations {
		return
	}
	if len(parsed.Annotations) > 0 {
		// Strip any accidental positional fields.
		for _, ann := range parsed.Annotations {
			delete(ann, "start_index")
			delete(ann, "end_index")
		}
		return
	}
	if len(parsed.SearchSources) == 0 {
		return
	}
	n := 0
	for _, source := range parsed.SearchSources {
		u, _ := source["url"].(string)
		if u == "" {
			continue
		}
		n++
		annotation := map[string]any{
			"type":  "url_citation",
			"url":   u,
			"title": fmt.Sprintf("%d", n),
		}
		if sourceTitle, _ := source["title"].(string); sourceTitle != "" {
			annotation["source_title"] = sourceTitle
		}
		parsed.Annotations = append(parsed.Annotations, annotation)
	}
}
