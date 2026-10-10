package edgestub

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The request checks of the real edge, rebuilt from its rules
// (src/client/edge.ts): a body is a JSON object of at
// most 64 KiB in strict UTF-8, it carries exactly the fields of the contract
// and nothing else, and every field has its type and its range. Anything
// else is 400 "request" (413 "too_large" for the size), before the session
// is even looked at. The stub is deliberately no more forgiving than that:
// a bridge that sends a field too many has to fail its tests.
const (
	maxJSONBytes      = 64 << 10
	maxRows           = 2000
	maxUIDs           = 500
	maxFlags          = 64
	maxFlagChars      = 100
	maxSessions       = 200
	maxAddressChars   = 320
	maxPasswordChars  = 256
	maxMessageIDChars = 1000
	maxMessageIDNorm  = 500
	maxFolderChars    = 64
	maxIPChars        = 64
	maxUID            = 0xffffffff
	maxSafeInteger    = 1<<53 - 1
	sessionTokenChars = 43
)

type body map[string]any

// parseObject reads a body the way the edge does: strict UTF-8, one JSON
// value, and that value an object.
func parseObject(data []byte) (body, bool) {
	if !utf8.Valid(data) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false // something after the value
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return obj, true
}

// onlyKeys: no field besides the allowed ones.
func onlyKeys(b body, allowed ...string) bool {
	for k := range b {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// chars counts the way the edge does (JavaScript string length).
func chars(s string) int { return len(utf16.Encode([]rune(s))) }

// text: a non-empty string of at most max characters.
func text(v any, max int) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" || chars(s) > max {
		return "", false
	}
	return s, true
}

// integer: a whole number within [min, max]. Fractions and exponents are
// refused although the edge would take "1e2": the bridge has no business
// sending them.
func integer(v any, min, max int64) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || i < min || i > max {
		return 0, false
	}
	return i, true
}

func uidList(v any) ([]uint32, bool) {
	list, ok := v.([]any)
	if !ok || len(list) > maxUIDs {
		return nil, false
	}
	out := make([]uint32, 0, len(list))
	for _, e := range list {
		uid, ok := integer(e, 1, maxUID)
		if !ok {
			return nil, false
		}
		out = append(out, uint32(uid))
	}
	return out, true
}

// flagList: missing or null counts as empty.
func flagList(v any) ([]string, bool) {
	if v == nil {
		return []string{}, true
	}
	list, ok := v.([]any)
	if !ok || len(list) > maxFlags {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		f, ok := text(e, maxFlagChars)
		if !ok {
			return nil, false
		}
		out = append(out, f)
	}
	return out, true
}

// tokenShape: 256 bit as base64url, the only form of a session.
func tokenShape(s string) bool {
	if len(s) != sessionTokenChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func parseIPv4(s string) ([]int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return nil, false
	}
	out := make([]int, 0, 4)
	for _, p := range parts {
		if len(p) < 1 || len(p) > 3 {
			return nil, false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return nil, false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n > 255 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func parseIPv6(s string) ([]int, bool) {
	var tail []int
	if i := strings.LastIndexByte(s, ':'); i >= 0 && strings.Contains(s[i+1:], ".") {
		v4, ok := parseIPv4(s[i+1:])
		if !ok {
			return nil, false
		}
		tail = []int{v4[0]<<8 | v4[1], v4[2]<<8 | v4[3]}
		s = s[:i+1]
		if !strings.HasSuffix(s, "::") {
			s = s[:len(s)-1]
		}
	}
	halves := strings.Split(s, "::")
	if len(halves) > 2 {
		return nil, false
	}
	groups := func(part string) ([]int, bool) {
		if part == "" {
			return []int{}, true
		}
		var out []int
		for _, g := range strings.Split(part, ":") {
			if len(g) < 1 || len(g) > 4 {
				return nil, false
			}
			n, err := strconv.ParseUint(g, 16, 16)
			if err != nil || strings.ContainsAny(g, "+-_") {
				return nil, false
			}
			out = append(out, int(n))
		}
		return out, true
	}
	head, ok := groups(halves[0])
	if !ok {
		return nil, false
	}
	rest := []int{}
	if len(halves) == 2 {
		if rest, ok = groups(halves[1]); !ok {
			return nil, false
		}
	}
	known := len(head) + len(rest) + len(tail)
	var out []int
	if len(halves) == 2 {
		if known > 7 {
			return nil, false
		}
		out = append(out, head...)
		for i := known; i < 8; i++ {
			out = append(out, 0)
		}
		out = append(out, rest...)
		out = append(out, tail...)
	} else {
		if known != 8 {
			return nil, false
		}
		out = append(append(out, head...), tail...)
	}
	return out, true
}

// throttleIP returns the client address in the form the throttle counts it:
// IPv4 whole, IPv6 as its /64, IPv4 wrapped in IPv6 as IPv4. Anything that
// is not a bare address (a port, brackets, a zone, a host name) is refused.
func throttleIP(v string) (string, bool) {
	s := jsTrim(v)
	if s == "" || chars(s) > maxIPChars {
		return "", false
	}
	if v4, ok := parseIPv4(s); ok {
		return joinInts(v4, ".", 10), true
	}
	if !strings.Contains(s, ":") {
		return "", false
	}
	g, ok := parseIPv6(s)
	if !ok {
		return "", false
	}
	mapped := g[5] == 0xffff
	for i := 0; i < 5; i++ {
		if g[i] != 0 {
			mapped = false
		}
	}
	if mapped {
		return joinInts([]int{g[6] >> 8, g[6] & 255, g[7] >> 8, g[7] & 255}, ".", 10), true
	}
	return joinInts(g[:4], ":", 16) + "::/64", true
}

func joinInts(in []int, sep string, base int) string {
	parts := make([]string, len(in))
	for i, n := range in {
		parts[i] = strconv.FormatInt(int64(n), base)
	}
	return strings.Join(parts, sep)
}

// normalizeMessageIDStrict is the edge's way to bring a Message-ID to its
// stored form: without angle brackets, trimmed, at most 500 characters.
func normalizeMessageIDStrict(v string) (string, bool) {
	s := jsTrim(v)
	s = strings.TrimLeft(s, "<")
	s = strings.TrimRight(s, ">")
	s = jsTrim(s)
	if s == "" || chars(s) > maxMessageIDNorm {
		return "", false
	}
	return s, true
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// jsTrim is trim() of JavaScript, which the edge uses: its white space is
// not that of strings.TrimSpace (the byte order mark counts, U+0085 does
// not).
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}
