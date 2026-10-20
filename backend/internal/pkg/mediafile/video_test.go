package mediafile

import "testing"

func TestVideoExtension(t *testing.T) {
	for _, testCase := range []struct {
		contentType string
		want        string
		ok          bool
	}{
		{contentType: "video/mp4", want: ".mp4", ok: true},
		{contentType: "video/webm; codecs=vp9", want: ".webm", ok: true},
		{contentType: "VIDEO/QUICKTIME", want: ".mov", ok: true},
		{contentType: "application/octet-stream"},
		{contentType: "video/mp4; invalid"},
	} {
		got, ok := VideoExtension(testCase.contentType)
		if got != testCase.want || ok != testCase.ok {
			t.Errorf("VideoExtension(%q) = %q, %v; want %q, %v", testCase.contentType, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestVideoType(t *testing.T) {
	ftyp := func(brands string) []byte {
		box := append([]byte{0, 0, 0, byte(8 + len(brands)), 'f', 't', 'y', 'p'}, brands...)
		return append(box, make([]byte, 32)...)
	}
	// A forged box size must not let the scan reach a brand far past a real box.
	forged := make([]byte, 1<<20)
	copy(forged, "\xff\xff\xff\xffftypzzzz")
	copy(forged[len(forged)-4:], "isom")
	for _, testCase := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "mp4", data: ftyp("isom\x00\x00\x02\x00isomiso2"), want: "video/mp4"},
		{name: "compatible-mp4", data: ftyp("zzzz\x00\x00\x00\x00mp42"), want: "video/mp4"},
		{name: "quicktime", data: ftyp("qt  \x00\x00\x02\x00qt  "), want: "video/quicktime"},
		{name: "heic", data: ftyp("heic\x00\x00\x00\x00mif1heic")},
		{name: "avif", data: ftyp("avif\x00\x00\x00\x00mif1miaf")},
		{name: "webm", data: append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 32)...), want: "video/webm"},
		{name: "png", data: []byte("\x89PNG\r\n\x1a\n0000")},
		{name: "forged-box-size", data: forged},
	} {
		if got := VideoType(testCase.data); got != testCase.want {
			t.Errorf("VideoType(%s) = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestVideoContentDispositionSanitizesName(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		contentType string
		want        string
	}{
		{name: `bad"name; drop`, contentType: "video/mp4", want: `inline; filename="badnamedrop.mp4"`},
		{name: "   ", contentType: "video/mp4", want: `inline; filename="video.mp4"`},
		{name: "video_request_1", contentType: "application/octet-stream", want: `inline; filename="video_request_1"`},
	} {
		if got := VideoContentDisposition(testCase.name, testCase.contentType); got != testCase.want {
			t.Errorf("VideoContentDisposition(%q, %q) = %q, want %q", testCase.name, testCase.contentType, got, testCase.want)
		}
	}
}
