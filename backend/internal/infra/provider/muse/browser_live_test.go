package muse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live tests drive the real muse.ai through a DevTools endpoint:
//
//	MUSE_LIVE_BROWSER=http://127.0.0.1:9222   browser-only tests
//	MUSE_LIVE_COOKIES=<Cookie-Editor JSON>     account tests (no quota)
//	MUSE_LIVE_SPEND=1                          tests that use weekly quota

func liveEngine(t *testing.T) *browserEngine {
	t.Helper()
	endpoint := os.Getenv("MUSE_LIVE_BROWSER")
	if endpoint == "" {
		t.Skip("MUSE_LIVE_BROWSER not set")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return newBrowserEngine(func() Config { return Config{BrowserURL: endpoint} }, func() *slog.Logger { return logger })
}

func liveSession(t *testing.T, spend bool) session {
	t.Helper()
	path := os.Getenv("MUSE_LIVE_COOKIES")
	if path == "" {
		t.Skip("MUSE_LIVE_COOKIES not set")
	}
	if spend && os.Getenv("MUSE_LIVE_SPEND") != "1" {
		t.Skip("MUSE_LIVE_SPEND not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var jar []cookie
	if err := json.Unmarshal(data, &jar); err != nil {
		t.Fatal("cookie file is not a Cookie-Editor JSON array")
	}
	return session{Cookies: jar}
}

func TestLiveProbeBrowser(t *testing.T) {
	e := liveEngine(t)
	endpoint := e.cfg().browserURL()
	for _, u := range []string{endpoint, strings.Replace(endpoint, "127.0.0.1", "localhost", 1)} {
		if err := probeBrowser(context.Background(), u); err != nil {
			t.Errorf("probeBrowser(%s): %v", u, err)
		}
	}
}

// blankPage opens an about:blank page in a throwaway context.
func blankPage(t *testing.T, e *browserEngine) *page {
	t.Helper()
	ctx := context.Background()
	conn, err := e.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var bc struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := conn.call(ctx, "", "Target.createBrowserContext", map[string]any{"disposeOnDetach": true}, &bc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.call(context.Background(), "", "Target.disposeBrowserContext", map[string]any{"browserContextId": bc.BrowserContextID}, nil)
	})
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := conn.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": bc.BrowserContextID}, &target); err != nil {
		t.Fatal(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := conn.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	return &page{conn: conn, session: attached.SessionID, log: e.logger()}
}

// streamdownFixture follows the markup muse.ai renders (see the probe dumps).
const streamdownFixture = `
<div class="hatch-chat-groupable-bubble hatch-agent-bubble-bg"><div data-hatch-markdown-streaming="false"><p>old reply</p></div></div>
<div class="hatch-chat-groupable-bubble bg-chat-user-bubble"><span class="sr-only">You:</span><p>question about the weekly limit</p></div>
<div class="hatch-chat-groupable-bubble bg-[var(--hatch-agent-bubble-bg)]"><div class="flex"><div class="prose" data-hatch-markdown-streaming="true"><div class="space-y-4">
<h2 data-streamdown="heading-2"><span data-sd-animate="true">Slow</span> <span data-sd-animate="true">start</span></h2>
<p node="[object Object]">Use <span class="font-semibold" data-streamdown="strong">bold </span>and <em>italic</em>, <code data-streamdown="inline-code">a` + "`" + `b</code>, <a data-streamdown="link" href="https://example.com/x">a link</a>.<br>Next line about the weekly limit.</p>
<ul data-streamdown="unordered-list">
<li data-streamdown="list-item">first</li>
<li data-streamdown="list-item">second
<ol data-streamdown="ordered-list" start="3"><li>three</li><li><p>four</p></li></ol>
</li>
</ul>
<div data-language="go" data-streamdown="code-block"><div data-streamdown="code-block-header"><span>go</span></div><div><div data-streamdown="code-block-actions"><button title="Copy Code"><svg></svg></button></div></div><div data-streamdown="code-block-body"><pre><code><span class="block"><span>func f() {</span></span><span class="block"><span>	s := "` + "```" + `"</span></span><span class="block"><span>}</span></span></code></pre></div></div>
<div data-streamdown="table-wrapper"><div><table data-streamdown="table"><thead><tr><th>Key</th><th>Value</th></tr></thead><tbody><tr><td>a|b</td><td><span data-streamdown="strong">1</span></td></tr><tr><td>c</td><td>2</td></tr></tbody></table></div></div>
<blockquote data-streamdown="blockquote"><p>quoted</p><p>twice</p></blockquote>
<hr data-streamdown="horizontal-rule">
<p>end</p>
</div></div></div></div>`

const streamdownMarkdown = "## Slow start\n\n" +
	"Use **bold** and *italic*, ``a`b``, [a link](https://example.com/x).\nNext line about the weekly limit.\n\n" +
	"- first\n- second\n  3. three\n  4. four\n\n" +
	"````go\nfunc f() {\n\ts := \"```\"\n}\n````\n\n" +
	"| Key | Value |\n| --- | --- |\n| a\\|b | **1** |\n| c | 2 |\n\n" +
	"> quoted\n>\n> twice\n\n" +
	"---\n\nend"

func TestLiveMarkdown(t *testing.T) {
	e := liveEngine(t)
	p := blankPage(t, e)
	ctx := context.Background()
	if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": "document.body.innerHTML = " + mustJSON(streamdownFixture)}, nil); err != nil {
		t.Fatal(err)
	}
	var st chatState
	if err := p.js(ctx, "chat", &st); err != nil {
		t.Fatal(err)
	}
	if st.MD != streamdownMarkdown {
		t.Errorf("markdown:\n%s\n--- want:\n%s", st.MD, streamdownMarkdown)
	}
	if st.Limit || st.Stuck || st.Stop {
		t.Errorf("flags = %+v, want none (limit words only appear inside bubbles)", st)
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func TestLiveLoggedOut(t *testing.T) {
	e := liveEngine(t)
	start := time.Now()
	_, err := e.Usage(context.Background(), session{})
	if !errors.Is(err, errLoggedOut) {
		t.Fatalf("Usage without cookies = %v, want errLoggedOut", err)
	}
	t.Logf("logged out detected in %s", time.Since(start).Round(time.Millisecond))
}

func TestLiveUsage(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, false)
	start := time.Now()
	u, err := e.Usage(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("usage %+v in %s", u, time.Since(start).Round(time.Millisecond))
}

func testPNG(c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// TestLiveAttach uploads two files into the composer without sending.
func TestLiveAttach(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, false)
	files := []attachment{
		{Name: "red.png", MIME: "image/png", Data: testPNG(color.RGBA{R: 255, A: 255})},
		{MIME: "image/png", Data: testPNG(color.RGBA{B: 255, A: 255})},
	}
	err := e.withPage(context.Background(), s, "/thread/new", func(p *page) error {
		start := time.Now()
		if err := p.attach(context.Background(), files); err != nil {
			return err
		}
		t.Logf("2 attachments confirmed in %s", time.Since(start).Round(time.Millisecond))
		return p.attach(context.Background(), nil) // clears them again
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLiveVideoResume(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, false)
	start := time.Now()
	f, err := e.Generate(context.Background(), s, mediaJob{Kind: mediaVideo, Thread: "/thread/5de71fe5-c4e8-4c9b-9e83-c0719bc0f212"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("video %s %d bytes %dx%d in %s", f.MIME, len(f.Data), f.Width, f.Height, time.Since(start).Round(time.Millisecond))
	if f.MIME != "video/mp4" || len(f.Data) < 1<<20 {
		t.Fatalf("unexpected video: %s %d bytes", f.MIME, len(f.Data))
	}
}

func TestLiveChat(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, true)
	start := time.Now()
	snapshots := 0
	reply, err := e.Chat(context.Background(), s, chatTurn{Prompt: "Reply in Markdown with exactly these parts and nothing else: " +
		"a level-2 heading 'Demo'; one paragraph with **bold**, *italic*, `inline code` and a link to https://example.com; " +
		"a bulleted list of 2 items whose second item has a nested numbered list of 2 items; a fenced Go code block of 3 lines; " +
		"a table with 2 columns and 2 data rows; a one-line blockquote."}, func(md string) error {
		snapshots++
		t.Logf("%6.2fs snapshot %d: %d chars", time.Since(start).Seconds(), snapshots, len(md))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reply after %s, %d snapshots:\n%s", time.Since(start).Round(time.Millisecond), snapshots, reply)
}

func TestLiveChatLargePrompt(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, true)
	filler := strings.Repeat("Filler line, ignore it. 填充行，请忽略。\r\n", 1500)
	prompt := "Reply with only the word PONG and nothing else.\n\n" + filler + "\nReply with only the word PONG."
	start := time.Now()
	reply, err := e.Chat(context.Background(), s, chatTurn{Prompt: prompt}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d-byte prompt answered in %s: %q", len(prompt), time.Since(start).Round(time.Millisecond), reply)
	if !strings.Contains(reply, "PONG") {
		t.Fatalf("reply %q has no PONG", reply)
	}
}

func TestLiveImageAndEdit(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, true)
	start := time.Now()
	var thread string
	img, err := e.Generate(context.Background(), s, mediaJob{Kind: mediaImage,
		Prompt:   "Generate a brand-new image from scratch: a small green cactus in a terracotta pot on a plain white background, square.",
		OnThread: func(path string) error { thread = path; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("image %s %d bytes %dx%d thread %s in %s", img.MIME, len(img.Data), img.Width, img.Height, thread, time.Since(start).Round(time.Millisecond))
	if !strings.HasPrefix(img.MIME, "image/") || img.Width == 0 || thread == "" {
		t.Fatalf("unexpected image result")
	}
	saveLive("image"+extension(img.MIME), img.Data)

	start = time.Now()
	edit, err := e.Generate(context.Background(), s, mediaJob{Kind: mediaImage,
		Prompt: "Edit the attached image: make the pot bright blue and keep everything else the same.",
		Inputs: []attachment{{Name: "cactus" + extension(img.MIME), MIME: img.MIME, Data: img.Data}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("edit %s %d bytes %dx%d in %s", edit.MIME, len(edit.Data), edit.Width, edit.Height, time.Since(start).Round(time.Millisecond))
	if bytes.Equal(edit.Data, img.Data) {
		t.Fatal("edit returned the input image")
	}
	saveLive("edit"+extension(edit.MIME), edit.Data)
}

// saveLive keeps a live result for inspection when MUSE_LIVE_OUT names a directory.
func saveLive(name string, data []byte) {
	if dir := os.Getenv("MUSE_LIVE_OUT"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, name), data, 0o600)
	}
}

// TestLiveMediaFixture runs the media wait and chunked transfer on a fake
// thread: a ~3 MB image (several chunks each way), then a text-only reply,
// then limit banners.
func TestLiveMediaFixture(t *testing.T) {
	e := liveEngine(t)
	p := blankPage(t, e)
	ctx := context.Background()
	noise := image.NewRGBA(image.Rect(0, 0, 1000, 800))
	_, _ = rand.Read(noise.Pix)
	for i := 3; i < len(noise.Pix); i += 4 {
		noise.Pix[i] = 255
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, noise); err != nil {
		t.Fatal(err)
	}
	want := buf.Bytes()
	eval := func(expr string) {
		t.Helper()
		if err := p.cmd(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Upload the PNG the way attachments go in, then show it as a result.
	parked := randomKey()
	for off := 0; off < len(want); off += chunkSize {
		if err := p.js(ctx, "putChunk", nil, parked, base64.StdEncoding.EncodeToString(want[off:min(off+chunkSize, len(want))])); err != nil {
			t.Fatal(err)
		}
	}
	eval(`document.body.innerHTML = ` + mustJSON(`<div class="hatch-chat-groupable-bubble bg-chat-user-bubble"><p>draw</p></div>`+
		`<div class="hatch-chat-groupable-bubble hatch-agent-bubble-bg"><div data-hatch-markdown-streaming="false"><p>Here you go</p></div></div>`+
		`<div data-testid="hatch-chat-attachment-presentation-image"><img id="result"></div>`) +
		`; new Promise((ok) => { const img = document.getElementById('result'); img.onload = ok; img.src = URL.createObjectURL(new Blob(window[` + mustJSON(parked) + `], {type: 'image/png'})); })`)
	found, err := p.waitMedia(ctx, mediaJob{Kind: mediaImage}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if found.W != 1000 || found.H != 800 || !strings.HasPrefix(found.Src, "blob:") {
		t.Fatalf("found %+v", found)
	}
	start := time.Now()
	got, err := p.download(ctx, found.Src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("downloaded %d bytes in %s", len(got.Data), time.Since(start).Round(time.Millisecond))
	if got.MIME != "image/png" || !bytes.Equal(got.Data, want) {
		t.Fatalf("download: %s %d bytes, want image/png %d bytes", got.MIME, len(got.Data), len(want))
	}
	// The same image as baseline is not a new result.
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, err = p.waitMedia(short, mediaJob{Kind: mediaImage}, []string{found.Src}, time.Now())
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("baseline image counted as new: %v", err)
	}

	eval(`document.body.insertAdjacentHTML('beforeend', '<div class="hatch-chat-groupable-bubble bg-chat-user-bubble"><p>draw a weapon</p></div>` +
		`<div class="hatch-chat-groupable-bubble hatch-agent-bubble-bg"><div data-hatch-markdown-streaming="false"><p>I can’t help with that.</p></div></div>')`)
	start = time.Now()
	_, err = p.waitMedia(ctx, mediaJob{Kind: mediaImage}, nil, time.Now())
	var textOnly *textOnlyError
	if !errors.As(err, &textOnly) || textOnly.Text != "I can’t help with that." {
		t.Fatalf("text-only reply = %v", err)
	}
	t.Logf("text-only reply detected after %s", time.Since(start).Round(time.Millisecond))

	for banner, limit := range map[string]bool{
		"You have used 80% of your weekly limit.":             false,
		"You've reached your weekly limit. Resets on Oct 31.": true,
		"本周额度已用完":                                             true,
	} {
		eval(`document.getElementById('banner')?.remove(); document.body.insertAdjacentHTML('afterbegin', '<div id="banner">' + ` + mustJSON(banner) + ` + '</div>')`)
		var st chatState
		if err := p.js(ctx, "chat", &st); err != nil {
			t.Fatal(err)
		}
		if st.Limit != limit {
			t.Errorf("banner %q: limit = %v, want %v", banner, st.Limit, limit)
		}
	}
}

// The override must reach the logged-in muse.ai page as one consistent
// desktop Chrome: user agent, navigator.platform and client hints agree.
func TestLiveIdentity(t *testing.T) {
	e := liveEngine(t)
	s := liveSession(t, false)
	for _, key := range []string{"windows-11", "macos-arm"} {
		s.Identity, _ = identityByKey(key)
		var got struct {
			UA, Platform, HintPlatform, Arch, PlatformVersion, Full string
			Brands                                                  []map[string]string
		}
		err := e.withPage(context.Background(), s, "/thread/new", func(p *page) error {
			var r struct {
				Result struct {
					Value json.RawMessage `json:"value"`
				} `json:"result"`
			}
			expr := `(async () => { const h = await navigator.userAgentData.getHighEntropyValues(["architecture","platformVersion","uaFullVersion"]);
				return {UA: navigator.userAgent, Platform: navigator.platform, HintPlatform: navigator.userAgentData.platform, Arch: h.architecture,
					PlatformVersion: h.platformVersion, Full: h.uaFullVersion, Brands: navigator.userAgentData.brands}; })()`
			if err := p.cmd(context.Background(), "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &r); err != nil {
				return err
			}
			return json.Unmarshal(r.Result.Value, &got)
		})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		t.Logf("%s: %+v", key, got)
		if got.UA != s.Identity.userAgent(got.Full) || got.Platform != s.Identity.platform || got.HintPlatform != s.Identity.uaPlatform || got.Arch != s.Identity.architecture || got.PlatformVersion != s.Identity.platformVersion {
			t.Fatalf("%s: page reports %+v", key, got)
		}
		if len(got.Brands) != 3 || got.Brands[0]["brand"] != "Google Chrome" {
			t.Fatalf("%s: brands = %v", key, got.Brands)
		}
	}
}
