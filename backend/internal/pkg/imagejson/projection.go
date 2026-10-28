package imagejson

import (
	"bytes"
	"encoding/json"
)

// Projection replaces nonempty image strings with a one-byte marker in an inspection copy,
// not from the forwarded bytes. Metadata can precede or follow the image. The
// compact JSON still goes through the normal decoder, and skipped strings must
// have valid JSON escapes too; a truncated/invalid image never becomes success.
type Projection struct {
	inString        bool
	escaped         bool
	unicodeDigits   int
	skipping        bool
	skipValue       bool
	stringStart     int
	lastString      []byte
	invalid         bool
	payloadObserved bool
	skipFilled      bool
}

func (p *Projection) Invalid() bool         { return p.invalid }
func (p *Projection) PayloadObserved() bool { return p.payloadObserved }

func (p *Projection) Append(dst, data []byte) []byte {
	for len(data) > 0 {
		if p.inString && p.skipping && !p.escaped && p.unicodeDigits == 0 {
			// Base64 takes this path: scan in place without copying or retaining
			// any of its bytes. Quotes, escapes and control bytes need validation.
			n := 0
			for n < len(data) && data[n] != '"' && data[n] != '\\' && data[n] >= ' ' {
				n++
			}
			data = data[n:]
			p.payloadObserved = p.payloadObserved || n > 0
			if n > 0 && !p.skipFilled {
				dst = append(dst, 'x')
				p.skipFilled = true
			}
			if len(data) == 0 {
				break
			}
		}
		c := data[0]
		data = data[1:]
		if p.inString {
			if p.skipping && (c != '"' || p.escaped || p.unicodeDigits > 0) {
				p.payloadObserved = true
				if !p.skipFilled {
					dst = append(dst, 'x')
					p.skipFilled = true
				}
			}
			if !p.skipping {
				dst = append(dst, c)
			}
			switch {
			case p.unicodeDigits > 0:
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
					p.invalid = true
				}
				p.unicodeDigits--
			case p.escaped:
				p.escaped = false
				if c == 'u' {
					p.unicodeDigits = 4
				} else if !bytes.ContainsRune([]byte(`"\/bfnrt`), rune(c)) {
					p.invalid = true
				}
			case c == '\\':
				p.escaped = true
			case c == '"':
				p.inString = false
				if p.skipping {
					dst = append(dst, '"')
					p.lastString = nil
				} else {
					p.lastString = dst[p.stringStart:]
				}
			case c < ' ':
				p.invalid = true
			}
			continue
		}
		dst = append(dst, c)
		switch c {
		case '"':
			p.inString, p.skipping = true, p.skipValue
			p.skipFilled = false
			p.stringStart = len(dst) - 1
			p.skipValue = false
		case ':':
			p.skipValue = imagePayloadKey(p.lastString)
			p.lastString = nil
		case ' ', '\t', '\r':
		default:
			p.skipValue = false
			p.lastString = nil
		}
	}
	return dst
}

func imagePayloadKey(raw []byte) bool {
	if bytes.Equal(raw, []byte(`"b64_json"`)) || bytes.Equal(raw, []byte(`"partial_image_b64"`)) {
		return true
	}
	// Escaped JSON member names have the same meaning as their plain forms.
	if len(raw) < 128 && bytes.IndexByte(raw, '\\') >= 0 {
		var key string
		return json.Unmarshal(raw, &key) == nil && (key == "b64_json" || key == "partial_image_b64")
	}
	return false
}
