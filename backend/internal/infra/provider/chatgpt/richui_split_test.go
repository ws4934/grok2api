package chatgpt

import (
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

// filterCuts runs text through a richUIFilter cut at cuts.
func filterCuts(text string, cuts []int) string {
	f := &richUIFilter{}
	var out strings.Builder
	prev := 0
	for _, cut := range append(cuts, len(text)) {
		out.WriteString(f.Feed(text[prev:cut]))
		prev = cut
	}
	return out.String() + f.Flush()
}

var richUIPieces = []string{
	"Hello ", "中文 ", "a < b ", "`code` ", "`<box>` ", "\n", "\n\n", "   ", "  ", "\t", "- item\n", "  - nested ",
	"```bash\n", "```\n", "npm i\n", "<box gap={3}>\n", "</box>\n", "<title>T</title>\n", "<row>", "</row>",
	"<AsyncImage query=\"x\"/>", "<Cite refs={[\"turn0search1\"]}/>", "<Cite", " refs={[\"turn0search2\"]} />",
	"<list>\n", "x", "``", "`",
}

// TestRichUIFilterSplitEquivalence: however the answer is cut into deltas
// (runes stay whole, as in the upstream's JSON strings), the client gets the
// text of the whole answer.
func TestRichUIFilterSplitEquivalence(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for range 400 {
		var b strings.Builder
		for n := 2 + r.Intn(10); n > 0; n-- {
			b.WriteString(richUIPieces[r.Intn(len(richUIPieces))])
		}
		text := b.String()
		want := filterCuts(text, nil)
		var every []int
		for i := range text {
			if i > 0 {
				every = append(every, i)
			}
		}
		for _, cuts := range append([][]int{every}, splitAt(every)...) {
			if got := filterCuts(text, cuts); got != want {
				t.Fatalf("text %q cut at %v\nwhole %q\nsplit %q", text, cuts, want, got)
			}
		}
	}
}

// splitAt lists one cut at each of cuts.
func splitAt(cuts []int) [][]int {
	var single [][]int
	for _, cut := range cuts {
		single = append(single, []int{cut})
	}
	return single
}

// TestRichUIFilterRestOfLineIsProse: once the start of a line was passed
// through, the rest is judged as part of that prose line.
func TestRichUIFilterRestOfLineIsProse(t *testing.T) {
	for name, c := range map[string]struct {
		chunks []string
		want   string
	}{
		"container tag at line end": {[]string{"Wrap it in ", "<box>\n\nNext paragraph.\n\n- a\n  - nested\n"}, "Wrap it in <box>\n\nNext paragraph.\n\n- a\n  - nested\n"},
		"markup after prose":        {[]string{"- Go 1.26 is out. ", "<Cite refs={[\"turn0image1\"]}/>\n- Next item\n"}, "- Go 1.26 is out. \n- Next item\n"},
		"indented fence in a list":  {[]string{"1. Install:\n", "   ", "```html\n", "   <title>x</title>\n", "   ", "```\n", "2. <title>Run</title>\n"}, "1. Install:\n   ```html\n   <title>x</title>\n   ```\n2. Run\n"},
		"nested item with markup":   {[]string{"- a\n  - nested ", "<AsyncImage query=\"x\"/>\n"}, "- a\n  - nested \n"},
		"space before markup":       {[]string{"Hello", " <Cite refs={[\"turn0search1\"]}/>"}, "Hello "},
	} {
		if got := filterCuts(strings.Join(c.chunks, ""), nil); got != c.want {
			t.Errorf("%s: whole %q, want %q", name, got, c.want)
		}
		f := &richUIFilter{}
		var out strings.Builder
		for _, chunk := range c.chunks {
			out.WriteString(f.Feed(chunk))
		}
		if got := out.String() + f.Flush(); got != c.want {
			t.Errorf("%s: streamed %q, want %q", name, got, c.want)
		}
	}
}

// TestRichUIFilterLongHeldLineIsLinear: a held line was copied and rescanned
// on every delta (64 KiB in 4-byte deltas: 551 MiB allocated).
func TestRichUIFilterLongHeldLineIsLinear(t *testing.T) {
	line := "Use `x` " + strings.Repeat("word ", 64<<10/5)
	cuts := make([]int, 0, len(line)/4)
	for i := 4; i < len(line); i += 4 {
		cuts = append(cuts, i)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := filterCuts(line, cuts)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; got != line || allocated > 4<<20 {
		t.Fatalf("allocated %d bytes, output intact %v", allocated, got == line)
	}
}
