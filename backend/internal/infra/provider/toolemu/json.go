package toolemu

import "strings"

// jsonScan follows one JSON value through text with the grammar of
// encoding/json. It keeps its place, so a value that streams in is read once
// however often it is checked.
type jsonScan struct {
	off   int    // next byte to read
	stack []byte // open containers, '{' or '['
	step  byte   // what the byte at off may be
	key   bool   // the string being read is an object key
	rest  string // the rest of a literal: true, false or null
	hex   int    // hex digits left in a \u escape
}

// Results of jsonScan.scan other than the end of the value.
const (
	jsonOpen = -1 // the value goes on past the end of text
	jsonBad  = -2 // text is not JSON
)

// maxJSONDepth is the nesting limit of encoding/json.
const maxJSONDepth = 10000

const (
	jsValue    byte = iota // a value
	jsElement              // a value or ']', after '['
	jsMember               // a key or '}', after '{'
	jsKey                  // a key, after ','
	jsColon                // ':', after a key
	jsNext                 // ',' or the closing bracket, after a value in a container
	jsString               // in a string
	jsEscape               // after '\' in a string
	jsHex                  // in the digits of \u
	jsLiteral              // in true, false or null
	jsMinus                // after '-'
	jsZero                 // after a leading 0
	jsInt                  // in integer digits
	jsDot                  // after '.'
	jsFrac                 // in fraction digits
	jsExp                  // after 'e'
	jsExpSign              // after the sign of the exponent
	jsExpDigit             // in exponent digits
)

// scan reads on from the last call and returns the end of the value,
// jsonOpen or jsonBad. Unless final, a number at the end of text stays open,
// as more digits may follow. Each call passes text that extends the text of
// the last one.
func (j *jsonScan) scan(text string, final bool) int {
	for ; j.off < len(text); j.off++ {
		c := text[j.off]
		switch j.step {
		case jsValue, jsElement:
			switch {
			case isSpace(c):
			case c == ']' && j.step == jsElement:
				if j.close() {
					return j.off + 1
				}
			case !j.begin(c):
				return jsonBad
			}
		case jsMember, jsKey:
			switch {
			case isSpace(c):
			case c == '}' && j.step == jsMember:
				if j.close() {
					return j.off + 1
				}
			case c == '"':
				j.step, j.key = jsString, true
			default:
				return jsonBad
			}
		case jsColon:
			switch {
			case isSpace(c):
			case c == ':':
				j.step = jsValue
			default:
				return jsonBad
			}
		case jsNext:
			open := j.stack[len(j.stack)-1]
			switch {
			case isSpace(c):
			case c == ',' && open == '{':
				j.step = jsKey
			case c == ',':
				j.step = jsValue
			case c == '}' && open == '{', c == ']' && open == '[':
				if j.close() {
					return j.off + 1
				}
			default:
				return jsonBad
			}
		case jsString:
			switch {
			case c == '"' && j.key:
				j.step = jsColon
			case c == '"':
				if j.end() {
					return j.off + 1
				}
			case c == '\\':
				j.step = jsEscape
			case c < 0x20:
				return jsonBad
			}
		case jsEscape:
			switch {
			case c == 'u':
				j.step, j.hex = jsHex, 4
			case strings.IndexByte(`"\/bfnrt`, c) >= 0:
				j.step = jsString
			default:
				return jsonBad
			}
		case jsHex:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return jsonBad
			}
			if j.hex--; j.hex == 0 {
				j.step = jsString
			}
		case jsLiteral:
			if c != j.rest[0] {
				return jsonBad
			}
			if j.rest = j.rest[1:]; j.rest == "" && j.end() {
				return j.off + 1
			}
		default:
			if next := numberNext(j.step, c); next != jsValue {
				j.step = next
				continue
			}
			if !numberEnds(j.step) {
				return jsonBad
			}
			if j.end() {
				return j.off // the number ends before c
			}
			j.off-- // read c again, after the number
		}
	}
	if final && len(j.stack) == 0 && numberEnds(j.step) {
		return len(text)
	}
	return jsonOpen
}

// begin starts the value that c opens.
func (j *jsonScan) begin(c byte) bool {
	switch {
	case c == '{' || c == '[':
		if len(j.stack) == maxJSONDepth {
			return false
		}
		j.stack = append(j.stack, c)
		j.step = jsMember
		if c == '[' {
			j.step = jsElement
		}
	case c == '"':
		j.step, j.key = jsString, false
	case c == '-':
		j.step = jsMinus
	case c == '0':
		j.step = jsZero
	case '1' <= c && c <= '9':
		j.step = jsInt
	case c == 't':
		j.step, j.rest = jsLiteral, "rue"
	case c == 'f':
		j.step, j.rest = jsLiteral, "alse"
	case c == 'n':
		j.step, j.rest = jsLiteral, "ull"
	default:
		return false
	}
	return true
}

// end finishes a value and reports whether it was the outermost one.
func (j *jsonScan) end() bool {
	if len(j.stack) == 0 {
		return true
	}
	j.step = jsNext
	return false
}

// close finishes the innermost container.
func (j *jsonScan) close() bool {
	j.stack = j.stack[:len(j.stack)-1]
	return j.end()
}

// numberNext is the step after c in a number, or jsValue when c does not
// continue it.
func numberNext(step, c byte) byte {
	digit := '0' <= c && c <= '9'
	switch {
	case step == jsMinus && c == '0':
		return jsZero
	case digit && (step == jsMinus || step == jsInt):
		return jsInt
	case c == '.' && (step == jsZero || step == jsInt):
		return jsDot
	case digit && (step == jsDot || step == jsFrac):
		return jsFrac
	case (c == 'e' || c == 'E') && (step == jsZero || step == jsInt || step == jsFrac):
		return jsExp
	case (c == '+' || c == '-') && step == jsExp:
		return jsExpSign
	case digit && (step == jsExp || step == jsExpSign || step == jsExpDigit):
		return jsExpDigit
	}
	return jsValue
}

// numberEnds reports a number that is complete after step.
func numberEnds(step byte) bool {
	return step == jsZero || step == jsInt || step == jsFrac || step == jsExpDigit
}
