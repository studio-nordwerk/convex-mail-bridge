package mirror

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/rawcache"
)

// Msg is one row of a folder.
type Msg struct {
	UID    uint32
	Flags  []imap.Flag // interned and immutable
	Modseq int64
	Size   int64
	Date   int64 // milliseconds since 1970
}

// Folder mirrors one folder of one mailbox for one uidValidity.
type Folder struct {
	box  *Mailbox
	Name string

	// syncMu serializes syncs (calls to the edge); mu guards the state and
	// every use of the tracker.
	syncMu sync.Mutex
	mu     sync.Mutex

	loaded      bool
	uidValidity uint32
	uidNext     uint32
	hiModseq    int64
	msgs        []Msg // ascending uid
	tracker     *imapserver.MailboxTracker
	views       map[*View]struct{}

	// rawGen counts how often the cached mails of this folder were thrown
	// away. A mail that was asked for before such a moment is not put into
	// the cache after it: its answer may be late and describe the folder
	// as it was. Guarded by mu.
	rawGen uint64
}

// dropRawLocked throws away the cached mails of the folder. Call with f.mu
// held.
func (f *Folder) dropRawLocked() {
	f.rawGen++
	f.box.st.raw.DropFolder(f.box.key, f.Name)
}

// dropRaw is dropRawLocked for callers that do not hold f.mu.
func (f *Folder) dropRaw() {
	f.mu.Lock()
	f.dropRawLocked()
	f.mu.Unlock()
}

// rawGeneration is read before a mail is asked for at the edge.
func (f *Folder) rawGeneration() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawGen
}

// keepRaw puts a fetched mail into the cache, unless the folder has another
// numbering by now, is not loaded any more, or its cached mails were thrown
// away since gen was read. Throwing away and keeping happen under the same
// lock, so a late answer cannot slip in after the reset.
func (f *Folder) keepRaw(gen uint64, key rawcache.Key, b []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded || f.uidValidity != key.UIDValidity || f.rawGen != gen {
		return false
	}
	f.box.st.raw.Put(key, b)
	return true
}

func newFolder(b *Mailbox, name string) *Folder {
	return &Folder{box: b, Name: name, views: map[*View]struct{}{}}
}

// UIDValidity returns the uidValidity the mirror was built for (0 if not
// loaded).
func (f *Folder) UIDValidity() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return 0
	}
	return f.uidValidity
}

func (f *Folder) hasViews() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.views) > 0
}

func (f *Folder) indexOf(uid uint32) int {
	i := sort.Search(len(f.msgs), func(i int) bool { return f.msgs[i].UID >= uid })
	if i < len(f.msgs) && f.msgs[i].UID == uid {
		return i
	}
	return -1
}

func (f *Folder) stats() (*metrics.Registry, string) { return f.box.st.m, f.box.hash }

// A view that does not collect its updates (no IDLE, no commands) makes its
// queue grow with every change in the folder. softBacklog may be exceeded
// for a moment (a large change, collected right away); a view still above it
// at the second poll in a row is closed. hardBacklog closes at once.
const (
	softBacklog = 2000
	hardBacklog = 20000
)

// queueFor accounts n more queued updates to a view, and to the store.
func (f *Folder) queueFor(v *View, n int) int64 {
	f.box.st.queued.Add(int64(n))
	return v.pending.Add(int64(n))
}

// forget takes whatever a view has pending out of the store's count.
func (f *Folder) forget(v *View) {
	f.box.st.queued.Add(-v.pending.Swap(0))
}

// notifyLocked accounts n queued updates to every view and wakes the
// idling ones.
func (f *Folder) notifyLocked(n int) {
	for v := range f.views {
		if n > 0 && f.queueFor(v, n) > hardBacklog {
			f.dropViewLocked(v, "backlog")
			continue
		}
		select {
		case v.notify <- struct{}{}:
		default:
		}
	}
}

// admitLocked is called before n updates are queued: views that would end
// up beyond hardBacklog are closed first, so that their queues are never
// filled in the first place (a reload that removes a whole folder would
// otherwise queue one entry per mail and view).
//
// All queues of all folders together have a limit as well (Options.
// MaxQueued). When this change would take them over it, the views of this
// folder that are already behind go first, and if that is not enough, all
// of them: reconnecting is cheap, running out of memory is not.
func (f *Folder) admitLocked(n int) {
	if n <= 0 {
		return
	}
	for v := range f.views {
		if v.pending.Load()+int64(n) > hardBacklog {
			f.dropViewLocked(v, "backlog")
		}
	}
	st := f.box.st
	if st.queued.Load()+int64(n)*int64(len(f.views)) <= st.o.MaxQueued {
		return
	}
	for v := range f.views {
		if v.pending.Load() > 0 {
			f.dropViewLocked(v, "backlog")
		}
	}
	if st.queued.Load()+int64(n)*int64(len(f.views)) > st.o.MaxQueued {
		for v := range f.views {
			f.dropViewLocked(v, "backlog")
		}
	}
}

// dropViewLocked ends one view and the connection behind it.
func (f *Folder) dropViewLocked(v *View, code string) {
	v.dead.Store(true)
	delete(f.views, v)
	f.forget(v)
	if !v.closed {
		v.closed = true
		v.tr.Close()
	}
	if code == "backlog" {
		if reg, _ := f.stats(); reg != nil {
			reg.MirrorBacklogKills.Add(1)
		}
	}
	if v.member != nil {
		v.member.Kill(code)
	}
	select {
	case v.notify <- struct{}{}:
	default:
	}
}

// checkBacklog closes views that stayed behind (called by the poller).
func (f *Folder) checkBacklog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for v := range f.views {
		if v.pending.Load() <= softBacklog {
			v.behind = false
			continue
		}
		if v.behind {
			f.dropViewLocked(v, "backlog")
		} else {
			v.behind = true
		}
	}
}

// killViewsLocked ends every connection that has this folder open: their
// sequence numbers and uids no longer describe the folder.
func (f *Folder) killViewsLocked(code string) {
	for v := range f.views {
		f.dropViewLocked(v, code)
	}
	f.views = map[*View]struct{}{}
}

// renumbered is called when the edge says the folder no longer has the
// uidValidity a view was built on: the mirror of this folder goes, the
// connections that have it open are closed, cached raw mails of the folder
// are dropped. The next SELECT reads the folder again.
func (f *Folder) renumbered(uidValidity uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded || f.uidValidity != uidValidity {
		return // somebody else was faster
	}
	reg, hash := f.stats()
	if reg != nil {
		reg.MirrorUIDValidity.Add(1)
		reg.Mailbox(hash, func(mb *metrics.Mailbox) { mb.UIDValidityChanges++ })
	}
	f.box.st.log.Event("uidvalidity_changed", logx.MB(hash), logx.Code("raw"))
	f.dropRawLocked()
	f.killViewsLocked("uidvalidity")
	f.box.rows.Add(-int64(len(f.msgs)))
	f.msgs, f.loaded, f.tracker = nil, false, nil
}

// reset drops the mirror (after a panic while applying changes).
func (f *Folder) reset(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killViewsLocked(code)
	f.box.rows.Add(-int64(len(f.msgs)))
	f.msgs = nil
	f.loaded = false
	f.tracker = nil
}

func rowToMsg(r *edge.Row) Msg {
	flags, _ := internFlags(r.Flags)
	return Msg{UID: r.UID, Flags: flags, Modseq: r.Modseq, Size: r.Size, Date: r.InternalDate}
}

// Sync brings the mirror up to date using the session of member m: a full
// read if there is no mirror yet, otherwise the changes since the mirror's
// state. A 410 of the edge, or a state that went backwards, drops the mirror
// and reads it again; a new uidValidity additionally ends the connections
// that have the folder open.
func (f *Folder) Sync(ctx context.Context, m *Member) error {
	if m.box != f.box {
		return ErrGone
	}
	f.syncMu.Lock()
	defer f.syncMu.Unlock()

	f.mu.Lock()
	loaded, uidv, since := f.loaded, f.uidValidity, f.hiModseq
	f.mu.Unlock()

	if !loaded {
		return f.fullLoad(ctx, m, "cold")
	}

	rows, page, err := f.collectChanges(ctx, m, since)
	if err != nil {
		if edge.IsResync(err) {
			// The mirror cannot be continued: it is too old, or it is a
			// state this folder never had (above its highest one, say after
			// the folder was set back). In the second case a number may
			// stand for another mail by now, so nothing cached for the
			// folder is trusted either.
			f.dropRaw()
			return f.fullLoad(ctx, m, "resync")
		}
		if err == errDeltaTooBig {
			return f.fullLoad(ctx, m, "delta")
		}
		return f.edgeErr(err)
	}
	if page.UIDValidity != uidv {
		return f.fullLoad(ctx, m, "uidvalidity")
	}
	if page.HighestModseq < since {
		// The folder went backwards: not the folder this mirror was built
		// from. Nothing cached for it can be trusted.
		f.dropRaw()
		return f.fullLoad(ctx, m, "regress")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded || f.uidValidity != uidv || f.hiModseq != since {
		return nil // replaced meanwhile (panic reset); the next sync reloads
	}
	// Room is only needed for mails that are new to the mirror; a change of
	// flags must never fail because the mirror is full.
	grow := int64(0)
	for i := range rows {
		known := f.indexOf(rows[i].UID) >= 0
		switch {
		case rows[i].IsLive() && !known:
			grow++
		case !rows[i].IsLive() && known:
			grow--
		}
	}
	if grow > 0 {
		if !f.box.st.reserve(grow) {
			if reg, _ := f.stats(); reg != nil {
				reg.MirrorRejected.Add(1)
			}
			return ErrBudget
		}
		defer f.box.st.unreserve(grow)
	}
	if !f.applyDeltaLocked(rows, page.HighestModseq, page.UIDNext) {
		// The delta does not fit the mirror (a uid appeared below the
		// highest known one, or a known uid is another mail now). Start
		// over; open views cannot be repaired.
		if reg, _ := f.stats(); reg != nil {
			reg.MirrorInconsistent.Add(1)
		}
		f.box.st.log.Event("mirror_inconsistent", logx.MB(f.box.hash))
		f.dropRawLocked()
		f.killViewsLocked("inconsistent")
		f.box.rows.Add(-int64(len(f.msgs)))
		f.msgs, f.loaded, f.tracker = nil, false, nil
	}
	return nil
}

var errDeltaTooBig = &edge.Error{Path: "/changes", Code: "delta_too_big"}

func mapEdgeErr(err error) error {
	if err == ErrSession || err == ErrBudget {
		return err
	}
	return ErrUnavailable
}

// edgeErr maps what a call for this folder failed with. A 404 "folder"
// means the edge does not have this folder for the mailbox: the bridge only
// ever asks for INBOX and Sent, so that is a state the mirror cannot be
// right about. The mirror of the folder goes, cached mails of it go, the
// connections that have it open are closed, and it is counted. Must not be
// called with f.mu held.
func (f *Folder) edgeErr(err error) error {
	if edge.IsNoFolder(err) {
		reg, hash := f.stats()
		if reg != nil {
			reg.MirrorNoFolder.Add(1)
		}
		f.box.st.log.Event("folder_missing", logx.MB(hash))
		f.dropRaw()
		f.reset("folder")
		return ErrUnavailable
	}
	return mapEdgeErr(err)
}

// collectChanges reads all pages of /changes after since and merges them to
// one row per uid (the newest).
func (f *Folder) collectChanges(ctx context.Context, m *Member, since int64) ([]edge.Row, *edge.Page, error) {
	st := f.box.st
	var all []edge.Row
	cursor := since
	var last *edge.Page
	for {
		page, err := st.edge.Changes(ctx, m.Caller(), f.Name, cursor, st.o.PageSize)
		if err = m.check(err); err != nil {
			return nil, nil, err
		}
		if last != nil && page.UIDValidity != last.UIDValidity {
			// renumbered between two pages: let the caller start over
			return nil, page, nil
		}
		all = append(all, page.Rows...)
		last = page
		if !page.More || len(page.Rows) == 0 {
			break
		}
		cursor = page.Rows[len(page.Rows)-1].Modseq
		if len(all) > st.o.MaxDeltaRows {
			return nil, nil, errDeltaTooBig
		}
	}
	if len(all) > 1 {
		// one row per uid, the newest wins
		newest := make(map[uint32]int, len(all))
		for i := range all {
			if j, ok := newest[all[i].UID]; !ok || all[i].Modseq > all[j].Modseq {
				newest[all[i].UID] = i
			}
		}
		if len(newest) != len(all) {
			merged := make([]edge.Row, 0, len(newest))
			for i := range all {
				if newest[all[i].UID] == i {
					merged = append(merged, all[i])
				}
			}
			all = merged
		}
	}
	return all, last, nil
}

// applyDeltaLocked applies merged changes to the mirror and queues the
// updates for the open views. Order matters for the tracker: flag changes
// first (sequence numbers still valid), then removals one by one from the
// back, then new mails in uid order, one EXISTS step each. It returns false
// if the delta cannot be applied to this mirror.
//
// Flag updates go to every view, including the one whose command caused
// them (see applyStore).
func (f *Folder) applyDeltaLocked(rows []edge.Row, newHi int64, uidNext uint32) bool {
	maxUID := uint32(0)
	if n := len(f.msgs); n > 0 {
		maxUID = f.msgs[n-1].UID
	}
	var gone []int
	var fresh []Msg
	queued := 0
	// how many updates this will be, before any is queued
	updates := 0
	for i := range rows {
		r := &rows[i]
		idx := f.indexOf(r.UID)
		switch {
		case !r.IsLive():
			if idx >= 0 {
				updates++
			}
		case idx < 0:
			updates++
		case r.Modseq >= f.msgs[idx].Modseq:
			// A number stands for one mail for good; only its flags change.
			// Another size or date of arrival is another mail under a known
			// number: the mirror, and what is cached for it, is not right.
			if r.Size != f.msgs[idx].Size || r.InternalDate != f.msgs[idx].Date {
				return false
			}
			if nf, _ := internFlags(r.Flags); !sameFlags(f.msgs[idx].Flags, nf) {
				updates++
			}
		}
	}
	f.admitLocked(updates)
	for i := range rows {
		r := &rows[i]
		idx := f.indexOf(r.UID)
		switch {
		case !r.IsLive():
			if idx >= 0 {
				gone = append(gone, idx)
			}
		case idx >= 0:
			cur := &f.msgs[idx]
			if r.Modseq < cur.Modseq {
				continue
			}
			cur.Modseq = r.Modseq
			nf, _ := internFlags(r.Flags)
			if !sameFlags(cur.Flags, nf) {
				cur.Flags = nf
				f.tracker.QueueMessageFlags(uint32(idx)+1, imap.UID(cur.UID), nf, nil)
				queued++
			}
		default:
			if r.UID <= maxUID {
				return false
			}
			fresh = append(fresh, rowToMsg(r))
		}
	}
	if len(gone) > 0 {
		sort.Ints(gone)
		if faultExpungeOrder {
			// Deliberately wrong (build tag faultinject): removes from the
			// front with sequence numbers computed before any removal.
			for k, idx := range gone {
				f.tracker.QueueExpunge(uint32(idx) + 1)
				j := idx - k
				f.msgs = append(f.msgs[:j], f.msgs[j+1:]...)
			}
		} else {
			for k := len(gone) - 1; k >= 0; k-- {
				idx := gone[k]
				f.tracker.QueueExpunge(uint32(idx) + 1)
				f.msgs = append(f.msgs[:idx], f.msgs[idx+1:]...)
			}
		}
		f.box.rows.Add(-int64(len(gone)))
	}
	if len(fresh) > 0 {
		sort.Slice(fresh, func(i, j int) bool { return fresh[i].UID < fresh[j].UID })
		for i := range fresh {
			f.msgs = append(f.msgs, fresh[i])
			f.tracker.QueueNumMessages(uint32(len(f.msgs)))
		}
		f.box.rows.Add(int64(len(fresh)))
	}
	f.hiModseq = newHi
	// The next number comes from the edge. It never goes back, and it is
	// never at or below a mail that is there (that would be a broken
	// answer; the edge client refuses those).
	if uidNext > f.uidNext {
		f.uidNext = uidNext
	}
	f.notifyLocked(queued + len(gone) + len(fresh))
	return true
}

// fullLoad reads the whole folder page by page, then the changes that
// happened while reading, and installs the result.
func (f *Folder) fullLoad(ctx context.Context, m *Member, reason string) error {
	st := f.box.st
	reg, hash := f.stats()

	var (
		msgs    []Msg
		first   *edge.Page
		pages   int
		held    int64  // rows reserved for this load
		uidNext uint32 // the highest "next number" any page reported
	)
	defer func() { st.unreserve(held) }()
	for attempt := 0; ; attempt++ {
		msgs, first, pages, uidNext = nil, nil, 0, 0
		after := uint32(0)
		restart := false
		for {
			page, err := st.edge.Entries(ctx, m.Caller(), f.Name, after, st.o.PageSize)
			if err = m.check(err); err != nil {
				return f.edgeErr(err)
			}
			pages++
			if first == nil {
				first = page
			} else if page.UIDValidity != first.UIDValidity {
				restart = true
				break
			}
			if page.UIDNext > uidNext {
				uidNext = page.UIDNext
			}
			if n := int64(len(page.Rows)); n > 0 {
				// The old rows of this folder are replaced, so only growth
				// beyond them needs room.
				f.mu.Lock()
				have := int64(len(f.msgs))
				f.mu.Unlock()
				if need := int64(len(msgs)) + n - have - held; need > 0 {
					if !st.reserve(need) {
						if reg != nil {
							reg.MirrorRejected.Add(1)
						}
						st.log.Event("mirror_rejected", logx.MB(hash), logx.Code("row_budget"))
						return ErrBudget
					}
					held += need
				}
			}
			for i := range page.Rows {
				msgs = append(msgs, rowToMsg(&page.Rows[i]))
			}
			if !page.More || len(page.Rows) == 0 {
				break
			}
			after = page.Rows[len(page.Rows)-1].UID
		}
		if !restart {
			break
		}
		if attempt >= 2 {
			return ErrUnavailable
		}
	}

	hi := first.HighestModseq
	if pages > 1 {
		// Several pages are several moments in time: fetch what changed
		// since the first one. (One page is one consistent picture.)
		rows, page, err := f.collectChanges(ctx, m, hi)
		if err != nil {
			return f.edgeErr(err)
		}
		if page.UIDValidity != first.UIDValidity {
			return ErrUnavailable // renumbered while loading; the client retries
		}
		msgs = mergeLoaded(msgs, rows)
		if page.HighestModseq > hi {
			hi = page.HighestModseq
		}
		if page.UIDNext > uidNext {
			uidNext = page.UIDNext
		}
	}
	if n := len(msgs); n > 0 && msgs[n-1].UID >= uidNext {
		return ErrUnavailable // a mail at or beyond the next number: not a usable answer
	}

	if reg != nil {
		reg.MirrorFullLoads.Add(1)
		reg.Mailbox(hash, func(mb *metrics.Mailbox) { mb.FullLoads++ })
		switch reason {
		case "resync", "regress", "delta":
			reg.MirrorResyncs.Add(1)
			reg.Mailbox(hash, func(mb *metrics.Mailbox) { mb.Resyncs++ })
		}
	}
	st.log.Event("mirror_loaded", logx.MB(hash), logx.Code(reason), logx.N("rows", int64(len(msgs))))

	f.mu.Lock()
	defer f.mu.Unlock()
	old := int64(len(f.msgs))
	// The catch-up after paging may have added mails: what is installed
	// has to be covered by the reservation in full.
	if need := int64(len(msgs)) - old - held; need > 0 {
		if !st.reserve(need) {
			if reg != nil {
				reg.MirrorRejected.Add(1)
			}
			return ErrBudget
		}
		held += need
	}
	queuedByReload := 0
	switch {
	case !f.loaded:
		f.tracker = imapserver.NewMailboxTracker(uint32(len(msgs)))
		f.msgs = msgs
	case f.uidValidity != first.UIDValidity:
		if reg != nil {
			reg.MirrorUIDValidity.Add(1)
			reg.Mailbox(hash, func(mb *metrics.Mailbox) { mb.UIDValidityChanges++ })
		}
		st.log.Event("uidvalidity_changed", logx.MB(hash))
		f.dropRawLocked()
		f.killViewsLocked("uidvalidity")
		f.tracker = imapserver.NewMailboxTracker(uint32(len(msgs)))
		f.msgs = msgs
	default:
		var ok bool
		if queuedByReload, ok = f.reconcileLocked(msgs); !ok {
			if reg != nil {
				reg.MirrorInconsistent.Add(1)
			}
			st.log.Event("mirror_inconsistent", logx.MB(hash), logx.Code("reload"))
			f.dropRawLocked()
			f.killViewsLocked("inconsistent")
			f.tracker = imapserver.NewMailboxTracker(uint32(len(msgs)))
			f.msgs = msgs
		}
	}
	f.box.rows.Add(int64(len(f.msgs)) - old)
	f.loaded = true
	f.uidValidity = first.UIDValidity
	f.uidNext = uidNext
	f.hiModseq = hi
	f.notifyLocked(queuedByReload)
	return nil
}

// mergeLoaded applies changes to a freshly loaded list (no tracker yet).
func mergeLoaded(msgs []Msg, rows []edge.Row) []Msg {
	if len(rows) == 0 {
		return msgs
	}
	byUID := make(map[uint32]int, len(msgs))
	for i := range msgs {
		byUID[msgs[i].UID] = i
	}
	drop := map[uint32]bool{}
	var fresh []Msg
	for i := range rows {
		r := &rows[i]
		idx, ok := byUID[r.UID]
		switch {
		case !r.IsLive():
			if ok {
				drop[r.UID] = true
			}
		case ok:
			if r.Modseq >= msgs[idx].Modseq {
				msgs[idx] = rowToMsg(r)
			}
		default:
			fresh = append(fresh, rowToMsg(r))
		}
	}
	if len(drop) > 0 {
		kept := msgs[:0]
		for _, m := range msgs {
			if !drop[m.UID] {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}
	if len(fresh) > 0 {
		msgs = append(msgs, fresh...)
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })
	}
	return msgs
}

// reconcileLocked replaces the rows by a freshly loaded list of the same
// uidValidity and tells open views what changed (queued counts the
// updates). It returns false if the new list cannot be reached by flag
// changes, removals and appends.
func (f *Folder) reconcileLocked(next []Msg) (queued int, ok bool) {
	oldMax := uint32(0)
	if n := len(f.msgs); n > 0 {
		oldMax = f.msgs[n-1].UID
	}
	// Anything new has to sort behind everything old.
	nextIdx := make(map[uint32]int, len(next))
	for i := range next {
		nextIdx[next[i].UID] = i
	}
	for i := range next {
		if next[i].UID <= oldMax && f.indexOf(next[i].UID) < 0 {
			return 0, false
		}
	}
	// A number stands for one mail for good: size and date of arrival do not
	// change. If they did, this is another mail under an old number (the
	// folder was set back and filled again), and no open view can be right.
	for i := range f.msgs {
		if j, ok := nextIdx[f.msgs[i].UID]; ok && (next[j].Size != f.msgs[i].Size || next[j].Date != f.msgs[i].Date) {
			return 0, false
		}
	}
	// how many updates this will be, before any is queued
	updates := 0
	for i := range f.msgs {
		if j, ok := nextIdx[f.msgs[i].UID]; !ok || !sameFlags(f.msgs[i].Flags, next[j].Flags) {
			updates++
		}
	}
	for i := range next {
		if next[i].UID > oldMax {
			updates++
		}
	}
	f.admitLocked(updates)
	// flags of mails that stay
	for i := range f.msgs {
		cur := &f.msgs[i]
		if j, ok := nextIdx[cur.UID]; ok && !sameFlags(cur.Flags, next[j].Flags) {
			cur.Flags = next[j].Flags
			f.tracker.QueueMessageFlags(uint32(i)+1, imap.UID(cur.UID), cur.Flags, nil)
			queued++
		}
	}
	// removals, from the back
	for i := len(f.msgs) - 1; i >= 0; i-- {
		if _, ok := nextIdx[f.msgs[i].UID]; !ok {
			f.tracker.QueueExpunge(uint32(i) + 1)
			f.msgs = append(f.msgs[:i], f.msgs[i+1:]...)
			queued++
		}
	}
	// appends
	have := len(f.msgs)
	kept := 0
	for i := range next {
		if next[i].UID <= oldMax {
			kept++
		}
	}
	if kept != have {
		return queued, false
	}
	for i := kept; i < len(next); i++ {
		f.tracker.QueueNumMessages(uint32(have + (i - kept) + 1))
		queued++
	}
	f.msgs = next
	return queued, true
}

// Store changes flags at the edge (at most edge.MaxStoreUIDs uids) and
// applies the answer to the mirror right away.
//
// It runs under the sync lock, like Expunge: a delta that was read from the
// edge before this change must not be applied after it. (Found by the
// random test: a removal applied directly, then an older delta that still
// listed the mail as alive.)
func (f *Folder) Store(ctx context.Context, m *Member, uids []uint32, add, remove []string) error {
	if m.box != f.box {
		return ErrGone
	}
	f.syncMu.Lock()
	defer f.syncMu.Unlock()
	rows, err := f.box.st.edge.Store(ctx, m.Caller(), f.Name, uids, add, remove)
	if err = m.check(err); err != nil {
		return f.edgeErr(err)
	}
	f.applyStore(rows)
	return nil
}

// Expunge hides mails at the edge (at most edge.MaxStoreUIDs uids) and
// removes the ones that were actually hidden from the mirror.
func (f *Folder) Expunge(ctx context.Context, m *Member, uids []uint32) error {
	if m.box != f.box {
		return ErrGone
	}
	f.syncMu.Lock()
	defer f.syncMu.Unlock()
	gone, err := f.box.st.edge.Expunge(ctx, m.Caller(), f.Name, uids)
	if err = m.check(err); err != nil {
		return f.edgeErr(err)
	}
	f.applyExpunge(gone)
	return nil
}

// applyStore applies the answer of /store (rows whose flags changed).
//
// The update is queued for every view, also for the one that sent the
// STORE and gets a direct answer anyway. Leaving that view out looks
// tidier but is wrong: an older update for the same mail may still wait in
// its queue and would be delivered after the direct answer, leaving the
// client with stale flags. With the update queued for everyone, the last
// thing each client hears about a mail is always its current state. (Found
// by the random test.)
func (f *Folder) applyStore(rows []edge.StoreRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return
	}
	queued := 0
	updates := 0
	for i := range rows {
		if idx := f.indexOf(rows[i].UID); idx >= 0 && rows[i].Modseq >= f.msgs[idx].Modseq {
			if nf, _ := internFlags(rows[i].Flags); !sameFlags(f.msgs[idx].Flags, nf) {
				updates++
			}
		}
	}
	f.admitLocked(updates)
	for i := range rows {
		r := &rows[i]
		idx := f.indexOf(r.UID)
		if idx < 0 {
			continue
		}
		cur := &f.msgs[idx]
		if r.Modseq < cur.Modseq {
			continue
		}
		cur.Modseq = r.Modseq
		nf, _ := internFlags(r.Flags)
		if !sameFlags(cur.Flags, nf) {
			cur.Flags = nf
			f.tracker.QueueMessageFlags(uint32(idx)+1, imap.UID(cur.UID), nf, nil)
			queued++
		}
	}
	if queued > 0 {
		f.notifyLocked(queued)
	}
}

// applyExpunge removes mails the edge has just hidden.
func (f *Folder) applyExpunge(uids []uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return
	}
	var gone []int
	for _, uid := range uids {
		if idx := f.indexOf(uid); idx >= 0 {
			gone = append(gone, idx)
		}
	}
	if len(gone) == 0 {
		return
	}
	sort.Ints(gone)
	queued := 0
	f.admitLocked(len(gone))
	for k := len(gone) - 1; k >= 0; k-- {
		idx := gone[k]
		if k > 0 && gone[k-1] == idx {
			continue
		}
		f.tracker.QueueExpunge(uint32(idx) + 1)
		f.msgs = append(f.msgs[:idx], f.msgs[idx+1:]...)
		f.box.rows.Add(-1)
		queued++
	}
	f.notifyLocked(queued)
}

// ---- views ---------------------------------------------------------------

// View is one connection's selected folder.
type View struct {
	f        *Folder
	member   *Member
	tr       *imapserver.SessionTracker
	ReadOnly bool
	uidv     uint32
	notify   chan struct{}
	dead     atomic.Bool
	closed   bool
	pending  atomic.Int64 // updates queued since the last full poll
	behind   bool         // above softBacklog at the last check
}

// Selected is what SELECT answers.
type Selected struct {
	NumMessages uint32
	FirstUnseen uint32
	UIDNext     uint32
	UIDValidity uint32
	Keywords    []imap.Flag
}

// Open creates a view on the (loaded) folder for member m.
func (f *Folder) Open(m *Member, readOnly bool) (*View, *Selected, error) {
	if m.box != f.box {
		return nil, nil, ErrGone
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return nil, nil, ErrUnavailable
	}
	v := &View{f: f, member: m, tr: f.tracker.NewSession(), ReadOnly: readOnly, uidv: f.uidValidity, notify: make(chan struct{}, 1)}
	f.views[v] = struct{}{}
	sel := &Selected{NumMessages: uint32(len(f.msgs)), UIDNext: f.uidNext, UIDValidity: f.uidValidity}
	seen := map[imap.Flag]bool{}
	for i := range f.msgs {
		isSeen := false
		for _, fl := range f.msgs[i].Flags {
			if fl == imap.FlagSeen {
				isSeen = true
			} else if len(fl) > 0 && fl[0] != '\\' && !seen[fl] && len(sel.Keywords) < 64 {
				seen[fl] = true
				sel.Keywords = append(sel.Keywords, fl)
			}
		}
		if !isSeen && sel.FirstUnseen == 0 {
			sel.FirstUnseen = uint32(i) + 1
		}
	}
	sort.Slice(sel.Keywords, func(i, j int) bool { return sel.Keywords[i] < sel.Keywords[j] })
	return v, sel, nil
}

// Folder returns the folder of the view.
func (v *View) Folder() *Folder { return v.f }

// UIDValidity is the uidValidity the view was opened on.
func (v *View) UIDValidity() uint32 { return v.uidv }

// Dead reports whether the folder was reset under the view.
func (v *View) Dead() bool { return v.dead.Load() }

// Notify signals that updates are queued (or that the view died).
func (v *View) Notify() <-chan struct{} { return v.notify }

// Close ends the view.
func (v *View) Close() {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	delete(f.views, v)
	f.forget(v)
	v.tr.Close()
}

// Poll writes queued updates to the client.
func (v *View) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if v.dead.Load() {
		return nil
	}
	if allowExpunge {
		// Everything queued so far is written now. Reset first: an update
		// queued in between is then counted once too often, never too
		// rarely.
		v.f.forget(v)
	}
	return v.tr.Poll(w, allowExpunge)
}

// Item is a mail as one view sees it: Seq is the client's sequence number.
type Item struct {
	Seq uint32
	Msg Msg
}

// Range is a range of sequence numbers or uids; 0 stands for "*".
type Range struct{ Lo, Hi uint32 }

// normalize resolves "*" to max, orders the bounds, sorts and merges.
func normalize(in []Range, max uint32) []Range {
	out := make([]Range, 0, len(in))
	for _, r := range in {
		if r.Lo == 0 {
			r.Lo = max
		}
		if r.Hi == 0 {
			r.Hi = max
		}
		if r.Lo == 0 || r.Hi == 0 {
			continue // "*" in an empty folder
		}
		if r.Lo > r.Hi {
			r.Lo, r.Hi = r.Hi, r.Lo
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lo < out[j].Lo })
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && (r.Lo <= merged[n-1].Hi || r.Lo == merged[n-1].Hi+1) {
			if r.Hi > merged[n-1].Hi {
				merged[n-1].Hi = r.Hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// visibleLocked returns how many leading rows this client knows about (new
// mails it has not been told of yet sit at the end) and its highest
// sequence number.
func (v *View) visibleLocked() (n int, maxSeq uint32) {
	f := v.f
	n = len(f.msgs)
	for n > 0 {
		if cs := v.tr.EncodeSeqNum(uint32(n)); cs != 0 {
			return n, cs
		}
		n--
	}
	return 0, 0
}

func (v *View) okLocked() bool {
	return !v.dead.Load() && !v.closed && v.f.loaded
}

// Bounds returns the client's highest sequence number and the highest uid
// it knows.
func (v *View) Bounds() (maxSeq, maxUID uint32, err error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !v.okLocked() {
		return 0, 0, ErrViewDead
	}
	n, ms := v.visibleLocked()
	if n > 0 {
		maxUID = f.msgs[n-1].UID
	}
	return ms, maxUID, nil
}

// Resolve returns the mails of a number set (uids or the client's sequence
// numbers) in ascending order.
func (v *View) Resolve(byUID bool, set []Range) ([]Item, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !v.okLocked() {
		return nil, ErrViewDead
	}
	n, maxSeq := v.visibleLocked()
	if n == 0 {
		return nil, nil
	}
	var out []Item
	if byUID {
		for _, r := range normalize(set, f.msgs[n-1].UID) {
			i := sort.Search(n, func(i int) bool { return f.msgs[i].UID >= r.Lo })
			for ; i < n && f.msgs[i].UID <= r.Hi; i++ {
				if cs := v.tr.EncodeSeqNum(uint32(i) + 1); cs != 0 {
					out = append(out, Item{Seq: cs, Msg: f.msgs[i]})
				}
			}
		}
		return out, nil
	}
	for _, r := range normalize(set, maxSeq) {
		// client sequence numbers grow with the row index
		i := sort.Search(n, func(i int) bool { return v.tr.EncodeSeqNum(uint32(i)+1) >= r.Lo })
		for ; i < n; i++ {
			cs := v.tr.EncodeSeqNum(uint32(i) + 1)
			if cs == 0 || cs > r.Hi {
				break
			}
			out = append(out, Item{Seq: cs, Msg: f.msgs[i]})
		}
	}
	return out, nil
}

// Each calls fn for every mail the client knows about, in order. fn runs
// under the folder lock and must not block.
func (v *View) Each(fn func(it Item)) error {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !v.okLocked() {
		return ErrViewDead
	}
	n, _ := v.visibleLocked()
	for i := 0; i < n; i++ {
		if cs := v.tr.EncodeSeqNum(uint32(i) + 1); cs != 0 {
			fn(Item{Seq: cs, Msg: f.msgs[i]})
		}
	}
	return nil
}

// Deleted returns the uids of mails flagged \Deleted, optionally limited
// to a uid set.
func (v *View) Deleted(limit []Range) ([]uint32, error) {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !v.okLocked() {
		return nil, ErrViewDead
	}
	var set []Range
	if limit != nil {
		max := uint32(0)
		if n := len(f.msgs); n > 0 {
			max = f.msgs[n-1].UID
		}
		set = normalize(limit, max)
	}
	var out []uint32
	for i := range f.msgs {
		m := &f.msgs[i]
		if !HasFlag(m.Flags, imap.FlagDeleted) {
			continue
		}
		if limit != nil && !inRanges(set, m.UID) {
			continue
		}
		out = append(out, m.UID)
	}
	return out, nil
}

func inRanges(set []Range, n uint32) bool {
	i := sort.Search(len(set), func(i int) bool { return set[i].Hi >= n })
	return i < len(set) && set[i].Lo <= n
}

// Normalize is normalize for callers outside the package (search).
func Normalize(in []Range, max uint32) []Range { return normalize(in, max) }

// InRanges reports whether n is in a normalized set.
func InRanges(set []Range, n uint32) bool { return inRanges(set, n) }

// Keywords returns the keywords in use in the folder (bounded).
func (v *View) Keywords() []imap.Flag {
	f := v.f
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[imap.Flag]bool{}
	var out []imap.Flag
	for i := range f.msgs {
		for _, fl := range f.msgs[i].Flags {
			if len(fl) > 0 && fl[0] != '\\' && !seen[fl] && len(out) < 64 {
				seen[fl] = true
				out = append(out, fl)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
