// Package logx writes the only log lines the bridge ever produces.
//
// The API is deliberately narrow: an event name, short codes, a mailbox hash
// and numbers. There is no free-form string field, so addresses, subjects,
// message content, passwords and session tokens cannot reach the log by
// accident. Every string that is written is reduced to a small token alphabet
// first.
package logx

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Logger writes one JSON object per line.
type Logger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// New returns a logger writing to w.
func New(w io.Writer) *Logger {
	return &Logger{w: w, now: time.Now}
}

// Field is one key/value pair of a log line.
type Field struct {
	key   string
	str   string
	num   int64
	isNum bool
}

// Code is a short machine-readable code (error code of the edge, reason of a
// rejection). It is sanitized and truncated.
func Code(v string) Field { return Field{key: "code", str: token(v, 40)} }

// MB is the shortened hash of a mailbox (see HashMailbox). Everything that
// is counted or logged per mailbox carries this.
func MB(hash string) Field { return Field{key: "mb", str: token(hash, 16)} }

// Addr is the shortened hash of a login address (see HashAddress). It
// appears where no mailbox is known (failed logins) and once per successful
// login next to MB, so that the two can be told to belong together.
func Addr(hash string) Field { return Field{key: "addr", str: token(hash, 16)} }

// Proto names the protocol ("imap" or "smtp").
func Proto(p string) Field { return Field{key: "proto", str: token(p, 8)} }

// Path names an edge path such as "/raw".
func Path(p string) Field { return Field{key: "path", str: token(p, 24)} }

// Frame is one function name of a stack trace after a panic. Only the name
// is written, never the panic value: that could quote input.
func Frame(i int, fn string) Field {
	return Field{key: "f" + strconv.Itoa(i), str: token(fn, 96)}
}

// N is a named counter or number.
func N(key string, v int64) Field { return Field{key: token(key, 24), num: v, isNum: true} }

// Ms is a duration in milliseconds.
func Ms(d time.Duration) Field { return Field{key: "ms", num: d.Milliseconds(), isNum: true} }

// Event writes one line: time, event name and the given fields.
func (l *Logger) Event(ev string, fields ...Field) {
	if l == nil || l.w == nil {
		return
	}
	var b strings.Builder
	b.Grow(96)
	b.WriteString(`{"t":"`)
	b.WriteString(l.now().UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteString(`","ev":"`)
	b.WriteString(token(ev, 48))
	b.WriteByte('"')
	for _, f := range fields {
		if f.key == "" {
			continue
		}
		b.WriteString(`,"`)
		b.WriteString(f.key)
		b.WriteString(`":`)
		if f.isNum {
			b.WriteString(strconv.FormatInt(f.num, 10))
		} else {
			b.WriteByte('"')
			b.WriteString(f.str)
			b.WriteByte('"')
		}
	}
	b.WriteString("}\n")
	l.mu.Lock()
	_, _ = io.WriteString(l.w, b.String())
	l.mu.Unlock()
}

// token reduces s to [A-Za-z0-9_./:-] and at most max bytes. Everything else
// becomes "_". The result never needs JSON escaping.
func token(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	clean := true
	for i := 0; i < len(s); i++ {
		if !tokenByte(s[i]) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if !tokenByte(c) {
			b[i] = '_'
		}
	}
	return string(b)
}

func tokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '.', c == '/', c == ':', c == '-':
		return true
	}
	return false
}

// HashMailbox returns the shortened SHA-256 of the mailbox identifier the
// edge hands out at login: 12 hex characters, stable across restarts.
func HashMailbox(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

// HashAddress returns the shortened SHA-256 of a login address (lower case,
// trimmed): 12 hex characters. It is stable across restarts so that an
// operator who knows an address can find its lines.
func HashAddress(addr string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(addr))))
	return hex.EncodeToString(sum[:6])
}
