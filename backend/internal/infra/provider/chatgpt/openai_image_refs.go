// Protocol portions adapted from SkyKoo/gptGrok2api at 15773099641575776025e21d1290b5209b058c5a. See LICENSE.reference.
package chatgpt

import (
	"regexp"
	"sort"
	"strings"
)

var openAIImageFileIDPattern = regexp.MustCompile(`(?i)^file_[a-z0-9][a-z0-9_-]{5,}$`)

func collectOpenAIImageRefs(value any, conversationID *string, fileIDs *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "conversation_id", "conversationid", "conversation-id":
				if *conversationID == "" {
					*conversationID = stringValue(item)
				}
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "asset_pointer", "assetpointer":
				pointer := stringValue(item)
				if strings.HasPrefix(pointer, "file-service://") {
					*fileIDs = append(*fileIDs, strings.TrimPrefix(pointer, "file-service://"))
				} else if strings.HasPrefix(pointer, "sediment://") {
					*fileIDs = append(*fileIDs, pointer)
				}
			case "file_id", "fileid":
				id := stringValue(item)
				if isOpenAIImageFileID(id) {
					*fileIDs = append(*fileIDs, id)
				}
			}
			collectOpenAIImageRefs(item, conversationID, fileIDs)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIImageRefs(item, conversationID, fileIDs)
		}
	case string:
		if *conversationID == "" {
			if match := regexp.MustCompile(`(?i)"conversation[_-]?id"\s*:\s*"([^"]+)"`).FindStringSubmatch(typed); len(match) == 2 {
				*conversationID = strings.TrimSpace(match[1])
			}
		}
		for _, match := range regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, match[1])
			}
		}
		for _, match := range regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, "sediment://"+match[1])
			}
		}
		for _, match := range regexp.MustCompile(`(?i)\b(file_[a-z0-9][a-z0-9_-]{5,})\b`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, match[1])
			}
		}
	}
}

type openAIImageOutputRecord struct {
	messageID string
	createdAt float64
	refs      []string
}

// collectOpenAIGeneratedImageRefs mirrors ChatGPT's conversation boundary: only
// tool and assistant records in mapping can produce downloadable outputs. User
// records contain uploaded references and must never enter the result list.
func collectOpenAIGeneratedImageRefs(value any, conversationID *string, fileIDs *[]string) {
	collectOpenAIConversationID(value, conversationID)
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	mapping, ok := root["mapping"].(map[string]any)
	if !ok {
		return
	}
	records := make([]openAIImageOutputRecord, 0, len(mapping))
	for messageID, rawNode := range mapping {
		node, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		message, ok := node["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringValue(author["role"])))
		if role != "tool" && role != "assistant" {
			continue
		}
		content := message["content"]
		metadata := message["metadata"]
		hasAssetPointer := hasOpenAIImageAssetPointer(content) || hasOpenAIImageAssetPointer(metadata)
		refs := []string{}
		if role == "assistant" {
			if !hasAssetPointer {
				continue
			}
			collectOpenAIAssetPointerRefs(content, &refs)
			collectOpenAIAssetPointerRefs(metadata, &refs)
		} else {
			unusedConversationID := ""
			collectOpenAIImageRefs(map[string]any{"content": content, "metadata": metadata}, &unusedConversationID, &refs)
		}
		refs = uniqueStrings(refs)
		filtered := refs[:0]
		for _, ref := range refs {
			if strings.TrimSpace(ref) != "file_upload" {
				filtered = append(filtered, ref)
			}
		}
		if len(filtered) == 0 {
			continue
		}
		createdAt, _ := numberValue(message["create_time"])
		records = append(records, openAIImageOutputRecord{messageID: messageID, createdAt: createdAt, refs: filtered})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].createdAt == records[j].createdAt {
			return records[i].messageID < records[j].messageID
		}
		return records[i].createdAt < records[j].createdAt
	})
	for _, record := range records {
		*fileIDs = append(*fileIDs, record.refs...)
	}
}

func collectOpenAIConversationID(value any, conversationID *string) {
	if *conversationID != "" {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "conversation_id" || normalized == "conversationid" || normalized == "conversation-id" {
				*conversationID = stringValue(item)
				if *conversationID != "" {
					return
				}
			}
			collectOpenAIConversationID(item, conversationID)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIConversationID(item, conversationID)
		}
	}
}

func hasOpenAIImageAssetPointer(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if strings.EqualFold(strings.TrimSpace(stringValue(typed["content_type"])), "image_asset_pointer") {
			return true
		}
		pointer := strings.TrimSpace(stringValue(typed["asset_pointer"]))
		if strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
		for _, item := range typed {
			if hasOpenAIImageAssetPointer(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if hasOpenAIImageAssetPointer(item) {
				return true
			}
		}
	}
	return false
}

func collectOpenAIAssetPointerRefs(value any, refs *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		pointer := strings.TrimSpace(stringValue(typed["asset_pointer"]))
		if strings.HasPrefix(pointer, "file-service://") {
			*refs = append(*refs, strings.TrimPrefix(pointer, "file-service://"))
		} else if strings.HasPrefix(pointer, "sediment://") {
			*refs = append(*refs, pointer)
		}
		for _, item := range typed {
			collectOpenAIAssetPointerRefs(item, refs)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIAssetPointerRefs(item, refs)
		}
	}
}

func isOpenAIImageFileID(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "file-service://") {
		value = strings.TrimPrefix(value, "file-service://")
	}
	return openAIImageFileIDPattern.MatchString(value)
}
