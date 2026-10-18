package chatgpt

import (
	"strings"
	"testing"
)

const richUISample = "根据气象北京今天的预报：\n\n<box gap={3} padding={4} border radius=\"xl\">\n  <row align=\"center\" gap={3}>\n    <AsyncImage query=\"sunny sky\" aspectRatio=\"1:1\" maxWidth=\"84px\"/>\n    <box flex=\"1\" gap={1}>\n      <title size=\"xl\">北京 · 晴</title>\n      <text color=\"secondary\" size=\"sm\">夜间最低约 6°C</text>\n    </box>\n  </row>\n  <divider color=\"subtle\"/>\n  <row align=\"start\" gap={3}>\n    <icon name=\"wind\" size=\"xl\"/>\n    <box flex=\"1\" gap={1}>\n      **风力较大**\n\n      白天偏北风约 3 级。\n    </box>\n  </row>\n</box>\n\n<text color=\"secondary\" size=\"xs\">来源：[气象北京](https://example.com/a)。<Cite refs={[\"turn1search0\"]}/></text>\n\n**提醒：** 注意保暖。"

const richUIWant = "根据气象北京今天的预报：\n\n北京 · 晴\n夜间最低约 6°C\n**风力较大**\n白天偏北风约 3 级。\n\n来源：[气象北京](https://example.com/a)。\n\n**提醒：** 注意保暖。"

func TestRichUIFilterTurnsWebMarkupIntoMarkdown(t *testing.T) {
	whole := &richUIFilter{}
	got := whole.Feed(richUISample) + whole.Flush()
	if got != richUIWant {
		t.Fatalf("got %q\nwant %q", got, richUIWant)
	}
	streamed := &richUIFilter{}
	var out strings.Builder
	for _, r := range richUISample {
		out.WriteString(streamed.Feed(string(r)))
	}
	out.WriteString(streamed.Flush())
	if out.String() != richUIWant {
		t.Fatalf("streamed %q", out.String())
	}
}

func TestRichUIFilterKeepsCodeAndPlainText(t *testing.T) {
	text := "看这段 HTML：\n```html\n<box gap={3}><title>x</title></box>\n<AsyncImage/>\n```\n比较 1 < 2 与 a<b 的写法。"
	f := &richUIFilter{}
	if got := f.Feed(text) + f.Flush(); got != text {
		t.Fatalf("got %q", got)
	}
	plain := &richUIFilter{}
	if got := plain.Feed("普通回答，没有标记。"); got != "普通回答，没有标记。" {
		t.Fatalf("plain text was held back: %q", got)
	}
}

func TestRichUIFilterUnwrapsHyphenatedContainers(t *testing.T) {
	f := &richUIFilter{}
	text := "<grid columns={2} gap={3}>\n  <grid-item>\n    <title size=\"lg\">24°C</title>\n  </grid-item>\n</grid>\nDone"
	if got := f.Feed(text) + f.Flush(); got != "24°C\nDone" {
		t.Fatalf("got %q", got)
	}
}
