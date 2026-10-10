// Package rawcache keeps recently fetched raw mails in memory, bounded by a
// fixed number of bytes (least recently used entries go first).
//
// A key always names mailbox, folder, uidValidity and uid. Callers must only
// build keys for the mailbox their connection logged in to; the cache itself
// cannot know who asks.
package rawcache

import (
	"container/list"
	"sync"
)

// Key identifies one raw mail.
type Key struct {
	Mailbox     string // opaque id of the mailbox as the edge names it (/login)
	Folder      string
	UIDValidity uint32
	UID         uint32
}

type entry struct {
	key Key
	buf []byte
}

// Cache is a byte-bounded LRU.
type Cache struct {
	mu      sync.Mutex
	max     int64
	size    int64
	ll      *list.List
	items   map[Key]*list.Element
	hits    int64
	misses  int64
	evicted int64
}

// New returns a cache holding at most max bytes.
func New(max int64) *Cache {
	return &Cache{max: max, ll: list.New(), items: map[Key]*list.Element{}}
}

// Get returns the bytes for k. The slice must not be modified.
func (c *Cache) Get(k Key) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		c.hits++
		return e.Value.(*entry).buf, true
	}
	c.misses++
	return nil, false
}

// Put stores buf under k. Entries larger than the whole cache are not kept.
func (c *Cache) Put(k Key, buf []byte) {
	n := int64(len(buf))
	if n > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return
	}
	c.items[k] = c.ll.PushFront(&entry{key: k, buf: buf})
	c.size += n
	for c.size > c.max {
		c.removeLocked(c.ll.Back())
		c.evicted++
	}
}

func (c *Cache) removeLocked(e *list.Element) {
	en := e.Value.(*entry)
	c.ll.Remove(e)
	delete(c.items, en.key)
	c.size -= int64(len(en.buf))
}

// DropFolder removes all entries of one folder of one mailbox (after the
// folder went backwards or changed its uidValidity).
func (c *Cache) DropFolder(mailbox, folder string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if k.Mailbox == mailbox && k.Folder == folder {
			c.removeLocked(e)
		}
	}
}

// DropMailbox removes all entries of one mailbox.
func (c *Cache) DropMailbox(mailbox string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if k.Mailbox == mailbox {
			c.removeLocked(e)
		}
	}
}

// Stats returns counters for /metrics.
func (c *Cache) Stats() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]int64{
		"entries":   int64(c.ll.Len()),
		"bytes":     c.size,
		"maxBytes":  c.max,
		"hits":      c.hits,
		"misses":    c.misses,
		"evictions": c.evicted,
	}
}
