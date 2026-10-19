package audit

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// sanitizeHeadersByMarshal is the previous implementation, which re-encoded
// the whole map after every header.
func sanitizeHeadersByMarshal(headers map[string][]string) map[string][]string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make(map[string][]string, len(headers))
	for _, originalName := range names {
		if len(result) >= requestHeaderCountLimit {
			break
		}
		name := truncateRequestMetadata(strings.TrimSpace(originalName), requestHeaderNameLimit)
		if name == "" {
			continue
		}
		values := headers[originalName]
		cloned := make([]string, 0, len(values))
		if isSensitiveRequestHeader(name) {
			cloned = []string{"[REDACTED]"}
		} else {
			for _, item := range values[:min(len(values), requestHeaderValuesLimit)] {
				cloned = append(cloned, truncateRequestMetadata(item, requestHeaderValueLimit))
			}
		}
		result[name] = cloned
		if encoded, _ := json.Marshal(result); len(encoded) > requestHeadersLimit {
			delete(result, name)
			break
		}
	}
	return result
}

func TestSanitizedHeadersMatchTheMarshalBound(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for round := range 300 {
		headers := make(map[string][]string)
		for index := range random.IntN(160) {
			name := fmt.Sprintf("X-H%03d-%s", index, strings.Repeat("n", random.IntN(40)))
			if random.IntN(10) == 0 {
				name = "Authorization-" + name
			}
			values := make([]string, 1+random.IntN(3))
			for v := range values {
				values[v] = strings.Repeat(string(rune('a'+random.IntN(26))), random.IntN(1200)) + `"<&>é`
			}
			headers[name] = values
		}
		got := sanitizeRequestMetadata(auditdomain.Record{RequestHeaders: headers}).RequestHeaders
		want := sanitizeHeadersByMarshal(headers)
		if len(want) == 0 {
			want = nil
		}
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: got %d headers, want %d", round, len(got), len(want))
		}
		if encoded, _ := json.Marshal(got); len(encoded) > requestHeadersLimit {
			t.Fatalf("round %d: %d bytes over the limit", round, len(encoded))
		}
	}
}
