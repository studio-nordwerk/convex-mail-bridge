package edge

import (
	"strconv"
	"testing"
	"time"
)

func TestGate(t *testing.T) {
	g := NewGate()
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }
	if g.Blocked("a@x.test", "192.0.2.1", "old") {
		t.Fatal("blocked without a reason")
	}
	g.Block("A@x.test ", "192.0.2.1", "old", 30)
	if !g.Blocked("a@x.test", "192.0.2.1", "old") {
		t.Fatal("not blocked after a throttled answer")
	}
	if g.Blocked("a@x.test", "192.0.2.2", "old") || g.Blocked("b@x.test", "192.0.2.1", "old") {
		t.Fatal("another client or another address is blocked too")
	}
	// The edge never throttles a valid password. So only the attempt that
	// was throttled is held back, not the address and client as such.
	if g.Blocked("a@x.test", "192.0.2.1", "new") {
		t.Fatal("another password for the same address and client is blocked")
	}
	now = now.Add(31 * time.Second)
	if g.Blocked("a@x.test", "192.0.2.1", "old") {
		t.Fatal("still blocked after the time")
	}
	g.Block("a@x.test", "192.0.2.1", "old", 99999)
	now = now.Add(301 * time.Second)
	if g.Blocked("a@x.test", "192.0.2.1", "old") {
		t.Fatal("an absurd waiting time was taken at face value")
	}
	for i := 0; i < gateMaxEntries+10; i++ {
		g.Block("a@x.test", strconv.Itoa(i), "old", 60)
	}
	if len(g.until) > gateMaxEntries {
		t.Fatalf("gate grew to %d entries", len(g.until))
	}
	var none *Gate
	none.Block("a", "b", "c", 1)
	if none.Blocked("a", "b", "c") {
		t.Fatal("nil gate blocks")
	}
}

// The parts of a key must not run into each other, and two gates must not
// agree on a key (the secret part is drawn per process).
func TestGateKey(t *testing.T) {
	g := NewGate()
	if g.key("a@x.test", "1", "23") == g.key("a@x.test", "12", "3") {
		t.Fatal("key parts run into each other")
	}
	if g.key("a@x.test", "1", "p") == NewGate().key("a@x.test", "1", "p") {
		t.Fatal("two gates derive the same key")
	}
}
