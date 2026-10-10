// Package metrics holds the counters behind the internal /metrics endpoint.
// Everything is in memory and resets with the process.
package metrics

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Proto are the counters of one listener (IMAP or SMTP).
type Proto struct {
	ConnsOpen        atomic.Int64
	ConnsAccepted    atomic.Int64
	RejectedTotal    atomic.Int64 // over the global connection limit
	RejectedPerIP    atomic.Int64 // over the per-address limit
	RejectedProxy    atomic.Int64 // PROXY header missing, invalid or from an untrusted peer
	PreauthTimeouts  atomic.Int64 // not authenticated within the time limit
	PreauthViolation atomic.Int64 // line, literal or byte limit before login
	InputViolation   atomic.Int64 // line or literal limit after login
	LoginsOK         atomic.Int64
	LoginsFailed     atomic.Int64 // wrong credentials (edge said "auth") or foreign authzid
	LoginsThrottled  atomic.Int64
	LoginsPreparing  atomic.Int64
	LoginsError      atomic.Int64 // edge unreachable or unexpected answer
	AuthLockouts     atomic.Int64 // connections closed after too many failed attempts
	SessionsEnded    atomic.Int64 // connections closed because the edge rejected the session
	SessionsExpired  atomic.Int64 // connections closed at the local expiry time
	Panics           atomic.Int64
}

func (p *Proto) snapshot() map[string]int64 {
	return map[string]int64{
		"connsOpen":         p.ConnsOpen.Load(),
		"connsAccepted":     p.ConnsAccepted.Load(),
		"rejectedTotal":     p.RejectedTotal.Load(),
		"rejectedPerIp":     p.RejectedPerIP.Load(),
		"rejectedProxy":     p.RejectedProxy.Load(),
		"preauthTimeouts":   p.PreauthTimeouts.Load(),
		"preauthViolations": p.PreauthViolation.Load(),
		"inputViolations":   p.InputViolation.Load(),
		"loginsOk":          p.LoginsOK.Load(),
		"loginsFailed":      p.LoginsFailed.Load(),
		"loginsThrottled":   p.LoginsThrottled.Load(),
		"loginsPreparing":   p.LoginsPreparing.Load(),
		"loginsError":       p.LoginsError.Load(),
		"authLockouts":      p.AuthLockouts.Load(),
		"sessionsEnded":     p.SessionsEnded.Load(),
		"sessionsExpired":   p.SessionsExpired.Load(),
		"panics":            p.Panics.Load(),
	}
}

// EdgePath are the counters of one path of the edge.
type EdgePath struct {
	Count    atomic.Int64
	Errors   atomic.Int64 // transport errors and answers that are not 2xx
	TotalUs  atomic.Int64
	MaxUs    atomic.Int64
	BytesOut atomic.Int64
	BytesIn  atomic.Int64

	statusMu sync.Mutex
	status   map[int]int64 // answers by HTTP status; 0: no answer
}

func (p *EdgePath) noteStatus(status int) {
	if status < 0 || status > 599 {
		status = 599
	}
	p.statusMu.Lock()
	if p.status == nil {
		p.status = map[int]int64{}
	}
	p.status[status]++
	p.statusMu.Unlock()
}

// StatusCount returns how often the path was answered with a status.
func (p *EdgePath) StatusCount(status int) int64 {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	return p.status[status]
}

func statusKey(status int) string {
	if status == 0 {
		return "none" // no answer: transport error, timeout, or not sent
	}
	return strconv.Itoa(status)
}

// Mailbox are the counters of one mailbox, keyed by the hash of its id (logx.HashMailbox).
type Mailbox struct {
	EdgeCalls          int64 `json:"edgeCalls"`
	EdgeBytesIn        int64 `json:"edgeBytesIn"`
	FullLoads          int64 `json:"fullLoads"`
	Resyncs            int64 `json:"resyncs"`
	UIDValidityChanges int64 `json:"uidValidityChanges"`
	RawFetches         int64 `json:"rawFetches"`
	RawBytes           int64 `json:"rawBytes"`
	Logins             int64 `json:"logins"`
	touched            int64
}

// maxMailboxes bounds the per-mailbox table. The least recently touched
// entries are dropped first.
const maxMailboxes = 4000

// Registry is the single place all counters live in.
type Registry struct {
	start time.Time

	IMAP Proto
	SMTP Proto

	// IMAP details
	AppendsKnown       atomic.Int64
	AppendsRejected    atomic.Int64 // unknown mail, wrong folder, no Message-ID
	SearchRejected     atomic.Int64 // criteria the mirror cannot answer exactly
	IdleKeepalives     atomic.Int64
	RawSizeMismatch    atomic.Int64 // raw mail length differs from the size of its row
	MirrorFullLoads    atomic.Int64
	MirrorResyncs      atomic.Int64 // 410 from the edge or a state that went backwards
	MirrorUIDValidity  atomic.Int64
	MirrorEvictions    atomic.Int64
	MirrorRejected     atomic.Int64 // load refused because the row budget is used up
	MirrorInconsistent atomic.Int64 // delta did not fit the mirror, views were closed
	MirrorNoFolder     atomic.Int64 // the edge answered 404 "folder" for INBOX or Sent
	MirrorBacklogKills atomic.Int64 // connections closed because they did not collect updates
	PollerRuns         atomic.Int64
	PollerErrors       atomic.Int64
	PollerPanics       atomic.Int64

	// Answers of the edge that point to a fault between bridge and edge,
	// not to anything a mail program did: a request the edge did not
	// understand (400), one that was too large (413), an error inside the
	// edge (500). Over all paths. A rejection of /submit that names its
	// SMTP answer is not counted here (the edge is speaking to the mail
	// program). Every answer, by path and status, is under
	// edge.<path>.status.
	Edge400 atomic.Int64
	Edge413 atomic.Int64
	Edge500 atomic.Int64
	// EdgeOversize counts JSON requests the bridge did not send because
	// they were larger than the edge takes. Should stay 0 like the above.
	EdgeOversize atomic.Int64
	// EdgeNotSent counts calls refused before sending because a field was
	// outside what the edge takes (a bug in a caller).
	EdgeNotSent atomic.Int64

	// SMTP details
	SubmitSent     atomic.Int64
	SubmitUnknown  atomic.Int64
	SubmitRejected atomic.Int64 // permanent answer of the edge
	SubmitDeferred atomic.Int64 // temporary answer or edge unreachable
	SenderRejected atomic.Int64 // MAIL FROM is not the logged-in address

	edgeMu sync.Mutex
	edge   map[string]*EdgePath

	mbMu  sync.Mutex
	mb    map[string]*Mailbox
	mbSeq int64

	gaugeMu sync.Mutex
	gauges  map[string]func() map[string]int64
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		start:  time.Now(),
		edge:   map[string]*EdgePath{},
		mb:     map[string]*Mailbox{},
		gauges: map[string]func() map[string]int64{},
	}
}

// Edge returns the counters of an edge path, creating them on first use.
func (r *Registry) Edge(path string) *EdgePath {
	r.edgeMu.Lock()
	defer r.edgeMu.Unlock()
	p := r.edge[path]
	if p == nil {
		p = &EdgePath{}
		r.edge[path] = p
	}
	return p
}

// ObserveEdge records one call to the edge. status is the HTTP status of the
// answer, 0 if there was none.
func (r *Registry) ObserveEdge(path string, status int, d time.Duration, bytesOut, bytesIn int64, failed bool) {
	p := r.Edge(path)
	p.Count.Add(1)
	if failed {
		p.Errors.Add(1)
	}
	p.noteStatus(status)
	us := d.Microseconds()
	p.TotalUs.Add(us)
	for {
		cur := p.MaxUs.Load()
		if us <= cur || p.MaxUs.CompareAndSwap(cur, us) {
			break
		}
	}
	p.BytesOut.Add(bytesOut)
	p.BytesIn.Add(bytesIn)
}

// EdgeFault counts an answer that points to a fault between bridge and edge.
func (r *Registry) EdgeFault(status int) {
	switch status {
	case 400:
		r.Edge400.Add(1)
	case 413:
		r.Edge413.Add(1)
	case 500:
		r.Edge500.Add(1)
	}
}

// Mailbox applies fn to the counters of the mailbox with the given hash.
func (r *Registry) Mailbox(hash string, fn func(m *Mailbox)) {
	if hash == "" {
		return
	}
	r.mbMu.Lock()
	defer r.mbMu.Unlock()
	m := r.mb[hash]
	if m == nil {
		if len(r.mb) >= maxMailboxes {
			r.dropOldestLocked(maxMailboxes / 10)
		}
		m = &Mailbox{}
		r.mb[hash] = m
	}
	r.mbSeq++
	m.touched = r.mbSeq
	fn(m)
}

func (r *Registry) dropOldestLocked(n int) {
	type kv struct {
		k string
		t int64
	}
	all := make([]kv, 0, len(r.mb))
	for k, v := range r.mb {
		all = append(all, kv{k, v.touched})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t < all[j].t })
	for i := 0; i < n && i < len(all); i++ {
		delete(r.mb, all[i].k)
	}
}

// MailboxSnapshot returns a copy of the counters of one mailbox.
func (r *Registry) MailboxSnapshot(hash string) Mailbox {
	r.mbMu.Lock()
	defer r.mbMu.Unlock()
	if m := r.mb[hash]; m != nil {
		return *m
	}
	return Mailbox{}
}

// Gauge registers a function that reports current values (sizes of caches).
func (r *Registry) Gauge(name string, fn func() map[string]int64) {
	r.gaugeMu.Lock()
	r.gauges[name] = fn
	r.gaugeMu.Unlock()
}

// Snapshot returns everything as a JSON-serializable tree.
func (r *Registry) Snapshot() map[string]any {
	out := map[string]any{
		"uptimeSeconds": int64(time.Since(r.start).Seconds()),
	}

	imap := r.IMAP.snapshot()
	imap["appendsKnown"] = r.AppendsKnown.Load()
	imap["appendsRejected"] = r.AppendsRejected.Load()
	imap["searchRejected"] = r.SearchRejected.Load()
	imap["idleKeepalives"] = r.IdleKeepalives.Load()
	imap["rawSizeMismatch"] = r.RawSizeMismatch.Load()
	out["imap"] = imap

	smtp := r.SMTP.snapshot()
	smtp["submitSent"] = r.SubmitSent.Load()
	smtp["submitUnknown"] = r.SubmitUnknown.Load()
	smtp["submitRejected"] = r.SubmitRejected.Load()
	smtp["submitDeferred"] = r.SubmitDeferred.Load()
	smtp["senderRejected"] = r.SenderRejected.Load()
	out["smtp"] = smtp

	out["panics"] = r.IMAP.Panics.Load() + r.SMTP.Panics.Load() + r.PollerPanics.Load()

	mirror := map[string]int64{
		"fullLoads":          r.MirrorFullLoads.Load(),
		"resyncs":            r.MirrorResyncs.Load(),
		"uidValidityChanges": r.MirrorUIDValidity.Load(),
		"evictions":          r.MirrorEvictions.Load(),
		"rejected":           r.MirrorRejected.Load(),
		"inconsistent":       r.MirrorInconsistent.Load(),
		"noFolder":           r.MirrorNoFolder.Load(),
		"backlogKills":       r.MirrorBacklogKills.Load(),
		"pollerRuns":         r.PollerRuns.Load(),
		"pollerErrors":       r.PollerErrors.Load(),
		"pollerPanics":       r.PollerPanics.Load(),
	}
	r.gaugeMu.Lock()
	for name, fn := range r.gauges {
		vals := fn()
		if name == "mirror" {
			for k, v := range vals {
				mirror[k] = v
			}
			continue
		}
		out[name] = vals
	}
	r.gaugeMu.Unlock()
	out["mirror"] = mirror

	edge := map[string]any{}
	r.edgeMu.Lock()
	for path, p := range r.edge {
		n := p.Count.Load()
		avg := int64(0)
		if n > 0 {
			avg = p.TotalUs.Load() / n
		}
		status := map[string]int64{}
		p.statusMu.Lock()
		for code, c := range p.status {
			status[statusKey(code)] = c
		}
		p.statusMu.Unlock()
		edge[path] = map[string]any{
			"count":    n,
			"errors":   p.Errors.Load(),
			"avgUs":    avg,
			"maxUs":    p.MaxUs.Load(),
			"totalMs":  p.TotalUs.Load() / 1000,
			"bytesOut": p.BytesOut.Load(),
			"bytesIn":  p.BytesIn.Load(),
			"status":   status,
		}
	}
	r.edgeMu.Unlock()
	out["edge"] = edge
	out["edgeFaults"] = map[string]int64{
		"http400":  r.Edge400.Load(),
		"http413":  r.Edge413.Load(),
		"http500":  r.Edge500.Load(),
		"oversize": r.EdgeOversize.Load(),
		"notSent":  r.EdgeNotSent.Load(),
	}

	boxes := map[string]Mailbox{}
	r.mbMu.Lock()
	for k, v := range r.mb {
		boxes[k] = *v
	}
	r.mbMu.Unlock()
	out["mailboxes"] = boxes
	return out
}
