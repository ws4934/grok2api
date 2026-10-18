package toolemu

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// decodedEnd is where encoding/json ends the first value of text, or -1.
func decodedEnd(text string) int {
	decoder := json.NewDecoder(strings.NewReader(text))
	var value json.RawMessage
	if decoder.Decode(&value) != nil {
		return -1
	}
	return int(decoder.InputOffset())
}

// scannedEnd feeds text to a jsonScan in pieces cut at cuts, then ends it.
func scannedEnd(text string, cuts []int) int {
	var j jsonScan
	for _, cut := range cuts {
		if end := j.scan(text[:cut], false); end != jsonOpen {
			return max(end, -1)
		}
	}
	return max(j.scan(text, true), -1)
}

var jsonPieces = []string{
	"{", "}", "[", "]", `"`, `\`, ":", ",", " ", "\n", "\t", "0", "1", "9", "-", ".", "e", "E", "+",
	"true", "false", "null", "tru", "nul", `"a"`, `"k":`, `\u00e9`, `\u12`, `\n`, `\x`, "x", "<", ">",
	"中", "\x01", "1.5e+3", "-0", "01", `"</tool_calls>"`, "{}", "[]",
}

// TestJSONScanMatchesEncodingJSON: the streaming scanner ends values where
// encoding/json does and rejects what it rejects, fed whole or in pieces.
func TestJSONScanMatchesEncodingJSON(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	texts := []string{
		"", "   ", "123", "123 ", "-", "1.", "1e", "1e+", "0x", `"abc"`, `"abc"x`, "truex", `{"a":1}}`,
		`{"a":[1,2,{"b":null}],"c":"\u00e9\n"}`, `[1,]`, `{"a" 1}`, `{,}`, "\xef\xbb\xbf{}",
		strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth),
		strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1),
	}
	for range 20000 {
		var b strings.Builder
		for n := 1 + r.Intn(12); n > 0; n-- {
			b.WriteString(jsonPieces[r.Intn(len(jsonPieces))])
		}
		texts = append(texts, b.String())
	}
	for _, text := range texts {
		want := decodedEnd(text)
		if got := scannedEnd(text, nil); got != want {
			t.Fatalf("%q: scanned end %d, encoding/json %d", text, got, want)
		}
		var cuts []int
		for i := 1; i < len(text); i++ {
			if r.Intn(3) == 0 {
				cuts = append(cuts, i)
			}
		}
		if got := scannedEnd(text, cuts); got != want {
			t.Fatalf("%q cut at %v: scanned end %d, encoding/json %d", text, cuts, got, want)
		}
	}
}
