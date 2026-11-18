// Hand-rolled JSON helpers for the hot paths. Everything appends into a
// caller-owned buffer; no encoding/json anywhere in request handling.
package main

import (
	"strconv"
	"unicode/utf8"
)

var hexDigits = "0123456789abcdef"

// appendJSONString appends s as a JSON string literal, with a fast path
// when no byte needs escaping.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			dst = append(dst, s[start:i]...)
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			start = i + 1
		}
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
}

// needsEscape reports whether s contains any byte JSON requires escaping.
func needsEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' {
			return true
		}
	}
	return false
}

func appendInt(dst []byte, n int) []byte {
	return strconv.AppendInt(dst, int64(n), 10)
}

func appendInt64(dst []byte, n int64) []byte {
	return strconv.AppendInt(dst, n, 10)
}

func appendBool(dst []byte, b bool) []byte {
	if b {
		return append(dst, "true"...)
	}
	return append(dst, "false"...)
}

// jsonError renders {"message": "..."}.
func jsonError(msg string) []byte {
	b := make([]byte, 0, len(msg)+16)
	b = append(b, `{"message":`...)
	b = appendJSONString(b, msg)
	b = append(b, '}')
	return b
}

// --- minimal JSON value parser (for request bodies) -----------------------

// jval is a tiny parsed JSON value: object, array, string, number, bool, null.
type jval struct {
	kind byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	str  string
	num  float64
	obj  map[string]*jval
	arr  []*jval
}

type jparser struct {
	b []byte
	i int
}

func parseJSON(b []byte) (*jval, bool) {
	p := &jparser{b: b}
	p.skipWS()
	v, ok := p.value()
	if !ok {
		return nil, false
	}
	p.skipWS()
	if p.i != len(p.b) {
		return nil, false
	}
	return v, true
}

func (p *jparser) skipWS() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() (*jval, bool) {
	if p.i >= len(p.b) {
		return nil, false
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, ok := p.string()
		if !ok {
			return nil, false
		}
		return &jval{kind: 's', str: s}, true
	case c == 't':
		if p.lit("true") {
			return &jval{kind: 'b', num: 1}, true
		}
		return nil, false
	case c == 'f':
		if p.lit("false") {
			return &jval{kind: 'b', num: 0}, true
		}
		return nil, false
	case c == 'n':
		if p.lit("null") {
			return &jval{kind: 'z'}, true
		}
		return nil, false
	default:
		return p.number()
	}
}

func (p *jparser) lit(s string) bool {
	if len(p.b)-p.i < len(s) || string(p.b[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

func (p *jparser) object() (*jval, bool) {
	v := &jval{kind: 'o', obj: make(map[string]*jval)}
	p.i++ // {
	p.skipWS()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return v, true
	}
	for {
		p.skipWS()
		key, ok := p.string()
		if !ok {
			return nil, false
		}
		p.skipWS()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, false
		}
		p.i++
		p.skipWS()
		val, ok := p.value()
		if !ok {
			return nil, false
		}
		v.obj[key] = val
		p.skipWS()
		if p.i >= len(p.b) {
			return nil, false
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return v, true
		default:
			return nil, false
		}
	}
}

func (p *jparser) array() (*jval, bool) {
	v := &jval{kind: 'a'}
	p.i++ // [
	p.skipWS()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return v, true
	}
	for {
		p.skipWS()
		val, ok := p.value()
		if !ok {
			return nil, false
		}
		v.arr = append(v.arr, val)
		p.skipWS()
		if p.i >= len(p.b) {
			return nil, false
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return v, true
		default:
			return nil, false
		}
	}
}

func (p *jparser) string() (string, bool) {
	if p.i >= len(p.b) || p.b[p.i] != '"' {
		return "", false
	}
	p.i++
	start := p.i
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			s := string(p.b[start:p.i])
			p.i++
			return s, true
		}
		if c == '\\' {
			// Slow path: rebuild with escapes resolved.
			return p.stringEscaped(start)
		}
		if c < 0x20 {
			return "", false
		}
		p.i++
	}
	return "", false
}

func (p *jparser) stringEscaped(start int) (string, bool) {
	out := make([]byte, 0, 32)
	out = append(out, p.b[start:p.i]...)
	for p.i < len(p.b) {
		c := p.b[p.i]
		if c == '"' {
			p.i++
			return string(out), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.b) {
				return "", false
			}
			switch e := p.b[p.i]; e {
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			case '/':
				out = append(out, '/')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				if p.i+4 >= len(p.b) {
					return "", false
				}
				r := parseHex4(p.b[p.i+1 : p.i+5])
				p.i += 4
				out = utf8.AppendRune(out, r)
			default:
				return "", false
			}
			p.i++
			continue
		}
		if c < 0x20 {
			return "", false
		}
		out = append(out, c)
		p.i++
	}
	return "", false
}

func parseHex4(b []byte) rune {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return utf8.RuneError
		}
	}
	return r
}

func (p *jparser) number() (*jval, bool) {
	start := p.i
	if p.i < len(p.b) && p.b[p.i] == '-' {
		p.i++
	}
	digits := 0
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
		digits++
	}
	if digits == 0 {
		return nil, false
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil {
		return nil, false
	}
	return &jval{kind: 'n', num: f}, true
}

// Accessors: missing keys yield zero values, never a panic.
func (v *jval) get(key string) *jval {
	if v == nil || v.kind != 'o' {
		return nil
	}
	return v.obj[key]
}

func (v *jval) strOr(def string) string {
	if v == nil || v.kind != 's' {
		return def
	}
	return v.str
}

func (v *jval) boolOr(def bool) bool {
	if v == nil || v.kind != 'b' {
		return def
	}
	return v.num != 0
}

func (v *jval) intOr(def int) int {
	if v == nil || v.kind != 'n' {
		return def
	}
	return int(v.num)
}

func (v *jval) isNull() bool { return v == nil || v.kind == 'z' }
