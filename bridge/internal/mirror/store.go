// Package mirror keeps, per mailbox and folder, the numbered rows of the
// edge in memory (uid, flags, size, date) and feeds changes to the IMAP
// connections that have the folder open.
//
// Access rule: a connection reaches mirror data and the raw cache only
// through the Member it got from Attach after its own successful login, and
// a Member only reaches its own mailbox. Before mirror data is handed out,
// the Member's session has to have been confirmed by the edge within the
// freshness window.
package mirror

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/rawcache"
)

// Errors returned to the protocol layer.
var (
	ErrSession     = errors.New("mirror: session no longer valid")
	ErrUnavailable = errors.New("mirror: edge unavailable")
	ErrBudget      = errors.New("mirror: row budget exhausted")
	ErrGone        = errors.New("mirror: mail is gone")
	ErrViewDead    = errors.New("mirror: folder was reset")
)

// Options configures a Store.
type Options struct {
	Edge           *edge.Client
	Raw            *rawcache.Cache
	Metrics        *metrics.Registry
	Log            *logx.Logger
	MaxRows        int64         // all mirrors together
	PageSize       int           // /entries and /changes limit, <= 2000
	Freshness      time.Duration // how long a session confirmation counts
	RawConcurrency int           // parallel /raw calls in total
	SyncWorkers    int           // parallel mailbox syncs of the poller
	MaxDeltaRows   int           // larger deltas are replaced by a full reload
	MaxQueued      int64         // updates waiting in all connections together
}

// Store holds all mirrors.
type Store struct {
	o      Options
	edge   *edge.Client
	raw    *rawcache.Cache
	m      *metrics.Registry
	log    *logx.Logger
	rawSem chan struct{}

	mu       sync.Mutex
	boxes    map[string]*Mailbox
	clock    int64
	reserved int64 // rows promised to loads that are still running

	queued atomic.Int64 // updates waiting in the queues of all views

	pollMu sync.Mutex // one poller pass at a time
}

// New returns an empty store.
func New(o Options) *Store {
	if o.PageSize <= 0 || o.PageSize > edge.MaxEntriesLimit {
		o.PageSize = edge.MaxEntriesLimit
	}
	if o.Freshness <= 0 {
		o.Freshness = 10 * time.Second
	}
	if o.RawConcurrency <= 0 {
		o.RawConcurrency = 6
	}
	if o.SyncWorkers <= 0 {
		o.SyncWorkers = 8
	}
	if o.MaxDeltaRows <= 0 {
		o.MaxDeltaRows = 20000
	}
	if o.MaxRows <= 0 {
		o.MaxRows = 400000
	}
	if o.MaxQueued <= 0 {
		o.MaxQueued = 300000 // about 30 MB of queue entries
	}
	st := &Store{
		o: o, edge: o.Edge, raw: o.Raw, m: o.Metrics, log: o.Log,
		rawSem: make(chan struct{}, o.RawConcurrency),
		boxes:  map[string]*Mailbox{},
	}
	if st.m != nil {
		st.m.Gauge("mirror", st.gauges)
	}
	return st
}

func (st *Store) gauges() map[string]int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	var rows, members int64
	for _, b := range st.boxes {
		rows += b.rows.Load()
		members += b.refs.Load()
	}
	return map[string]int64{
		"mailboxes": int64(len(st.boxes)),
		"rows":      rows,
		"reserved":  st.reserved,
		"queued":    st.queued.Load(),
		"maxQueued": st.o.MaxQueued,
		"maxRows":   st.o.MaxRows,
		"members":   members,
	}
}

// Mailbox is the shared state of one mailbox, by the identifier the edge
// hands out at login. Two addresses of one mailbox share it; one address
// that later names another mailbox does not see it.
type Mailbox struct {
	st   *Store
	key  string
	hash string

	refs atomic.Int64 // attached members; pins the mailbox against eviction
	used int64        // store clock at last detach (guarded by st.mu)
	rows atomic.Int64 // rows held by its folders

	folders [2]*Folder

	mu       sync.Mutex
	members  map[*Member]struct{}
	lastSeq  int64
	seqKnown bool

	syncing atomic.Bool
}

// Folder returns the folder with the given canonical name, or nil.
func (b *Mailbox) folder(name string) *Folder {
	switch name {
	case edge.FolderInbox:
		return b.folders[0]
	case edge.FolderSent:
		return b.folders[1]
	}
	return nil
}

// Member is one logged-in connection's handle on its mailbox.
type Member struct {
	box       *Mailbox
	token     string
	expiresAt time.Time
	kill      func(code string)

	validatedAt atomic.Int64 // unix nanoseconds of the last confirmation
	killed      atomic.Bool
	detached    atomic.Bool
}

// Attach registers a connection that has just logged in and was told by the
// edge that its session belongs to the mailbox with this identifier. kill
// is called (once, asynchronously safe) when the edge no longer accepts the
// session.
func (st *Store) Attach(mailbox, token string, expiresAt time.Time, kill func(code string)) *Member {
	key := mailbox // opaque: used as it is
	st.mu.Lock()
	b := st.boxes[key]
	if b == nil {
		b = &Mailbox{st: st, key: key, hash: logx.HashMailbox(key), members: map[*Member]struct{}{}}
		b.folders[0] = newFolder(b, edge.FolderInbox)
		b.folders[1] = newFolder(b, edge.FolderSent)
		st.boxes[key] = b
	}
	b.refs.Add(1)
	st.mu.Unlock()

	m := &Member{box: b, token: token, expiresAt: expiresAt, kill: kill}
	m.validatedAt.Store(time.Now().UnixNano())
	b.mu.Lock()
	b.members[m] = struct{}{}
	b.mu.Unlock()
	return m
}

// Detach ends the membership. The mirror stays (LRU) for the next login.
func (m *Member) Detach() {
	if m.detached.Swap(true) {
		return
	}
	b := m.box
	b.mu.Lock()
	delete(b.members, m)
	b.mu.Unlock()
	st := b.st
	st.mu.Lock()
	b.refs.Add(-1)
	st.clock++
	b.used = st.clock
	st.mu.Unlock()
}

// Hash is the shortened hash of the mailbox identifier.
func (m *Member) Hash() string { return m.box.hash }

// Caller is the edge identity of this member.
func (m *Member) Caller() edge.Caller { return edge.Caller{Session: m.token, MB: m.box.hash} }

// ExpiresAt is the local end of the session.
func (m *Member) ExpiresAt() time.Time { return m.expiresAt }

// Folder returns the mirror of one of the two folders of the member's own
// mailbox, or nil for any other name.
func (m *Member) Folder(name string) *Folder { return m.box.folder(name) }

// Kill ends the connection behind this member.
func (m *Member) Kill(code string) {
	if m.killed.Swap(true) {
		return
	}
	if m.kill != nil {
		m.kill(code)
	}
}

// Killed reports whether the member was ended.
func (m *Member) Killed() bool { return m.killed.Load() }

func (m *Member) confirm() { m.validatedAt.Store(time.Now().UnixNano()) }

// IsFresh reports whether the session was confirmed recently (no call).
func (m *Member) IsFresh() bool {
	if m.killed.Load() {
		return false
	}
	if !m.expiresAt.IsZero() && !time.Now().Before(m.expiresAt) {
		return false
	}
	return time.Since(time.Unix(0, m.validatedAt.Load())) <= m.box.st.o.Freshness
}

// EnsureFresh makes sure the session was confirmed within the freshness
// window, asking the edge if needed.
func (m *Member) EnsureFresh(ctx context.Context) error {
	if m.killed.Load() {
		return ErrSession
	}
	if !m.expiresAt.IsZero() && !time.Now().Before(m.expiresAt) {
		m.Kill("expired")
		return ErrSession
	}
	if m.IsFresh() {
		return nil
	}
	res, err := m.box.st.edge.Seq(ctx, []string{m.token})
	if err != nil {
		return ErrUnavailable
	}
	if !res[0].OK {
		m.Kill("session")
		return ErrSession
	}
	m.confirm()
	return nil
}

// check maps an edge error of a call made with this member's session.
func (m *Member) check(err error) error {
	if err == nil {
		m.confirm()
		return nil
	}
	if edge.IsSession(err) {
		m.Kill("session")
		return ErrSession
	}
	return err
}

// Raw returns the bytes of a mail from the cache or the edge. uidValidity
// is the one the caller's view was built on; the edge checks it. If the
// folder has been renumbered, the mirror of the folder is dropped, the
// connections that have it open are closed, and nothing is kept.
func (m *Member) Raw(ctx context.Context, f *Folder, uidValidity, uid uint32, size int64) ([]byte, error) {
	if f == nil || f.box != m.box {
		return nil, ErrGone
	}
	if m.killed.Load() {
		return nil, ErrSession // also for what is already in memory
	}
	st := m.box.st
	key := rawcache.Key{Mailbox: m.box.key, Folder: f.Name, UIDValidity: uidValidity, UID: uid}
	if b, ok := st.raw.Get(key); ok {
		return b, nil
	}
	select {
	case st.rawSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ErrUnavailable
	}
	// What the cache of this folder looked like when the mail was asked for:
	// if it is thrown away while the answer is on its way, the answer is
	// not kept.
	gen := f.rawGeneration()
	b, err := st.edge.Raw(ctx, m.Caller(), f.Name, uidValidity, uid)
	<-st.rawSem
	if edge.IsUIDValidity(err) {
		m.confirm() // the edge did accept the session
		f.renumbered(uidValidity)
		return nil, ErrViewDead
	}
	if err = m.check(err); err != nil {
		if err == ErrSession {
			return nil, err
		}
		if edge.Is(err, 404, "gone") {
			return nil, ErrGone
		}
		return nil, ErrUnavailable
	}
	if st.m != nil {
		st.m.Mailbox(m.box.hash, func(mb *metrics.Mailbox) {
			mb.RawFetches++
			mb.RawBytes += int64(len(b))
		})
		if size >= 0 && int64(len(b)) != size {
			st.m.RawSizeMismatch.Add(1) // the edge counts the mail differently
		}
	}
	// The mirror may have moved on while the mail was fetched (another
	// connection noticed a new numbering, or the folder was read again
	// because its state did not fit): then this is not kept.
	if !f.keepRaw(gen, key, b) && f.UIDValidity() != uidValidity {
		return nil, ErrViewDead
	}
	return b, nil
}

// reserve sets n more rows aside, evicting mirrors of mailboxes without
// connections (least recently used first) to make room. It reports whether
// they fit. What was reserved has to be given back with unreserve once the
// rows are installed (and counted by their mailbox) or the load failed;
// until then parallel loads see the room as taken.
func (st *Store) reserve(n int64) bool {
	if n <= 0 {
		return true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	total := st.reserved
	for _, b := range st.boxes {
		total += b.rows.Load()
	}
	if total+n <= st.o.MaxRows {
		st.reserved += n
		return true
	}
	idle := make([]*Mailbox, 0)
	for _, b := range st.boxes {
		if b.refs.Load() == 0 {
			idle = append(idle, b)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].used < idle[j].used })
	for _, b := range idle {
		if total+n <= st.o.MaxRows {
			break
		}
		total -= b.rows.Load()
		delete(st.boxes, b.key)
		st.raw.DropMailbox(b.key)
		if st.m != nil {
			st.m.MirrorEvictions.Add(1)
		}
	}
	if total+n > st.o.MaxRows {
		return false
	}
	st.reserved += n
	return true
}

func (st *Store) unreserve(n int64) {
	if n <= 0 {
		return
	}
	st.mu.Lock()
	st.reserved -= n
	st.mu.Unlock()
}

// ---- poller --------------------------------------------------------------

// HasMembers reports whether any connection is logged in.
func (st *Store) HasMembers() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, b := range st.boxes {
		if b.refs.Load() > 0 {
			return true
		}
	}
	return false
}

// PollOnce asks the edge for the change counters of all logged-in
// connections (one call per 200 sessions), ends connections whose session
// is no longer valid, and brings open folders of changed mailboxes up to
// date. It returns after all syncs finished.
func (st *Store) PollOnce(ctx context.Context) {
	st.pollMu.Lock()
	defer st.pollMu.Unlock()

	st.mu.Lock()
	var members []*Member
	for _, b := range st.boxes {
		if b.refs.Load() == 0 {
			continue
		}
		b.mu.Lock()
		for m := range b.members {
			if !m.killed.Load() {
				members = append(members, m)
			}
		}
		b.mu.Unlock()
	}
	st.mu.Unlock()
	if len(members) == 0 {
		return
	}
	if st.m != nil {
		st.m.PollerRuns.Add(1)
	}

	now := time.Now()
	type change struct {
		box *Mailbox
		by  *Member
		seq int64
	}
	changed := map[*Mailbox]*change{}
	for i := 0; i < len(members); i += edge.MaxSeqSessions {
		j := i + edge.MaxSeqSessions
		if j > len(members) {
			j = len(members)
		}
		chunk := members[i:j]
		tokens := make([]string, len(chunk))
		for k, m := range chunk {
			tokens[k] = m.token
		}
		res, err := st.edge.Seq(ctx, tokens)
		if err != nil {
			if st.m != nil {
				st.m.PollerErrors.Add(1)
			}
			st.log.Event("poll_failed", logx.Code(edge.CodeOf(err)))
			continue
		}
		for k, m := range chunk {
			if !m.expiresAt.IsZero() && !now.Before(m.expiresAt) {
				m.Kill("expired")
				continue
			}
			if !res[k].OK {
				m.Kill("session")
				continue
			}
			m.confirm()
			b := m.box
			if c := changed[b]; c == nil {
				changed[b] = &change{box: b, by: m, seq: res[k].Seq}
			}
		}
	}

	sem := make(chan struct{}, st.o.SyncWorkers)
	var wg sync.WaitGroup
	for _, c := range changed {
		b := c.box
		b.mu.Lock()
		same := b.seqKnown && b.lastSeq == c.seq
		b.mu.Unlock()
		if same {
			continue
		}
		if !b.syncing.CompareAndSwap(false, true) {
			continue // the previous sync of this mailbox still runs
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c *change) {
			defer wg.Done()
			defer func() { <-sem }()
			defer c.box.syncing.Store(false)
			defer func() {
				if v := recover(); v != nil {
					if st.m != nil {
						st.m.PollerPanics.Add(1)
					}
					st.log.Event("panic", logx.Proto("poller"), logx.MB(c.box.hash))
					// The mirror may be half applied: drop it, views close.
					for _, f := range c.box.folders {
						f.reset("panic")
					}
				}
			}()
			ok := true
			for _, f := range c.box.folders {
				if !f.hasViews() {
					continue
				}
				if err := f.Sync(ctx, c.by); err != nil {
					ok = false
				}
			}
			if ok {
				c.box.mu.Lock()
				c.box.lastSeq, c.box.seqKnown = c.seq, true
				c.box.mu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	// Connections that do not collect their updates are closed.
	seen := map[*Mailbox]bool{}
	for _, m := range members {
		if !seen[m.box] {
			seen[m.box] = true
			for _, f := range m.box.folders {
				f.checkBacklog()
			}
		}
	}
}

// Run polls every interval while connections are logged in, until ctx ends.
func (st *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st.HasMembers() {
			st.PollOnce(ctx)
		}
	}
}
