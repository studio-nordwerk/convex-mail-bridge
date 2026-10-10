package edge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

// Gate remembers "throttled" answers of the edge, so that the very same
// attempt repeated within the announced time is answered by the bridge and
// does not cost another call: a mail program with an outdated password
// retries the same one over and over.
//
// It never blocks anything the edge has not blocked itself. The edge only
// throttles attempts that are not valid; a valid device password logs in
// whatever the counters say. So what is remembered is one attempt, address,
// client and password together, never the address or the client alone: a
// second device behind the same connection with the right password is not
// held back by the first one's wrong one.
//
// The password is not kept. The key is an HMAC over the three parts with a
// key drawn at start that never leaves the process.
type Gate struct {
	mu    sync.Mutex
	salt  [32]byte
	until map[[32]byte]time.Time
	now   func() time.Time
}

const (
	gateMaxEntries = 20000
	gateMaxSeconds = 300
)

// NewGate returns an empty gate.
func NewGate() *Gate {
	g := &Gate{until: map[[32]byte]time.Time{}, now: time.Now}
	if _, err := rand.Read(g.salt[:]); err != nil {
		panic(err)
	}
	return g
}

func (g *Gate) key(address, clientIP, password string) [32]byte {
	h := hmac.New(sha256.New, g.salt[:])
	var n [8]byte
	for _, part := range []string{strings.ToLower(strings.TrimSpace(address)), clientIP, password} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Blocked reports whether the edge asked exactly this attempt to wait and
// the time has not passed yet.
func (g *Gate) Blocked(address, clientIP, password string) bool {
	if g == nil {
		return false
	}
	k := g.key(address, clientIP, password)
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.until[k]
	if !ok {
		return false
	}
	if !g.now().Before(t) {
		delete(g.until, k)
		return false
	}
	return true
}

// Block notes a "throttled" answer with its retryAfterSeconds.
func (g *Gate) Block(address, clientIP, password string, seconds int) {
	if g == nil {
		return
	}
	if seconds <= 0 {
		seconds = 5
	}
	if seconds > gateMaxSeconds {
		seconds = gateMaxSeconds
	}
	now := g.now()
	k := g.key(address, clientIP, password)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.until) >= gateMaxEntries {
		for k, t := range g.until {
			if !now.Before(t) {
				delete(g.until, k)
			}
		}
		if len(g.until) >= gateMaxEntries {
			// Too many to remember: forget them. The edge still throttles.
			g.until = map[[32]byte]time.Time{}
		}
	}
	g.until[k] = now.Add(time.Duration(seconds) * time.Second)
}

// RetryAfterOf returns the retryAfterSeconds of a "throttled" answer.
func RetryAfterOf(err error) int {
	if e, ok := err.(*Error); ok {
		return e.RetryAfter
	}
	return 0
}
