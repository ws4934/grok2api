package chatgpt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

// Clients such as agent runtimes resend the whole history on every request.
// Each reply records where its ChatGPT conversation ends, keyed by the history
// the client will send back; a request that extends that history continues
// the conversation with only the new messages, so ChatGPT keeps the context
// and the request stays small.
const (
	conversationTTL  = time.Hour
	maxConversations = 4096
)

// chatCursor is where a follow-up continues a ChatGPT conversation.
type chatCursor struct {
	account              uint64
	conversation, parent string
	expires              time.Time
}

type conversationStore struct {
	mu      sync.Mutex
	cursors map[string]chatCursor
}

// historyKey identifies the history up to end, an index just after a run of
// assistant messages: the places where a reply ended.
type historyKey struct {
	end int
	key string
}

// find returns the latest cursor of the account for a prefix of messages that
// leaves new messages after it, and where those messages start.
func (s *conversationStore) find(keys []historyKey, n int, account uint64, now time.Time) (chatCursor, string, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(keys) - 1; i >= 0; i-- {
		cursor, ok := s.cursors[keys[i].key]
		switch {
		case !ok || keys[i].end >= n:
		case now.After(cursor.expires):
			delete(s.cursors, keys[i].key)
		case cursor.account == account:
			return cursor, keys[i].key, keys[i].end, true
		}
	}
	return chatCursor{}, "", 0, false
}

func (s *conversationStore) save(key string, cursor chatCursor, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = make(map[string]chatCursor)
	}
	if len(s.cursors) >= maxConversations {
		for k, c := range s.cursors {
			if now.After(c.expires) {
				delete(s.cursors, k)
			}
		}
		// ponytail: arbitrary eviction past the cap; LRU if hit rates ever matter.
		for k := range s.cursors {
			if len(s.cursors) < maxConversations {
				break
			}
			delete(s.cursors, k)
		}
	}
	cursor.expires = now.Add(conversationTTL)
	s.cursors[key] = cursor
}

func (s *conversationStore) drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cursors, key)
}

// historyKeys chains a digest over the history, one link per run of messages
// with the same role, and returns the digest after each assistant run. A run
// is read the way any client echoes a reply: text joined and trimmed, calls by
// name and canonical arguments, and empty messages skipped (a reply that only
// calls tools often comes back with an empty text message), so splitting text
// across messages or re-encoding arguments still matches.
func historyKeys(seed string, messages []turnMessage) []historyKey {
	chain := sha256.Sum256([]byte(seed))
	var keys []historyKey
	for i := 0; i < len(messages); {
		if empty(messages[i]) {
			i++
			continue
		}
		role := messages[i].Role
		link := sha256.New()
		link.Write(chain[:])
		writeField(link, role)
		var text strings.Builder
		j := i
		for ; j < len(messages) && (messages[j].Role == role || empty(messages[j])); j++ {
			for _, call := range messages[j].calls {
				writeField(link, "call:"+call.Name+":"+canonicalArguments(call.Arguments))
			}
			if messages[j].calls != nil {
				continue
			}
			for _, part := range messages[j].Parts {
				if part.Image != "" {
					writeField(link, "image:"+part.Image)
				}
				text.WriteString(part.Text)
			}
		}
		writeField(link, "text:"+strings.TrimSpace(text.String()))
		link.Sum(chain[:0])
		if role == "assistant" {
			keys = append(keys, historyKey{end: j, key: hex.EncodeToString(chain[:])})
		}
		i = j
	}
	return keys
}

func empty(message turnMessage) bool {
	if message.calls != nil {
		return false
	}
	for _, part := range message.Parts {
		if part.Image != "" || strings.TrimSpace(part.Text) != "" {
			return false
		}
	}
	return true
}

func writeField(h hash.Hash, value string) {
	fmt.Fprintf(h, "%d:%s|", len(value), value)
}

// canonicalArguments re-encodes JSON arguments with sorted keys and raw
// characters, so a client that parses and re-serialises them still matches.
func canonicalArguments(arguments string) string {
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return strings.TrimSpace(arguments)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return strings.TrimSpace(arguments)
	}
	return strings.TrimSpace(out.String())
}

// echo is the reply as the client will send it back: its text, then its calls.
func (t *turn) echo() []turnMessage {
	var text strings.Builder
	var calls []toolemu.Call
	for _, raw := range t.output {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				value, _ := part["text"].(string)
				text.WriteString(value)
			}
		case "function_call":
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			calls = append(calls, toolemu.Call{Name: name, Arguments: arguments})
		}
	}
	var out []turnMessage
	if text.Len() > 0 {
		out = append(out, textMessage("assistant", text.String()))
	}
	if calls != nil {
		out = append(out, turnMessage{Role: "assistant", calls: calls})
	}
	return out
}
