// Package budget bounds how many mail bytes the bridge holds in memory at
// the same time outside the raw cache: batches being written to IMAP
// clients, APPEND literals, SMTP DATA. Without it, memory would grow with
// the number of connections times the largest mail.
//
// One owner (a mailbox) can hold at most a quarter of the budget, so that a
// single account with many slow connections cannot keep everybody else
// waiting.
package budget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrBusy is returned when the bytes did not become available in time.
var ErrBusy = errors.New("budget: no memory available in time")

// Bytes is a counting budget.
type Bytes struct {
	mu      sync.Mutex
	max     int64
	share   int64 // per owner
	used    int64
	byOwner map[string]int64
	waiters map[chan struct{}]struct{}

	waits    atomic.Int64
	timeouts atomic.Int64
}

// New returns a budget of max bytes.
func New(max int64) *Bytes {
	return &Bytes{max: max, share: max / 4, byOwner: map[string]int64{}, waiters: map[chan struct{}]struct{}{}}
}

// Share is the most one owner can hold.
func (b *Bytes) Share() int64 { return b.share }

// Acquire reserves n bytes for owner, waiting until they are free or ctx
// ends. A request larger than one owner's share is treated as the whole
// share, so a single large mail can always pass on its own. The returned
// function gives the bytes back and may be called more than once.
func (b *Bytes) Acquire(ctx context.Context, owner string, n int64) (release func(), err error) {
	if n <= 0 {
		return func() {}, nil
	}
	if n > b.share {
		n = b.share
	}
	waited := false
	for {
		b.mu.Lock()
		if b.used+n <= b.max && b.byOwner[owner]+n <= b.share {
			b.used += n
			b.byOwner[owner] += n
			b.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { b.release(owner, n) }) }, nil
		}
		ch := make(chan struct{})
		b.waiters[ch] = struct{}{}
		b.mu.Unlock()
		if !waited {
			waited = true
			b.waits.Add(1)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			b.mu.Lock()
			delete(b.waiters, ch)
			b.mu.Unlock()
			b.timeouts.Add(1)
			return nil, ErrBusy
		}
	}
}

func (b *Bytes) release(owner string, n int64) {
	b.mu.Lock()
	b.used -= n
	if left := b.byOwner[owner] - n; left > 0 {
		b.byOwner[owner] = left
	} else {
		delete(b.byOwner, owner)
	}
	waiters := b.waiters
	b.waiters = map[chan struct{}]struct{}{}
	b.mu.Unlock()
	for ch := range waiters {
		close(ch)
	}
}

// Stats returns counters for /metrics.
func (b *Bytes) Stats() map[string]int64 {
	b.mu.Lock()
	used, owners, waiting := b.used, int64(len(b.byOwner)), int64(len(b.waiters))
	b.mu.Unlock()
	return map[string]int64{
		"bytes":    used,
		"maxBytes": b.max,
		"owners":   owners,
		"waiting":  waiting,
		"waits":    b.waits.Load(),
		"timeouts": b.timeouts.Load(),
	}
}
