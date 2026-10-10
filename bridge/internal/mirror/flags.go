package mirror

import (
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
)

// Flag sets are interned: most mailboxes use a handful of combinations, so
// rows share one immutable slice per combination. Never modify a returned
// slice.

var (
	internMu  sync.Mutex
	internMap = map[string][]imap.Flag{}
)

const maxInterned = 8192

// systemFlags maps lower case to canonical spelling.
var systemFlags = map[string]imap.Flag{
	`\seen`:     imap.FlagSeen,
	`\answered`: imap.FlagAnswered,
	`\flagged`:  imap.FlagFlagged,
	`\deleted`:  imap.FlagDeleted,
	`\draft`:    imap.FlagDraft,
}

// ValidFlag reports whether s can be written as an IMAP flag: an optional
// leading backslash and atom characters, no longer than the edge takes. \Recent and \* are not stored.
func ValidFlag(s string) bool {
	if s == "" || len(s) > edge.MaxFlagChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			if i != 0 {
				return false
			}
			continue
		}
		if !atomChar(c) {
			return false
		}
	}
	if s[0] == '\\' {
		if len(s) == 1 {
			return false
		}
		if _, ok := systemFlags[strings.ToLower(s)]; !ok {
			return false
		}
	}
	return true
}

func atomChar(c byte) bool {
	switch c {
	case '(', ')', '{', ' ', '%', '*', '"', '\\', ']':
		return false
	}
	return c > 0x20 && c < 0x7f
}

// CanonFlag returns the canonical spelling of a system flag and the flag
// unchanged otherwise.
func CanonFlag(s string) string {
	if len(s) > 0 && s[0] == '\\' {
		if f, ok := systemFlags[strings.ToLower(s)]; ok {
			return string(f)
		}
	}
	return s
}

// internFlags normalizes flags from the edge (valid ones only, canonical,
// sorted, unique) and returns the shared slice for that combination.
// dropped counts flags that could not be written to a client.
func internFlags(in []string) (out []imap.Flag, dropped int) {
	clean := make([]string, 0, len(in))
	for _, f := range in {
		if !ValidFlag(f) {
			dropped++
			continue
		}
		clean = append(clean, CanonFlag(f))
	}
	sort.Strings(clean)
	uniq := clean[:0]
	for i, f := range clean {
		if i == 0 || f != clean[i-1] {
			uniq = append(uniq, f)
		}
	}
	k := strings.Join(uniq, " ")
	internMu.Lock()
	defer internMu.Unlock()
	if s, ok := internMap[k]; ok {
		return s, dropped
	}
	s := make([]imap.Flag, len(uniq))
	for i, f := range uniq {
		s[i] = imap.Flag(f)
	}
	if len(internMap) < maxInterned {
		internMap[k] = s
	}
	return s, dropped
}

func sameFlags(a, b []imap.Flag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// HasFlag reports whether flags contains f (exact canonical match; system
// flags are canonical in the mirror, keywords compare case-insensitively as
// IMAP requires).
func HasFlag(flags []imap.Flag, f imap.Flag) bool {
	for _, x := range flags {
		if x == f || strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}
