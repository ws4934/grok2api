package toolemu

import (
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

// siftCuts streams reply through a Sieve cut at cuts, as the ChatGPT adapter
// does, and returns the client text and the calls as name+arguments.
func siftCuts(available map[string]struct{}, reply string, cuts []int) (string, []string) {
	sieve := NewSieve(available)
	var text strings.Builder
	var calls []string
	take := func(result StreamResult) {
		text.WriteString(result.SafeText)
		for _, call := range result.Calls {
			calls = append(calls, call.Name+call.Arguments)
		}
	}
	prev := 0
	for _, cut := range append(cuts, len(reply)) {
		for result := sieve.Feed(reply[prev:cut]); ; result = sieve.Feed("") {
			take(result)
			if !result.Complete {
				break
			}
			if len(result.Calls) == 0 {
				text.WriteString(result.Raw)
			}
		}
		prev = cut
	}
	take(sieve.Flush())
	return text.String(), calls
}

func fuzzBlock(name, arguments string, upper bool) string {
	block := "<tool_calls>\n<tool_call><tool_name>" + name + "</tool_name><parameters>" + arguments + "</parameters></tool_call>\n</tool_calls>"
	if upper {
		block = strings.ReplaceAll(block, "tool_call", "TOOL_CALL")
		block = strings.ReplaceAll(block, "parameters", "Parameters")
	}
	return block
}

var fuzzArguments = []string{
	`{"q":"x"}`,
	`{"content":"</tool_calls>"}`,
	`{"content":"</parameters></tool_call></tool_calls><tool_calls>"}`,
	`{"content":"<parameters>{}</parameters></tool_call></tool_calls>"}`,
	`{"a":"\"</tool_calls>\\","b":[1,{"c":"<tool_calls>"}]}`,
	`{"u":"</tool_calls>","m":"İstanbul 中文 ✓"}`,
	`{"n": 1820000000000000123}`,
	`{&quot;q&quot;:&quot;x&quot;}`,
	`{"bad":"unterminated}`,
	`{"a":1}}`,
	`{"q": "x"`,
	``,
	`{"x":"` + "```" + `"}`,
}

var fuzzProse = []string{
	"Hello ", "İstanbul ", "中文 ", "a < b ", "`code` ", "\n", "```go\nx := 1\n```\n", "```xml\n", "```\n", "\n```",
	"<tool_ca", "<TOOL_C", "tail", " ", "  \n", "<tool_callsX>", "`", "``",
	"<tool_calls><tool_call><tool_name>lookup</tool_call></tool_calls>",
}

func fuzzReply(r *rand.Rand) string {
	var reply strings.Builder
	n := 1 + r.Intn(6)
	for i := range n {
		switch r.Intn(5) {
		case 0, 1:
			reply.WriteString(fuzzProse[r.Intn(len(fuzzProse))])
		case 2:
			name := "lookup"
			if r.Intn(4) == 0 {
				name = "rm"
			}
			reply.WriteString(fuzzBlock(name, fuzzArguments[r.Intn(len(fuzzArguments))], r.Intn(3) == 0))
		case 3:
			reply.WriteString("```xml\n" + fuzzBlock("lookup", fuzzArguments[r.Intn(len(fuzzArguments))], false) + "\n```\n")
		case 4:
			if i == n-1 {
				reply.WriteString(`<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>{"q":1}</parameters></tool_call>`)
			}
		}
	}
	return reply.String()
}

// TestSieveSplitEquivalence: however the reply is cut into deltas (at every
// byte, byte by byte, at random), the client text and the calls are those of
// the whole reply.
func TestSieveSplitEquivalence(t *testing.T) {
	available := map[string]struct{}{"lookup": {}, "write_file": {}}
	r := rand.New(rand.NewSource(1))
	for range 60 {
		reply := fuzzReply(r)
		wantText, wantCalls := siftCuts(available, reply, nil)
		check := func(cuts []int) {
			if text, calls := siftCuts(available, reply, cuts); text != wantText || fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
				t.Fatalf("reply %q cut at %v\nwhole text %q calls %v\nsplit text %q calls %v", reply, cuts, wantText, wantCalls, text, calls)
			}
		}
		var every, random []int
		for i := 1; i < len(reply); i++ {
			check([]int{i})
			every = append(every, i)
			if r.Intn(6) == 0 {
				random = append(random, i)
			}
		}
		check(every)
		check(random)
	}
}

// TestSieveWaitsForClosingTagAfterJSON: a delta that ends right after the
// JSON arguments must not end the block at a </parameters> inside them.
func TestSieveWaitsForClosingTagAfterJSON(t *testing.T) {
	available := map[string]struct{}{"write_file": {}}
	arguments := `{"path":"prompt.txt","content":"<tool_calls><tool_call><tool_name>x</tool_name><parameters>{}</parameters></tool_call></tool_calls>"}`
	block := writeFileBlock(arguments)
	cut := strings.Index(block, `"}</parameters>`) + 2
	for _, at := range []int{cut, cut + 3, cut + 10} {
		if text, calls := siftCuts(available, block, []int{at}); text != "" || len(calls) != 1 || calls[0] != "write_file"+arguments {
			t.Fatalf("cut after %q: text %q calls %v", block[:at], text, calls)
		}
	}
	// A name left open ends at the next tag, so the block still ends at its
	// own </tool_calls>.
	open := "<tool_calls><tool_call><tool_name>lookup</tool_call></tool_calls> then " + writeFileBlock(`{"q":1}`)
	if text, calls := siftCuts(available, open, nil); text != "<tool_calls><tool_call><tool_name>lookup</tool_call></tool_calls> then " || len(calls) != 1 {
		t.Fatalf("open name: text %q calls %v", text, calls)
	}
}

// TestSieveClosingTagsInArgumentsAreLinear: every </tool_calls> inside JSON
// arguments once re-read the whole block (2000 of them in 8-byte deltas:
// 112 ms, 152 MB).
func TestSieveClosingTagsInArgumentsAreLinear(t *testing.T) {
	arguments := `{"path":"fixture.xml","content":"` + strings.Repeat(`<tool_calls></tool_calls>\n`, 2000) + `"}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, calls := sieveAll(map[string]struct{}{"write_file": {}}, writeFileBlock(arguments), 8)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; len(calls) != 1 || calls[0].Arguments != arguments || allocated > 4<<20 {
		t.Fatalf("calls %d, allocated %d bytes", len(calls), allocated)
	}
}
