// Package edgestub is a test edge: an HTTP handler that implements
// edge-contract.md completely in memory, plus functions tests and the
// local dev stack use to change mailboxes from the outside (new mail, read,
// removed, revoked).
//
// It is not the Convex side and makes no claim about how that side stores
// anything. It only has to answer the way the contract says.
package edgestub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Folder names.
const (
	Inbox = "INBOX"
	Sent  = "Sent"
)

// Row is one numbered mail of a folder. Rows are never deleted by normal
// operation: a mail that left the folder keeps its row with Live false.
type Row struct {
	UID          uint32
	Flags        []string
	Modseq       int64
	Size         int64
	InternalDate int64
	Live         bool
	Raw          []byte
	MessageID    string // normalized, for Sent
	Digest       string
}

type folder struct {
	name        string
	specialUse  string
	uidValidity uint32
	uidNext     uint32
	modseq      int64
	floor       int64 // /changes below this answer 410
	rows        []*Row
}

type devicePassword struct {
	hash    [32]byte
	revoked bool
}

type mailbox struct {
	id        string // stable, opaque; what /login returns as "mailbox"
	address   string
	disabled  bool
	preparing bool
	seq       int64
	passwords []*devicePassword
	folders   map[string]*folder
	submits   map[string]submitRecord // by normalized Message-ID
}

type submitRecord struct {
	digest string
	uid    uint32
	status string
}

type session struct {
	box       *mailbox
	pw        *devicePassword
	expiresAt time.Time
	dead      bool
}

type attempt struct {
	count int
	since time.Time
}

// SubmitResult lets a test decide how /submit answers.
type SubmitResult struct {
	Status   string // "sent", "unknown" or "" for an error
	HTTP     int    // for errors, e.g. 422
	Error    string
	SMTPCode int
	Message  string
}

// Fault makes the next calls to a path fail or stall.
type Fault struct {
	Status int           // HTTP status to answer with (0: none)
	Body   string        // body for Status
	Delay  time.Duration // wait before answering
	Count  int           // how many calls are affected (<=0: until cleared)
	Cut    bool          // /raw: announce the full length, send half
	Late   bool          // /raw: read the mail first, then wait Delay (an answer that is late)
	Header map[string]string
}

// Stub is the test edge.
type Stub struct {
	mu       sync.Mutex
	secret   string
	now      func() time.Time
	boxes    map[string]*mailbox
	sessions map[string]*session
	attempts map[string]*attempt
	faults   map[string]*Fault
	calls    map[string]int
	answers  map[string]int // "<path> <status>"
	refused  int            // 400 and 413 for JSON requests
	uidvSeed uint32

	// SessionTTL is the lifetime of new sessions (12 h in the contract).
	SessionTTL time.Duration
	// After ThrottleAfter failed logins per address and client within
	// ThrottleWindow, further failed ones answer 429 (body and Retry-After
	// header). A valid device password is never throttled.
	ThrottleAfter  int
	ThrottleWindow time.Duration
	// ThrottleHeaderOnly leaves retryAfterSeconds out of the 429 body, so
	// that only the Retry-After header says how long to wait.
	ThrottleHeaderOnly bool
	// MaxSubmitBytes is the size limit of /submit (5 MiB).
	MaxSubmitBytes int
	// SubmitHook, if set, decides the outcome of a submission. It runs under
	// the lock of the stub and must not call back into it.
	SubmitHook func(from string, to []string, raw []byte) *SubmitResult
	// OnSent is called after a submission was filed in Sent (dev stack:
	// deliver to local recipients).
	OnSent func(from string, to []string, raw []byte)

	seen Seen
}

// Seen is what the stub last received, for assertions.
type Seen struct {
	AppendDigest    string
	AppendMessageID string
	LoginIP         string
	SubmitFrom      string
	SubmitTo        []string
}

// Last returns what the stub last received.
func (s *Stub) Last() Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.seen
	cp.SubmitTo = append([]string(nil), s.seen.SubmitTo...)
	return cp
}

// SetNow replaces the clock (tests: let sessions run out).
func (s *Stub) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// New returns an empty stub that accepts the given shared secret.
func New(secret string) *Stub {
	return &Stub{
		secret:         secret,
		now:            time.Now,
		boxes:          map[string]*mailbox{},
		sessions:       map[string]*session{},
		attempts:       map[string]*attempt{},
		faults:         map[string]*Fault{},
		calls:          map[string]int{},
		answers:        map[string]int{},
		uidvSeed:       1791000000,
		SessionTTL:     12 * time.Hour,
		ThrottleAfter:  5,
		ThrottleWindow: time.Minute,
		MaxSubmitBytes: 5 << 20,
	}
}

// ---- control functions -------------------------------------------------

func key(address string) string { return strings.ToLower(jsTrim(address)) }

// AddMailbox creates a mailbox with empty INBOX and Sent. If the address
// already names a mailbox, that one is switched off for good (its sessions
// end) and the address names the new one from now on: a new mailbox with
// its own identifier.
func (s *Stub) AddMailbox(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.boxes[key(address)]; old != nil {
		old.disabled = true
		for _, se := range s.sessions {
			if se.box == old {
				se.dead = true
			}
		}
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	b := &mailbox{id: "mbx_" + hex.EncodeToString(buf), address: key(address), folders: map[string]*folder{}, submits: map[string]submitRecord{}}
	for _, name := range []string{Inbox, Sent} {
		s.uidvSeed += 7
		f := &folder{name: name, uidValidity: s.uidvSeed, uidNext: 1}
		if name == Sent {
			f.specialUse = `\Sent`
		}
		b.folders[name] = f
	}
	s.boxes[b.address] = b
}

// AddAlias lets a second address log in to an existing mailbox (with the
// mailbox's device passwords).
func (s *Stub) AddAlias(alias, address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boxes[key(alias)] = s.boxes[key(address)]
}

// MailboxID returns the identifier /login hands out for an address.
func (s *Stub) MailboxID(address string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boxes[key(address)].id
}

// AddPassword creates a device password (192 bits, prefix mb_) and returns
// it. The stub keeps only its hash.
func (s *Stub) AddPassword(address string) string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	pw := "mb_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	s.SetPassword(address, pw)
	return pw
}

// SetPassword registers a given device password (dev stack: from a file).
func (s *Stub) SetPassword(address, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	if b == nil {
		panic("edgestub: unknown mailbox")
	}
	b.passwords = append(b.passwords, &devicePassword{hash: sha256.Sum256([]byte(password))})
}

// RevokePassword revokes a device password. Sessions bound to it stop
// working at once.
func (s *Stub) RevokePassword(address, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	h := sha256.Sum256([]byte(password))
	for _, p := range b.passwords {
		if p.hash == h {
			p.revoked = true
		}
	}
}

// SetPreparing switches the "backfill is running" state of a mailbox.
func (s *Stub) SetPreparing(address string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boxes[key(address)].preparing = on
}

// Disable switches a mailbox off: its sessions become invalid for good.
func (s *Stub) Disable(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	b.disabled = true
	for _, se := range s.sessions {
		if se.box == b {
			se.dead = true
		}
	}
}

// ExpireSessions lets all sessions of a mailbox run out now.
func (s *Stub) ExpireSessions(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	for _, se := range s.sessions {
		if se.box == b {
			se.expiresAt = s.now().Add(-time.Second)
		}
	}
}

// SessionTokens returns every session token ever issued (tests: none of
// them may appear in a log).
func (s *Stub) SessionTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for t := range s.sessions {
		out = append(out, t)
	}
	return out
}

func normFlags(in []string) []string {
	set := map[string]struct{}{}
	for _, f := range in {
		if f != "" {
			set[f] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (s *Stub) bump(b *mailbox, f *folder, r *Row) {
	f.modseq++
	r.Modseq = f.modseq
	b.seq++
}

// Deliver files a mail into a folder and returns its uid.
func (s *Stub) Deliver(address, folderName string, raw []byte, internalDate time.Time, flags ...string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliverLocked(s.boxes[key(address)], folderName, raw, internalDate, flags)
}

func (s *Stub) deliverLocked(b *mailbox, folderName string, raw []byte, internalDate time.Time, flags []string) uint32 {
	f := b.folders[folderName]
	sum := sha256.Sum256(raw)
	r := &Row{
		UID: f.uidNext, Flags: normFlags(flags), Size: int64(len(raw)),
		InternalDate: internalDate.UnixMilli(), Live: true,
		Raw: append([]byte(nil), raw...), MessageID: NormalizeMessageID(HeaderValue(raw, "Message-ID")),
		Digest: hex.EncodeToString(sum[:]),
	}
	f.uidNext++
	f.rows = append(f.rows, r)
	s.bump(b, f, r)
	return r.UID
}

func (f *folder) find(uid uint32) *Row {
	i := sort.Search(len(f.rows), func(i int) bool { return f.rows[i].UID >= uid })
	if i < len(f.rows) && f.rows[i].UID == uid {
		return f.rows[i]
	}
	return nil
}

func applyFlags(r *Row, add, remove []string) bool {
	set := map[string]struct{}{}
	for _, f := range r.Flags {
		set[f] = struct{}{}
	}
	changed := false
	for _, f := range add {
		if _, ok := set[f]; !ok && f != "" {
			set[f] = struct{}{}
			changed = true
		}
	}
	for _, f := range remove {
		if _, ok := set[f]; ok {
			delete(set, f)
			changed = true
		}
	}
	if !changed {
		return false
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	r.Flags = out
	return true
}

// SetFlags changes flags from the outside (console: read, unread). It
// reports whether anything changed.
func (s *Stub) SetFlags(address, folderName string, uid uint32, add, remove []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	f := b.folders[folderName]
	r := f.find(uid)
	if r == nil || !r.Live {
		return false
	}
	if !applyFlags(r, add, remove) {
		return false
	}
	s.bump(b, f, r)
	return true
}

// Remove takes a mail out of a folder from the outside (deleted for good in
// the app). It reports whether the mail was there.
func (s *Stub) Remove(address, folderName string, uid uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	f := b.folders[folderName]
	r := f.find(uid)
	if r == nil || !r.Live {
		return false
	}
	r.Live = false
	s.bump(b, f, r)
	return true
}

// Prune drops removed rows and makes every /changes call with an older
// sinceModseq answer 410 (retention ran out).
func (s *Stub) Prune(address, folderName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.boxes[key(address)].folders[folderName]
	kept := f.rows[:0]
	for _, r := range f.rows {
		if r.Live {
			kept = append(kept, r)
		}
	}
	f.rows = kept
	f.floor = f.modseq
}

// Renumber gives a folder a new uidValidity and numbers its living mails
// from 1 (an explicit repair run).
func (s *Stub) Renumber(address, folderName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	f := b.folders[folderName]
	s.uidvSeed += 7
	f.uidValidity = s.uidvSeed
	var kept []*Row
	uid := uint32(1)
	for _, r := range f.rows {
		if !r.Live {
			continue
		}
		r.UID = uid
		uid++
		f.modseq++
		r.Modseq = f.modseq
		kept = append(kept, r)
	}
	f.rows = kept
	f.uidNext = uid
	f.floor = f.modseq
	b.seq++
}

// SetUIDValidity forces the uidValidity of a folder (tests: two mailboxes
// whose folders look alike in every number).
func (s *Stub) SetUIDValidity(address, folderName string, v uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boxes[key(address)].folders[folderName].uidValidity = v
}

// SkipUIDs moves the next uid of a folder forward by n without creating
// rows (numbers that were given out and are gone again).
func (s *Stub) SkipUIDs(address, folderName string, n uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boxes[key(address)].folders[folderName].uidNext += n
}

// FolderState is a copy of a folder for assertions.
type FolderState struct {
	UIDValidity   uint32
	UIDNext       uint32
	HighestModseq int64
	Rows          []Row // living rows, ascending uid
}

// State returns the living rows of a folder.
func (s *Stub) State(address, folderName string) FolderState {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.boxes[key(address)].folders[folderName]
	st := FolderState{UIDValidity: f.uidValidity, UIDNext: f.uidNext, HighestModseq: f.modseq}
	for _, r := range f.rows {
		if r.Live {
			cp := *r
			cp.Flags = append([]string(nil), r.Flags...)
			st.Rows = append(st.Rows, cp)
		}
	}
	return st
}

// Seq returns the change counter of a mailbox.
func (s *Stub) Seq(address string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boxes[key(address)].seq
}

// SetFault installs a fault for a path such as "/raw".
func (s *Stub) SetFault(path string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := f
	s.faults[path] = &cp
}

// ClearFaults removes all faults.
func (s *Stub) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = map[string]*Fault{}
}

// Calls returns how often a path was called.
func (s *Stub) Calls(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// ResetCalls clears the call counters.
func (s *Stub) ResetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = map[string]int{}
}

// DropFolder removes a folder from a mailbox: the edge then answers 404
// "folder" for it (404 "gone" on /raw) and no longer lists it.
func (s *Stub) DropFolder(address, folderName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.boxes[key(address)]; b != nil {
		delete(b.folders, folderName)
		b.seq++
	}
}

// Rewind takes the newest n mails of a folder away without a trace and sets
// the folder back to the state before them, numbering included: what a
// restored backup looks like. A bridge that asks /changes with its newer
// state gets 410; the next mail delivered reuses the numbers.
func (s *Stub) Rewind(address, folderName string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[key(address)]
	if b == nil {
		return
	}
	f := b.folders[folderName]
	if f == nil || n <= 0 {
		return
	}
	if n > len(f.rows) {
		n = len(f.rows)
	}
	cut := f.rows[len(f.rows)-n:]
	f.rows = f.rows[:len(f.rows)-n]
	f.uidNext = cut[0].UID
	f.modseq = 0
	for _, r := range f.rows {
		if r.Modseq > f.modseq {
			f.modseq = r.Modseq
		}
	}
	if f.floor > f.modseq {
		f.floor = f.modseq
	}
	b.seq++
}

// ---- HTTP ----------------------------------------------------------------

type apiError struct {
	status int
	body   map[string]any
	header map[string]string
}

// errorText: every error body carries a short message without addresses
// and contents, like the edge's.
var errorText = map[string]string{
	"auth":        "login failed",
	"throttled":   "too many failed attempts, try again later",
	"preparing":   "the mailbox is being prepared",
	"session":     "session invalid or expired",
	"folder":      "folder unknown or not allowed here",
	"gone":        "mail no longer there",
	"uidvalidity": "the numbering of the folder changed, read it again",
	"resync":      "state too old, read the folder again",
	"unknown":     "mail unknown",
	"request":     "request invalid",
	"too_large":   "request too large",
	"internal":    "internal error",
}

func fail(status int, code string) *apiError {
	msg := errorText[code]
	if msg == "" {
		msg = code
	}
	return &apiError{status: status, body: map[string]any{"error": code, "message": msg}}
}

var (
	errRequest  = fail(http.StatusBadRequest, "request")
	errTooLarge = fail(http.StatusRequestEntityTooLarge, "too_large")
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func itoa(n int) string { return strconv.Itoa(n) }

// answer writes an error and notes it.
func (s *Stub) answer(w http.ResponseWriter, path string, e *apiError) {
	s.mu.Lock()
	s.note(path, e.status)
	s.mu.Unlock()
	for k, v := range e.header {
		w.Header().Set(k, v)
	}
	writeJSON(w, e.status, e.body)
}

// note counts an answer by path and status. Call with s.mu held.
func (s *Stub) note(path string, status int) {
	s.answers[path+" "+itoa(status)]++
}

// refuse answers a JSON request that is malformed (400) or too large (413)
// and counts it: this is what a bridge that keeps to the contract never
// causes.
func (s *Stub) refuse(w http.ResponseWriter, path string, e *apiError) {
	s.mu.Lock()
	s.refused++
	s.mu.Unlock()
	s.answer(w, path, e)
}

// Answered returns how often a path was answered with a status.
func (s *Stub) Answered(path string, status int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answers[path+" "+itoa(status)]
}

// Refused returns how many JSON requests the stub turned down as malformed
// or too large (400 and 413), over all paths. For a bridge that keeps to the
// contract this stays 0.
func (s *Stub) Refused() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// jsonRoutes are the paths with a JSON body; the value says whether the
// call carries a session.
var jsonRoutes = map[string]bool{
	"/login": false, "/seq": false,
	"/folders": true, "/entries": true, "/changes": true, "/raw": true,
	"/store": true, "/expunge": true, "/append": true,
}

// ServeHTTP implements the edge under /bridge/v1.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/bridge/v1"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)

	// Secret first, before anything else, empty body on failure. A secret
	// of less than 32 characters switches the edge off altogether.
	got := r.Header.Get("Authorization")
	want := "Bearer " + s.secret
	if len(s.secret) < 32 || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	s.calls[path]++
	var fault *Fault
	if f := s.faults[path]; f != nil {
		cp := *f
		fault = &cp
		if f.Count > 0 {
			f.Count--
			if f.Count == 0 {
				delete(s.faults, path)
			}
		}
	}
	s.mu.Unlock()

	if fault != nil {
		if fault.Delay > 0 && !fault.Late {
			select {
			case <-time.After(fault.Delay):
			case <-r.Context().Done():
				return
			}
		}
		if fault.Status != 0 {
			for k, v := range fault.Header {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fault.Status)
			_, _ = io.WriteString(w, fault.Body)
			return
		}
	}

	if path == "/submit" {
		// The only call whose body is a mail.
		if !tokenShape(r.Header.Get("X-Bridge-Session")) {
			s.answer(w, path, fail(http.StatusUnauthorized, "session"))
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			s.answer(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.submit(se, r, raw) })
		return
	}

	needsSession, known := jsonRoutes[path]
	if !known {
		http.NotFound(w, r)
		return
	}
	// The form of the session, where the call carries one.
	if needsSession && !tokenShape(r.Header.Get("X-Bridge-Session")) {
		s.answer(w, path, fail(http.StatusUnauthorized, "session"))
		return
	}
	// The body: first the announced length, then the real one.
	if r.ContentLength > maxJSONBytes {
		s.refuse(w, path, errTooLarge)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBytes+1))
	if err != nil {
		s.refuse(w, path, errRequest)
		return
	}
	if len(data) > maxJSONBytes {
		s.refuse(w, path, errTooLarge)
		return
	}
	b, ok := parseObject(data)
	if !ok {
		s.refuse(w, path, errRequest)
		return
	}

	switch path {
	case "/login":
		s.handleLogin(w, b)
	case "/seq":
		s.handleSeq(w, b)
	case "/raw":
		s.handleRaw(w, r, b, fault)
	case "/folders":
		if !onlyKeys(b) {
			s.refuse(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.folders(se) })
	case "/entries":
		in, ok := parseEntries(b)
		if !ok {
			s.refuse(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.entries(se, in) })
	case "/changes":
		in, ok := parseChanges(b)
		if !ok {
			s.refuse(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.changes(se, in) })
	case "/store":
		in, ok := parseStore(b)
		if !ok {
			s.refuse(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.store(se, in) })
	case "/expunge":
		in, ok := parseExpunge(b)
		if !ok {
			s.refuse(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.expunge(se, in) })
	case "/append":
		in, ok := parseAppend(b)
		if !ok {
			s.refuse(w, path, errRequest)
			return
		}
		s.withSession(w, r, path, func(se *session) (any, *apiError) { return s.append(se, in) })
	}
}

// sessionLocked resolves a session token. The mailbox comes from the token
// only.
func (s *Stub) sessionLocked(token string) *session {
	se := s.sessions[token]
	if se == nil || se.dead || se.pw.revoked || se.box.disabled || !s.now().Before(se.expiresAt) {
		return nil
	}
	return se
}

func (s *Stub) withSession(w http.ResponseWriter, r *http.Request, path string, fn func(se *session) (any, *apiError)) {
	s.mu.Lock()
	se := s.sessionLocked(r.Header.Get("X-Bridge-Session"))
	if se == nil {
		s.mu.Unlock()
		s.answer(w, path, fail(http.StatusUnauthorized, "session"))
		return
	}
	out, aerr := fn(se)
	if aerr == nil {
		s.note(path, http.StatusOK)
	}
	s.mu.Unlock()
	if aerr != nil {
		s.answer(w, path, aerr)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Stub) handleLogin(w http.ResponseWriter, b body) {
	const path = "/login"
	if !onlyKeys(b, "address", "password", "clientIp") {
		s.refuse(w, path, errRequest)
		return
	}
	address, ok1 := text(b["address"], maxAddressChars)
	password, ok2 := text(b["password"], maxPasswordChars)
	clientIP, ok3 := text(b["clientIp"], maxIPChars)
	ip, ok4 := throttleIP(clientIP)
	if !ok1 || !ok2 || !ok3 || !ok4 || key(address) == "" {
		s.refuse(w, path, errRequest)
		return
	}

	s.mu.Lock()
	s.seen.LoginIP = clientIP
	now := s.now()
	bx := s.boxes[key(address)]
	var pw *devicePassword
	if bx != nil && !bx.disabled {
		h := sha256.Sum256([]byte(password))
		for _, p := range bx.passwords {
			if !p.revoked && subtle.ConstantTimeCompare(p.hash[:], h[:]) == 1 {
				pw = p
			}
		}
	}
	if pw == nil {
		// Unknown address, wrong and revoked password: the same answer. The
		// throttle is only for these: per pair of address and client.
		akey := key(address) + "|" + ip
		a := s.attempts[akey]
		if a != nil && now.Sub(a.since) > s.ThrottleWindow {
			delete(s.attempts, akey)
			a = nil
		}
		if a != nil && a.count >= s.ThrottleAfter {
			retry := int(s.ThrottleWindow.Seconds() - now.Sub(a.since).Seconds())
			if retry < 1 {
				retry = 1
			}
			s.mu.Unlock()
			e := fail(http.StatusTooManyRequests, "throttled")
			if !s.ThrottleHeaderOnly {
				e.body["retryAfterSeconds"] = retry
			}
			e.header = map[string]string{"Retry-After": itoa(retry)}
			s.answer(w, path, e)
			return
		}
		if a == nil {
			a = &attempt{since: now}
			s.attempts[akey] = a
		}
		a.count++
		s.mu.Unlock()
		s.answer(w, path, fail(http.StatusUnauthorized, "auth"))
		return
	}
	// A valid device password is never throttled, whatever the counters say.
	if bx.preparing {
		s.mu.Unlock()
		s.answer(w, path, fail(http.StatusServiceUnavailable, "preparing"))
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	// 256 bit as base64url, 43 characters: the form of the edge's sessions.
	token := base64.RawURLEncoding.EncodeToString(buf)
	exp := now.Add(s.SessionTTL)
	s.sessions[token] = &session{box: bx, pw: pw, expiresAt: exp}
	s.note(path, http.StatusOK)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"session": token, "expiresAt": exp.UnixMilli(), "mailbox": bx.id})
}

func (s *Stub) handleSeq(w http.ResponseWriter, b body) {
	const path = "/seq"
	list, isList := b["sessions"].([]any)
	if !onlyKeys(b, "sessions") || !isList || len(list) > maxSessions {
		s.refuse(w, path, errRequest)
		return
	}
	tokens := make([]string, 0, len(list))
	for _, e := range list {
		t, ok := e.(string)
		if !ok {
			s.refuse(w, path, errRequest)
			return
		}
		if !tokenShape(t) {
			t = "" // cannot be a session: answered as invalid, in its place
		}
		tokens = append(tokens, t)
	}
	s.mu.Lock()
	results := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		if se := s.sessionLocked(t); t != "" && se != nil {
			results = append(results, map[string]any{"ok": true, "seq": se.box.seq})
		} else {
			results = append(results, map[string]any{"ok": false})
		}
	}
	s.note(path, http.StatusOK)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Stub) folders(se *session) (any, *apiError) {
	var out []map[string]any
	for _, name := range []string{Inbox, Sent} {
		f := se.box.folders[name]
		if f == nil {
			continue
		}
		exists, unseen := 0, 0
		for _, r := range f.rows {
			if !r.Live {
				continue
			}
			exists++
			if !hasFlag(r.Flags, `\Seen`) {
				unseen++
			}
		}
		m := map[string]any{
			"name": f.name, "uidValidity": f.uidValidity, "uidNext": f.uidNext,
			"highestModseq": f.modseq, "exists": exists, "unseen": unseen,
		}
		if f.specialUse != "" {
			m["specialUse"] = f.specialUse
		}
		out = append(out, m)
	}
	return map[string]any{"folders": out}, nil
}

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

func rowJSON(r *Row, withLive bool) map[string]any {
	m := map[string]any{
		"uid": r.UID, "flags": append([]string{}, r.Flags...), "modseq": r.Modseq,
		"size": r.Size, "internalDate": r.InternalDate,
	}
	if withLive {
		m["live"] = r.Live
	}
	return m
}

type entriesIn struct {
	folder   string
	afterUID uint32
	limit    int
}

func parseEntries(b body) (in entriesIn, ok bool) {
	if !onlyKeys(b, "folder", "afterUid", "limit") {
		return in, false
	}
	folder, ok1 := text(b["folder"], maxFolderChars)
	limit, ok2 := integer(b["limit"], 1, maxRows)
	if !ok1 || !ok2 {
		return in, false
	}
	in.folder, in.limit = folder, int(limit)
	if v := b["afterUid"]; v != nil { // missing or null: from the start
		after, ok := integer(v, 0, maxUID)
		if !ok {
			return in, false
		}
		in.afterUID = uint32(after)
	}
	return in, true
}

func (s *Stub) entries(se *session, in entriesIn) (any, *apiError) {
	f := se.box.folders[in.folder]
	if f == nil {
		return nil, fail(http.StatusNotFound, "folder")
	}
	rows := []map[string]any{}
	more := false
	for _, r := range f.rows {
		if !r.Live || r.UID <= in.afterUID {
			continue
		}
		if len(rows) == in.limit {
			more = true
			break
		}
		rows = append(rows, rowJSON(r, false))
	}
	return map[string]any{
		"rows": rows, "more": more, "uidValidity": f.uidValidity,
		"uidNext": f.uidNext, "highestModseq": f.modseq,
	}, nil
}

type changesIn struct {
	folder string
	since  int64
	limit  int
}

func parseChanges(b body) (in changesIn, ok bool) {
	if !onlyKeys(b, "folder", "sinceModseq", "limit") {
		return in, false
	}
	folder, ok1 := text(b["folder"], maxFolderChars)
	since, ok2 := integer(b["sinceModseq"], 0, maxSafeInteger)
	limit, ok3 := integer(b["limit"], 1, maxRows)
	if !ok1 || !ok2 || !ok3 {
		return in, false
	}
	return changesIn{folder: folder, since: since, limit: int(limit)}, true
}

func (s *Stub) changes(se *session, in changesIn) (any, *apiError) {
	f := se.box.folders[in.folder]
	if f == nil {
		return nil, fail(http.StatusNotFound, "folder")
	}
	// Too old to continue from, or a state this folder never had.
	if in.since < f.floor || in.since > f.modseq {
		return nil, fail(http.StatusGone, "resync")
	}
	var changed []*Row
	for _, r := range f.rows {
		if r.Modseq > in.since {
			changed = append(changed, r)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Modseq < changed[j].Modseq })
	more := false
	if len(changed) > in.limit {
		changed = changed[:in.limit]
		more = true
	}
	rows := []map[string]any{}
	for _, r := range changed {
		rows = append(rows, rowJSON(r, true))
	}
	return map[string]any{
		"rows": rows, "more": more, "uidValidity": f.uidValidity,
		"uidNext": f.uidNext, "highestModseq": f.modseq,
	}, nil
}

func (s *Stub) handleRaw(w http.ResponseWriter, r *http.Request, b body, fault *Fault) {
	const path = "/raw"
	folderName, ok1 := text(b["folder"], maxFolderChars)
	uid, ok2 := integer(b["uid"], 1, maxUID)
	uidValidity, ok3 := integer(b["uidValidity"], 1, maxUID)
	if !onlyKeys(b, "folder", "uid", "uidValidity") || !ok1 || !ok2 || !ok3 {
		s.refuse(w, path, errRequest)
		return
	}
	s.mu.Lock()
	se := s.sessionLocked(r.Header.Get("X-Bridge-Session"))
	if se == nil {
		s.mu.Unlock()
		s.answer(w, path, fail(http.StatusUnauthorized, "session"))
		return
	}
	var raw []byte
	found, renumbered := false, false
	// An unknown folder has no mails: "gone", like a number that is not there.
	if f := se.box.folders[folderName]; f != nil {
		if uint32(uidValidity) != f.uidValidity {
			// the uid belongs to a numbering that is no longer valid
			renumbered = true
		} else if row := f.find(uint32(uid)); row != nil && row.Live {
			raw, found = row.Raw, true
		}
	}
	if found {
		s.note(path, http.StatusOK)
	}
	s.mu.Unlock()
	if fault != nil && fault.Late && fault.Delay > 0 {
		// The answer was made up; it only leaves later.
		select {
		case <-time.After(fault.Delay):
		case <-r.Context().Done():
			return
		}
	}
	if renumbered {
		s.answer(w, path, fail(http.StatusConflict, "uidvalidity"))
		return
	}
	if !found {
		s.answer(w, path, fail(http.StatusNotFound, "gone"))
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	if fault != nil && fault.Cut {
		_, _ = w.Write(raw[:len(raw)/2])
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
			}
		}
		return
	}
	_, _ = w.Write(raw)
}

type storeIn struct {
	folder      string
	uids        []uint32
	add, remove []string
}

func parseStore(b body) (in storeIn, ok bool) {
	if !onlyKeys(b, "folder", "uids", "add", "remove") {
		return in, false
	}
	folder, ok1 := text(b["folder"], maxFolderChars)
	uids, ok2 := uidList(b["uids"])
	add, ok3 := flagList(b["add"])
	remove, ok4 := flagList(b["remove"])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return in, false
	}
	return storeIn{folder: folder, uids: uids, add: add, remove: remove}, true
}

func (s *Stub) store(se *session, in storeIn) (any, *apiError) {
	f := se.box.folders[in.folder]
	if f == nil {
		return nil, fail(http.StatusNotFound, "folder")
	}
	rows := []map[string]any{}
	for _, uid := range in.uids {
		r := f.find(uid)
		if r == nil || !r.Live {
			continue
		}
		if applyFlags(r, in.add, in.remove) {
			s.bump(se.box, f, r)
			rows = append(rows, map[string]any{"uid": r.UID, "flags": append([]string{}, r.Flags...), "modseq": r.Modseq})
		}
	}
	return map[string]any{"rows": rows}, nil
}

type expungeIn struct {
	folder string
	uids   []uint32
}

func parseExpunge(b body) (in expungeIn, ok bool) {
	if !onlyKeys(b, "folder", "uids") {
		return in, false
	}
	folder, ok1 := text(b["folder"], maxFolderChars)
	uids, ok2 := uidList(b["uids"])
	if !ok1 || !ok2 {
		return in, false
	}
	return expungeIn{folder: folder, uids: uids}, true
}

func (s *Stub) expunge(se *session, in expungeIn) (any, *apiError) {
	f := se.box.folders[in.folder]
	if f == nil {
		return nil, fail(http.StatusNotFound, "folder")
	}
	gone := []uint32{}
	for _, uid := range in.uids {
		r := f.find(uid)
		if r == nil || !r.Live {
			continue
		}
		r.Live = false
		s.bump(se.box, f, r)
		gone = append(gone, uid)
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i] < gone[j] })
	return map[string]any{"uids": gone}, nil
}

type appendIn struct {
	folder    string
	messageID string // as sent
	norm      string // normalized
	digest    string
}

func parseAppend(b body) (in appendIn, ok bool) {
	if !onlyKeys(b, "folder", "messageId", "digest") {
		return in, false
	}
	folder, ok1 := text(b["folder"], maxFolderChars)
	rawID, ok2 := text(b["messageId"], maxMessageIDChars)
	digest, _ := b["digest"].(string)
	digest = strings.ToLower(digest)
	if !ok1 || !ok2 || !isHex64(digest) {
		return in, false
	}
	norm, ok3 := normalizeMessageIDStrict(rawID)
	if !ok3 {
		return in, false
	}
	return appendIn{folder: folder, messageID: rawID, norm: norm, digest: digest}, true
}

func (s *Stub) append(se *session, in appendIn) (any, *apiError) {
	if in.folder != Sent {
		return nil, fail(http.StatusForbidden, "folder")
	}
	s.seen.AppendDigest = in.digest
	s.seen.AppendMessageID = in.messageID
	id := NormalizeMessageID(in.messageID)
	f := se.box.folders[Sent]
	if f != nil && id != "" {
		for _, r := range f.rows {
			if r.Live && r.MessageID == id {
				return map[string]any{"uid": r.UID, "uidValidity": f.uidValidity}, nil
			}
		}
	}
	return nil, fail(http.StatusConflict, "unknown")
}

func (s *Stub) submit(se *session, r *http.Request, raw []byte) (any, *apiError) {
	from := r.Header.Get("X-Envelope-From")
	var to []string
	if json.Unmarshal([]byte(r.Header.Get("X-Envelope-To")), &to) != nil || len(to) == 0 {
		return nil, &apiError{status: 400, body: map[string]any{"error": "envelope", "smtpCode": 550, "message": "envelope missing"}}
	}
	s.seen.SubmitFrom = from
	s.seen.SubmitTo = append([]string(nil), to...)
	reject := func(status, code int, errc, msg string) (any, *apiError) {
		return nil, &apiError{status: status, body: map[string]any{"error": errc, "smtpCode": code, "message": msg}}
	}
	if s.boxes[key(from)] != se.box {
		return reject(422, 550, "sender", "sender is not the mailbox")
	}
	if len(raw) > s.MaxSubmitBytes {
		return reject(413, 552, "size", "message too large")
	}
	if s.boxes[key(firstAddress(HeaderValue(raw, "From")))] != se.box {
		return reject(422, 550, "sender", "From header is not the mailbox")
	}
	// The envelope recipients have to be exactly To and Cc.
	want := map[string]bool{}
	for _, h := range []string{"To", "Cc"} {
		for _, a := range addressList(HeaderValue(raw, h)) {
			want[key(a)] = true
		}
	}
	got := map[string]bool{}
	for _, a := range to {
		got[key(a)] = true
	}
	if len(want) != len(got) {
		return reject(422, 550, "envelope", "envelope does not match To and Cc")
	}
	for a := range got {
		if !want[a] {
			return reject(422, 550, "envelope", "envelope does not match To and Cc")
		}
	}

	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	id := NormalizeMessageID(HeaderValue(raw, "Message-ID"))
	if id != "" {
		if rec, ok := se.box.submits[id]; ok {
			if rec.digest != digest {
				return reject(422, 550, "duplicate", "same Message-ID with different content")
			}
			if rec.status == "sent" {
				return map[string]any{"status": "sent", "uid": rec.uid}, nil
			}
			return map[string]any{"status": "unknown"}, nil
		}
	}

	res := &SubmitResult{Status: "sent"}
	if s.SubmitHook != nil {
		if hr := s.SubmitHook(from, to, raw); hr != nil {
			res = hr
		}
	}
	switch res.Status {
	case "sent":
		uid := s.deliverLocked(se.box, Sent, raw, s.now(), []string{`\Seen`})
		if id != "" {
			se.box.submits[id] = submitRecord{digest: digest, uid: uid, status: "sent"}
		}
		if s.OnSent != nil {
			cb, f, t, rawCopy := s.OnSent, from, append([]string(nil), to...), append([]byte(nil), raw...)
			// Outside the lock: the callback delivers into other mailboxes.
			go cb(f, t, rawCopy)
		}
		return map[string]any{"status": "sent", "uid": uid}, nil
	case "unknown":
		if id != "" {
			se.box.submits[id] = submitRecord{digest: digest, status: "unknown"}
		}
		return map[string]any{"status": "unknown"}, nil
	default:
		status := res.HTTP
		if status == 0 {
			status = 500
		}
		return reject(status, res.SMTPCode, res.Error, res.Message)
	}
}

// ---- small mail helpers (the stub needs no real parser) ------------------

// HeaderValue returns the unfolded value of the first header field with the
// given name, or "".
func HeaderValue(raw []byte, name string) string {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if alt := bytes.Index(raw, []byte("\n\n")); end < 0 || (alt >= 0 && alt < end) {
		end = alt
	}
	head := raw
	if end >= 0 {
		head = raw[:end]
	}
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	prefix := strings.ToLower(name) + ":"
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.ToLower(lines[i]), prefix) {
			continue
		}
		v := lines[i][len(prefix):]
		for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.HasPrefix(lines[i+1], "\t")) {
			i++
			v += " " + strings.TrimSpace(lines[i])
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// NormalizeMessageID strips angle brackets and blanks.
func NormalizeMessageID(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '<'); i >= 0 {
		if j := strings.IndexByte(v[i:], '>'); j > 0 {
			v = v[i+1 : i+j]
		}
	}
	return strings.TrimSpace(v)
}

func firstAddress(v string) string {
	l := addressList(v)
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

// addressList pulls bare addresses out of a header value. Good enough for
// test mails: "Name <a@b>, c@d".
func addressList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.LastIndexByte(part, '<'); i >= 0 {
			if j := strings.IndexByte(part[i:], '>'); j > 0 {
				part = part[i+1 : i+j]
			}
		}
		if strings.Contains(part, "@") {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}
