package chatgpt

import "testing"

func TestCiteComponentsMayCarryOtherAttributes(t *testing.T) {
	search := &chatSearch{}
	search.result(map[string]any{"turn_index": 7.0, "ref_type": "search", "ref_index": 8.0}, "https://go.dev/doc/devel/release", "Release History", "go.dev")
	c := &citer{}
	text, cited := c.Feed(`Out. <Cite variant="inline" refs={["turn7search8"]} />`, search)
	if text+c.Flush() != "Out. ([go.dev](https://go.dev/doc/devel/release))" || len(cited) != 1 {
		t.Fatalf("text = %q, cited = %v", text, cited)
	}
}
