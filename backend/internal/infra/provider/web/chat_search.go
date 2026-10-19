package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
)

func collectSearchSources(parsed *parsedChat, response map[string]any) {
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	collectWebSearchResults(parsed, response["webSearchResults"])
	collectWebSearchResults(parsed, response["citedWebSearchResults"])
	collectXSearchResults(parsed, response["xSearchResults"])
	collectXSearchResults(parsed, response["xposts"])
	collectXSearchResults(parsed, response["citedXposts"])
}

func collectWebSearchResults(parsed *parsedChat, value any) {
	if wrapped, _ := value.(map[string]any); wrapped != nil {
		value = wrapped["results"]
	}
	values, _ := value.([]any)
	for _, raw := range values {
		item, _ := raw.(map[string]any)
		rawURL, _ := item["url"].(string)
		if rawURL == "" {
			continue
		}
		title, _ := item["title"].(string)
		appendSearchSource(parsed, rawURL, title, "web")
	}
}

func collectXSearchResults(parsed *parsedChat, value any) {
	if wrapped, _ := value.(map[string]any); wrapped != nil {
		value = wrapped["results"]
	}
	values, _ := value.([]any)
	for _, raw := range values {
		item, _ := raw.(map[string]any)
		username, _ := item["username"].(string)
		postID, _ := item["postId"].(string)
		if username == "" || postID == "" {
			continue
		}
		title, _ := item["text"].(string)
		rawURL := "https://x.com/" + url.PathEscape(username) + "/status/" + url.PathEscape(postID)
		appendSearchSource(parsed, rawURL, title, "x_post")
	}
}

func appendSearchSource(parsed *parsedChat, value, title, sourceType string) {
	value, valid := searchresult.NormalizeURL(value)
	if !valid {
		return
	}
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	if _, exists := parsed.sourceKeys[value]; exists {
		return
	}
	if len(parsed.SearchSources) >= searchresult.MaxResults {
		return
	}
	parsed.sourceKeys[value] = struct{}{}
	title = searchresult.NormalizeTitle(title, value)
	parsed.SearchSources = append(parsed.SearchSources, map[string]any{"url": value, "title": title, "type": sourceType})
}

func collectServerTool(parsed *parsedChat, response map[string]any) {
	if parsed.serverToolKeys == nil {
		parsed.serverToolKeys = make(map[string]struct{})
	}
	key := serverToolKey(response)
	if _, exists := parsed.serverToolKeys[key]; !exists {
		if len(parsed.serverToolKeys) >= maxTrackedServerTools {
			return
		}
		parsed.serverToolKeys[key] = struct{}{}
		parsed.ServerTools++
	}
	if webServerToolName(response) != "web_search" {
		return
	}
	if parsed.webSearchKeys == nil {
		parsed.webSearchKeys = make(map[string]struct{})
	}
	if _, exists := parsed.webSearchKeys[key]; exists || len(parsed.webSearchKeys) >= maxTrackedServerTools {
		return
	}
	parsed.webSearchKeys[key] = struct{}{}
	parsed.WebSearchTools++
}

func serverToolKey(response map[string]any) string {
	key := firstString(response, "rolloutId", "responseId", "toolUsageCardId")
	step, hasStep := numberAsInt(response["messageStepId"])
	if key != "" {
		if hasStep {
			key += fmt.Sprintf(":%d", step)
		}
		return key
	}
	if token, _ := response["token"].(string); token != "" {
		sum := sha256.Sum256([]byte(token))
		return "token:" + hex.EncodeToString(sum[:8])
	}
	if hasStep {
		return fmt.Sprintf("step:%d", step)
	}
	return firstString(response, "messageTag")
}

func webServerToolName(response map[string]any) string {
	if name := strings.ToLower(strings.TrimSpace(firstString(response, "toolName", "tool_name"))); name != "" {
		return name
	}
	if card, _ := response["toolUsageCard"].(map[string]any); card != nil {
		if name := strings.ToLower(strings.TrimSpace(firstString(card, "toolName", "tool_name", "name"))); name != "" {
			return name
		}
		for _, tool := range []struct {
			field string
			name  string
		}{
			{field: "webSearch", name: "web_search"},
			{field: "web_search", name: "web_search"},
			{field: "xSearch", name: "x_search"},
			{field: "x_search", name: "x_search"},
			{field: "browsePage", name: "browse_page"},
			{field: "browse_page", name: "browse_page"},
			{field: "searchImages", name: "search_images"},
			{field: "search_images", name: "search_images"},
			{field: "chatroomSend", name: "chatroom_send"},
			{field: "chatroom_send", name: "chatroom_send"},
		} {
			if card[tool.field] != nil {
				return tool.name
			}
		}
	}
	token, _ := response["token"].(string)
	match := grokToolNamePattern.FindStringSubmatch(token)
	if len(match) < 2 {
		return ""
	}
	name := strings.TrimSpace(match[1])
	name = strings.TrimPrefix(name, "<![CDATA[")
	name = strings.TrimSuffix(name, "]]>")
	return strings.ToLower(strings.TrimSpace(name))
}

func upsertHostedSearchCall(parsed *parsedChat, id, kind, query, status string) *hostedSearchCall {
	if parsed == nil {
		return nil
	}
	if id == "" {
		// Some mgw tool_result frames omit tool_call_id. Correlate only when
		// exactly one unfinished call of the same kind exists; otherwise keep
		// the result separate instead of attaching it to the wrong invocation.
		matchedID := ""
		for i := range parsed.HostedSearchCalls {
			call := &parsed.HostedSearchCalls[i]
			if call.Kind != kind || call.Status == "completed" {
				continue
			}
			if matchedID != "" {
				matchedID = ""
				break
			}
			matchedID = call.ID
		}
		if matchedID != "" {
			id = matchedID
		} else {
			id = fmt.Sprintf("%s_%d", kind, len(parsed.HostedSearchCalls)+1)
		}
	}
	if parsed.hostedSearchByID == nil {
		parsed.hostedSearchByID = make(map[string]int)
	}
	if idx, ok := parsed.hostedSearchByID[id]; ok {
		call := &parsed.HostedSearchCalls[idx]
		if query != "" {
			call.Query = query
		}
		if status != "" {
			call.Status = status
		}
		if kind != "" && call.Kind == "" {
			call.Kind = kind
		}
		return call
	}
	if len(parsed.HostedSearchCalls) >= maxTrackedServerTools {
		return nil
	}
	if status == "" {
		status = "in_progress"
	}
	parsed.HostedSearchCalls = append(parsed.HostedSearchCalls, hostedSearchCall{
		ID: id, Kind: kind, Query: query, Status: status,
	})
	parsed.hostedSearchByID[id] = len(parsed.HostedSearchCalls) - 1
	return &parsed.HostedSearchCalls[len(parsed.HostedSearchCalls)-1]
}

func appendHostedSearchSources(call *hostedSearchCall, sources []map[string]any) {
	if call == nil || len(sources) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(call.Sources)+len(sources))
	for _, existing := range call.Sources {
		if u, _ := existing["url"].(string); u != "" {
			seen[u] = struct{}{}
		}
	}
	for _, source := range sources {
		u, _ := source["url"].(string)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		if len(call.Sources) >= searchresult.MaxResults {
			break
		}
		seen[u] = struct{}{}
		// Normalize early so in-memory shape matches wire (type url + optional title).
		item := map[string]any{"type": "url", "url": u}
		if title, _ := source["title"].(string); title != "" {
			item["title"] = title
		}
		call.Sources = append(call.Sources, item)
	}
}

// hostedSearchActionSources copies sources for web_search_call/x_search_call action.
// Always sets type:"url" (OpenAPI required); preserves title when present.
func hostedSearchActionSources(sources []map[string]any) []map[string]any {
	if len(sources) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		u, _ := source["url"].(string)
		if u == "" {
			continue
		}
		item := map[string]any{"type": "url", "url": u}
		if title, _ := source["title"].(string); title != "" {
			item["title"] = title
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// xaiHostedSearchOutputItems builds Responses output items: web_search_call / x_search_call.
func xaiHostedSearchOutputItems(parsed parsedChat) []any {
	if len(parsed.HostedSearchCalls) == 0 {
		return nil
	}
	items := make([]any, 0, len(parsed.HostedSearchCalls))
	for _, call := range parsed.HostedSearchCalls {
		if call.Status != "completed" && len(call.Sources) == 0 {
			continue
		}
		typeName := "web_search_call"
		if call.Kind == "x_search" {
			typeName = "x_search_call"
		}
		status := call.Status
		if status == "" {
			status = "completed"
		}
		action := map[string]any{"type": "search"}
		if call.Query != "" {
			action["query"] = call.Query
		}
		if len(call.Sources) > 0 {
			// OpenAPI: sources[] requires type:"url"+url; title kept as optional extension.
			action["sources"] = hostedSearchActionSources(call.Sources)
		}
		items = append(items, map[string]any{
			"id": call.ID, "type": typeName, "status": status, "action": action,
		})
	}
	return items
}

func xaiServerSideToolUsage(parsed parsedChat) map[string]any {
	var web, x int64
	for _, call := range parsed.HostedSearchCalls {
		// Billable/successful executions: completed or with returned sources.
		if call.Status != "completed" && len(call.Sources) == 0 {
			continue
		}
		switch call.Kind {
		case "web_search":
			web++
		case "x_search":
			x++
		}
	}
	// Legacy Web frames do not expose hosted call completion records, so their
	// deduplicated tool counters remain the only available signal. For mgw,
	// never turn an in_progress/failed attempt into successful billable usage.
	if len(parsed.HostedSearchCalls) == 0 {
		web = parsed.WebSearchTools
		x = parsed.XSearchTools
	}
	usage := map[string]any{}
	if web > 0 {
		usage["SERVER_SIDE_TOOL_WEB_SEARCH"] = web
	}
	if x > 0 {
		usage["SERVER_SIDE_TOOL_X_SEARCH"] = x
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}
