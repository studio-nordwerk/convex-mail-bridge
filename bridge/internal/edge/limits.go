package edge

import (
	"strings"
	"unicode/utf8"
)

// Limits of the contract.
const (
	MaxEntriesLimit = 2000 // rows per /entries and /changes page
	MaxStoreUIDs    = 500  // uids per /store and /expunge call
	MaxSeqSessions  = 200  // sessions per /seq call

	// MaxRequestBytes: the edge answers a larger JSON body with 413.
	MaxRequestBytes = 64 << 10
)

// Limits the edge applies to single fields. They are not in the contract
// but in the checks of the edge (src/client/edge.ts,
// BRIDGE_LIMITS); what lies outside is answered with 400 there. The bridge
// knows them so that it turns such input away itself, with the answer that
// fits the mail program, instead of producing a 400 that looks like a fault
// between bridge and edge. If the edge changes them, change them here.
const (
	MaxAddressChars  = 320 // /login address
	MaxPasswordChars = 256 // /login password
	MaxFlagsPerList  = 64  // /store add, remove
	MaxFlagChars     = 100 // one flag
	// /append messageId: as sent, and without angle brackets
	MaxMessageIDChars     = 1000
	MaxMessageIDNormChars = 500

	// MaxSessionChars bounds a session as the bridge accepts it from
	// /login (the edge issues 43 characters). 200 of them in one /seq body
	// stay under MaxRequestBytes: 200 * (256 + 3) + 15 bytes.
	MaxSessionChars = 256
)

// utf16Len counts the way the edge counts characters (JavaScript string
// length), for a string as it arrives there: invalid bytes have become one
// replacement character each.
func utf16Len(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// LoginInput reports whether address and password are something the edge
// accepts as a login attempt at all. Anything else cannot be a valid login;
// the bridge answers it as a failed one without asking.
func LoginInput(address, password string) bool {
	// The edge trims the address and wants something left. What counts as
	// white space differs between here and there, so ask for more than
	// that: at least one visible ASCII character, which every address has.
	visible := false
	for i := 0; i < len(address); i++ {
		if address[i] > 0x20 && address[i] < 0x7f {
			visible = true
			break
		}
	}
	return visible && utf16Len(address) <= MaxAddressChars &&
		password != "" && utf16Len(password) <= MaxPasswordChars
}

// AppendableMessageID reports whether a Message-ID (as it stands in the
// header, with or without angle brackets) is one the edge can look up.
func AppendableMessageID(id string) bool {
	if id == "" || utf16Len(id) > MaxMessageIDChars {
		return false
	}
	norm := jsTrim(id)
	norm = strings.TrimLeft(norm, "<")
	norm = strings.TrimRight(norm, ">")
	norm = jsTrim(norm)
	return norm != "" && utf16Len(norm) <= MaxMessageIDNormChars
}

// jsSpace reports what the edge's trim() takes away (white space and line
// ends of JavaScript). That is not the set of strings.TrimSpace: the byte
// order mark belongs to it, U+0085 does not.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }
