package budget

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBudget(t *testing.T) {
	b := New(400) // 100 per owner
	ctx := context.Background()
	r1, err := b.Acquire(ctx, "a", 60)
	if err != nil {
		t.Fatal(err)
	}
	// the same owner beyond its share: waits, and gives up with the context
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	if _, err := b.Acquire(short, "a", 50); err != ErrBusy {
		t.Fatalf("got %v", err)
	}
	cancel()
	if st := b.Stats(); st["waiting"] != 0 || st["timeouts"] != 1 {
		t.Fatalf("a waiter that gave up is still listed: %v", st)
	}
	// another owner is not held up by the first
	r2, err := b.Acquire(ctx, "b", 100)
	if err != nil {
		t.Fatal(err)
	}
	// fits after a release
	got := make(chan struct{})
	go func() {
		r, err := b.Acquire(ctx, "a", 50)
		if err == nil {
			r()
			close(got)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	r1()
	r1() // twice is harmless
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not woken")
	}
	r2()
	// larger than a share: passes alone, counted as the share
	r, err := b.Acquire(ctx, "c", 100000)
	if err != nil {
		t.Fatal(err)
	}
	if b.Stats()["bytes"] != 100 {
		t.Fatalf("stats = %v", b.Stats())
	}
	r()
	// never over the limits under load
	var wg sync.WaitGroup
	var mu sync.Mutex
	held, peak := int64(0), int64(0)
	perOwner, ownerPeak := map[string]int64{}, int64(0)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(owner string, n int64) {
			defer wg.Done()
			rel, err := b.Acquire(ctx, owner, n)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			held += n
			perOwner[owner] += n
			if held > peak {
				peak = held
			}
			if perOwner[owner] > ownerPeak {
				ownerPeak = perOwner[owner]
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			held -= n
			perOwner[owner] -= n
			mu.Unlock()
			rel()
		}(fmt.Sprint("o", i%7), int64(10+i%40))
	}
	wg.Wait()
	if st := b.Stats(); peak > 400 || ownerPeak > 100 || st["bytes"] != 0 || st["owners"] != 0 {
		t.Fatalf("peak %d, owner peak %d, left %v", peak, ownerPeak, st)
	}
}
