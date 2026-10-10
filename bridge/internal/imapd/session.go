package imapd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/guard"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/mirror"
)

const delim rune = '/'

// The two folders of stage 1.
var folderList = []struct {
	name  string
	attrs []imap.MailboxAttr
}{
	{edge.FolderInbox, []imap.MailboxAttr{imap.MailboxAttrHasNoChildren}},
	{edge.FolderSent, []imap.MailboxAttr{imap.MailboxAttrHasNoChildren, imap.MailboxAttrSent}},
}

func canonicalFolder(name string) (string, bool) {
	if strings.EqualFold(name, edge.FolderInbox) {
		return edge.FolderInbox, true
	}
	if name == edge.FolderSent {
		return edge.FolderSent, true
	}
	return "", false
}

func no(code imap.ResponseCode, text string) *imap.Error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Code: code, Text: text}
}

var (
	errNoMailbox   = no(imap.ResponseCodeNonExistent, "No such mailbox")
	errFixed       = no(imap.ResponseCodeCannot, "Folders are fixed on this server")
	errNoCopy      = no(imap.ResponseCodeCannot, "Copying and moving mail is not supported on this server")
	errUnavailable = no(imap.ResponseCodeUnavailable, "Temporarily unavailable, try again")
	errSessionOver = no(imap.ResponseCodeUnavailable, "Session ended, please log in again")
	errBusy        = no(imap.ResponseCodeUnavailable, "Server is busy, try again later")
	errReset       = no(imap.ResponseCodeUnavailable, "Mailbox was reset, please reconnect")
	errReadOnly    = no("", "Mailbox is read-only")
	errAppendOther = no(imap.ResponseCodeCannot, "Only mail sent through this account can be stored, and only in Sent")
	errNoBinary    = no(imap.ResponseCodeCannot, "BINARY is not supported")
	errBadFlag     = no(imap.ResponseCodeCannot, "Flag cannot be stored")
	errAuthzid     = no(imap.ResponseCodeAuthorizationFailed, "Authorization identity must be empty or the user itself")
	errPartDepth   = &imap.Error{Type: imap.StatusResponseTypeBad, Code: imap.ResponseCodeClientBug, Text: "Body section is nested too deep"}
)

func mapErr(err error) error {
	switch err {
	case nil:
		return nil
	case mirror.ErrSession:
		return errSessionOver
	case mirror.ErrBudget:
		return errBusy
	case mirror.ErrViewDead:
		return errReset
	}
	if _, ok := err.(*imap.Error); ok {
		return err
	}
	return errUnavailable
}

// session is one IMAP connection.
type session struct {
	srv  *Server
	conn *imapserver.Conn
	g    *guard.Conn

	failures int
	member   *mirror.Member
	expiry   *time.Timer

	// The selected folder. The library runs IDLE in its own goroutine and
	// may close the session while that goroutine starts, hence atomic.
	viewPtr atomic.Pointer[mirror.View]

	byeOnce sync.Once
}

var (
	_ imapserver.SessionNamespace   = (*session)(nil)
	_ imapserver.SessionMove        = (*session)(nil)
	_ imapserver.SessionSASL        = (*session)(nil)
	_ imapserver.SessionAppendLimit = (*session)(nil)
)

// AppendLimit is announced as APPENDLIMIT and enforced by the library
// before a literal is accepted.
func (s *session) AppendLimit() uint32 { return s.srv.cfg.AppendMaxBytes }

func (s *session) Close() error {
	s.closeView()
	if s.expiry != nil {
		s.expiry.Stop()
	}
	if s.member != nil {
		s.member.Detach()
	}
	s.srv.forget(s)
	return nil
}

func (s *session) view() *mirror.View { return s.viewPtr.Load() }

func (s *session) closeView() {
	if v := s.viewPtr.Swap(nil); v != nil {
		v.Close()
	}
}

// bye writes BYE and closes the connection. It may be called from any
// goroutine. BYE has to wait for a response that is being written; after
// grace the connection is closed regardless, so a client that does not
// read cannot hold it up.
func (s *session) bye(text string, grace time.Duration) {
	s.byeOnce.Do(func() {
		t := time.AfterFunc(grace, func() { _ = s.conn.NetConn().Close() })
		defer t.Stop()
		_ = s.conn.Bye(text)
	})
}

// kill is called by the mirror when this connection must end: the edge no
// longer accepts its session, the session ran out, or its folder was reset.
func (s *session) kill(code string) {
	hash := ""
	if s.member != nil {
		hash = s.member.Hash()
	}
	text := "Session ended, please log in again"
	switch code {
	case "expired":
		s.srv.m.IMAP.SessionsExpired.Add(1)
	case "session":
		s.srv.m.IMAP.SessionsEnded.Add(1)
	default:
		text = "Mailbox was reset, please reconnect"
	}
	s.srv.log.Event("imap_conn_ended", logx.Code(code), logx.MB(hash))
	// A session that is over gets no time to finish what it is writing.
	go s.bye(text, 300*time.Millisecond)
}

// ---- login ---------------------------------------------------------------

func validCredential(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// failed counts a failed attempt; after the configured number the
// connection closes right after the answer.
func (s *session) failed(err error) error {
	s.failures++
	if s.failures >= s.srv.cfg.MaxAuthFailures {
		s.srv.m.IMAP.AuthLockouts.Add(1)
		s.srv.log.Event("imap_conn_ended", logx.Code("auth_failures"))
		if s.g != nil {
			s.g.CloseAfterNextWrite()
		}
	}
	return err
}

func (s *session) Login(username, password string) error {
	m := s.srv.m
	username = strings.TrimSpace(username)
	// What the edge would not even take as an attempt (empty, too long)
	// cannot be a valid login: answered here, not asked there.
	if !validCredential(username, edge.MaxAddressChars) || !validCredential(password, edge.MaxPasswordChars) ||
		!edge.LoginInput(username, password) {
		m.IMAP.LoginsFailed.Add(1)
		return s.failed(imapserver.ErrAuthFailed)
	}
	var client netip.Addr
	if s.g != nil {
		client = s.g.ClientIP()
	}
	clientIP := edge.ClientIP(client)
	errThrottled := no(imap.ResponseCodeUnavailable, "Too many attempts, try again later")
	if s.srv.gate.Blocked(username, clientIP, password) {
		// The edge said "later" to exactly this attempt; asking again
		// before that only costs. Another password is asked: a valid one
		// is never throttled.
		m.IMAP.LoginsThrottled.Add(1)
		return s.failed(errThrottled)
	}
	ctx, cancel := ctxFor(s.srv.cfg.CallTimeout)
	defer cancel()
	sess, err := s.srv.edge.Login(ctx, username, password, client)
	if err != nil {
		switch {
		case edge.Is(err, 401, "auth"):
			m.IMAP.LoginsFailed.Add(1)
			s.srv.log.Event("imap_login_failed", logx.Code("auth"), logx.Addr(logx.HashAddress(username)))
			return s.failed(imapserver.ErrAuthFailed)
		case edge.Is(err, 429, "throttled"):
			s.srv.gate.Block(username, clientIP, password, edge.RetryAfterOf(err))
			m.IMAP.LoginsThrottled.Add(1)
			s.srv.log.Event("imap_login_failed", logx.Code("throttled"), logx.Addr(logx.HashAddress(username)))
			return s.failed(errThrottled)
		case edge.Is(err, 503, "preparing"):
			m.IMAP.LoginsPreparing.Add(1)
			return s.failed(no(imap.ResponseCodeUnavailable, "Mailbox is being prepared, try again in a few minutes"))
		}
		m.IMAP.LoginsError.Add(1)
		s.srv.log.Event("imap_login_failed", logx.Code("edge_"+edge.CodeOf(err)))
		return s.failed(errUnavailable)
	}
	// The password is not kept: from here on the connection only has its
	// session. Which mailbox that is, the edge has said; mirror and cache
	// hang on that identifier, not on what the client typed as address.
	s.member = s.srv.store.Attach(sess.Mailbox, sess.Token, sess.ExpiresAt, s.kill)
	if s.g != nil {
		s.g.SetAuthenticated()
	}
	if !sess.ExpiresAt.IsZero() {
		member := s.member
		s.expiry = time.AfterFunc(time.Until(sess.ExpiresAt), func() { member.Kill("expired") })
	}
	m.IMAP.LoginsOK.Add(1)
	m.Mailbox(s.member.Hash(), func(mb *metrics.Mailbox) { mb.Logins++ })
	s.srv.log.Event("imap_login", logx.MB(s.member.Hash()), logx.Addr(logx.HashAddress(username)))
	return nil
}

func (s *session) AuthenticateMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Authenticate(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, s.failed(no("", "SASL mechanism not supported"))
	}
	return &plainServer{s: s}, nil
}

// plainServer is SASL PLAIN with every failure counted against the
// connection. The authorization identity has to be empty or the user.
type plainServer struct {
	s    *session
	done bool
}

func (p *plainServer) Next(response []byte) ([]byte, bool, error) {
	if p.done {
		return nil, false, p.s.failed(imapserver.ErrAuthFailed)
	}
	if response == nil {
		return []byte{}, false, nil // ask for the response
	}
	p.done = true
	parts := bytes.Split(response, []byte{0})
	if len(parts) != 3 {
		p.s.srv.m.IMAP.LoginsFailed.Add(1)
		return nil, false, p.s.failed(&imap.Error{Type: imap.StatusResponseTypeBad, Text: "Malformed SASL response"})
	}
	authzid, user, pass := string(parts[0]), string(parts[1]), string(parts[2])
	if authzid != "" && !strings.EqualFold(strings.TrimSpace(authzid), strings.TrimSpace(user)) {
		p.s.srv.m.IMAP.LoginsFailed.Add(1)
		return nil, false, p.s.failed(errAuthzid)
	}
	if err := p.s.Login(user, pass); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// fresh makes sure this connection's own session was confirmed by the edge
// recently. Nothing from the mirror or the raw cache is handed out without.
func (s *session) fresh() error {
	if s.member == nil {
		return errSessionOver
	}
	ctx, cancel := ctxFor(s.srv.cfg.CallTimeout)
	defer cancel()
	return mapErr(s.member.EnsureFresh(ctx))
}

// ---- folders -------------------------------------------------------------

func (s *session) Create(string, *imap.CreateOptions) error         { return errFixed }
func (s *session) Delete(string) error                              { return errFixed }
func (s *session) Rename(string, string, *imap.RenameOptions) error { return errFixed }

// Subscribe is fine for the two folders (they are always subscribed);
// everything that would change subscriptions is refused.
func (s *session) Subscribe(mailbox string) error {
	if _, ok := canonicalFolder(mailbox); ok {
		return nil
	}
	return errFixed
}

func (s *session) Unsubscribe(string) error { return errFixed }

func (s *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: delim}}}, nil
}

func (s *session) folders() (map[string]edge.Folder, error) {
	ctx, cancel := ctxFor(s.srv.cfg.CallTimeout)
	defer cancel()
	l, err := s.srv.edge.Folders(ctx, s.member.Caller())
	if err = s.check(err); err != nil {
		return nil, err
	}
	out := make(map[string]edge.Folder, len(l))
	for _, f := range l {
		out[f.Name] = f
	}
	return out, nil
}

// check handles the answer to an edge call made with this connection's
// session: a rejected session ends this connection, and only this one.
func (s *session) check(err error) error {
	if err == nil {
		return nil
	}
	if edge.IsSession(err) {
		s.member.Kill("session")
		return errSessionOver
	}
	return errUnavailable
}

func (s *session) status(name string, info edge.Folder, o *imap.StatusOptions) (*imap.StatusData, error) {
	if o.NumDeleted || o.Size || o.DeletedStorage {
		return nil, no(imap.ResponseCodeCannot, "This STATUS item is not supported")
	}
	d := &imap.StatusData{Mailbox: name}
	if o.NumMessages {
		n := info.Exists
		d.NumMessages = &n
	}
	if o.NumUnseen {
		n := info.Unseen
		d.NumUnseen = &n
	}
	if o.NumRecent {
		n := uint32(0)
		d.NumRecent = &n
	}
	if o.UIDNext {
		d.UIDNext = imap.UID(info.UIDNext)
	}
	if o.UIDValidity {
		d.UIDValidity = info.UIDValidity
	}
	if o.AppendLimit {
		n := s.srv.cfg.AppendMaxBytes
		d.AppendLimit = &n
	}
	return d, nil
}

func (s *session) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	name, ok := canonicalFolder(mailbox)
	if !ok {
		return nil, errNoMailbox
	}
	if err := s.fresh(); err != nil {
		return nil, err
	}
	infos, err := s.folders()
	if err != nil {
		return nil, err
	}
	info, ok := infos[name]
	if !ok {
		return nil, errUnavailable
	}
	return s.status(name, info, options)
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if err := s.fresh(); err != nil {
		return err
	}
	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: delim})
	}
	// The pattern matcher of the library is exponential in the number of
	// wildcards; our names are five characters long, nothing sensible needs
	// long patterns.
	if len(patterns) > 8 || len(ref) > 64 {
		return nil
	}
	for _, p := range patterns {
		if len(p) > 64 || strings.Count(p, "*")+strings.Count(p, "%") > 4 {
			return nil
		}
	}
	var infos map[string]edge.Folder
	if options.ReturnStatus != nil {
		var err error
		if infos, err = s.folders(); err != nil {
			return err
		}
	}
	for _, f := range folderList {
		match := false
		for _, p := range patterns {
			if imapserver.MatchList(f.name, delim, ref, p) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		special := len(f.attrs) > 1
		if options.SelectSpecialUse && !special {
			continue
		}
		d := &imap.ListData{Mailbox: f.name, Delim: delim}
		if options.SelectSubscribed || options.ReturnSubscribed {
			d.Attrs = append(d.Attrs, imap.MailboxAttrSubscribed)
		}
		// The special-use attribute is always sent, asked for or not.
		d.Attrs = append(d.Attrs, f.attrs...)
		if options.ReturnStatus != nil {
			if info, ok := infos[f.name]; ok {
				st, err := s.status(f.name, info, options.ReturnStatus)
				if err != nil {
					return err
				}
				d.Status = st
			}
		}
		if err := w.WriteList(d); err != nil {
			return err
		}
	}
	return nil
}

// ---- select --------------------------------------------------------------

var systemFlagList = []imap.Flag{imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagSeen, imap.FlagDraft}

func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	name, ok := canonicalFolder(mailbox)
	if !ok {
		return nil, errNoMailbox
	}
	if err := s.fresh(); err != nil {
		return nil, err
	}
	s.closeView()
	f := s.member.Folder(name)
	ctx, cancel := ctxFor(4 * s.srv.cfg.CallTimeout)
	defer cancel()
	if err := f.Sync(ctx, s.member); err != nil {
		return nil, mapErr(err)
	}
	readOnly := options != nil && options.ReadOnly
	v, sel, err := f.Open(s.member, readOnly)
	if err != nil {
		return nil, mapErr(err)
	}
	s.viewPtr.Store(v)
	flags := append(append([]imap.Flag{}, systemFlagList...), sel.Keywords...)
	perm := []imap.Flag{}
	if !readOnly {
		perm = append(append(perm, flags...), imap.FlagWildcard)
	}
	return &imap.SelectData{
		Flags:             flags,
		PermanentFlags:    perm,
		NumMessages:       sel.NumMessages,
		FirstUnseenSeqNum: sel.FirstUnseen,
		UIDNext:           imap.UID(sel.UIDNext),
		UIDValidity:       sel.UIDValidity,
	}, nil
}

func (s *session) Unselect() error {
	s.closeView()
	return nil
}

func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	v := s.view()
	if v == nil || s.member == nil || !s.member.IsFresh() {
		return nil
	}
	return v.Poll(w, allowExpunge)
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	v := s.view()
	if v == nil {
		<-stop
		return nil
	}
	var tick <-chan time.Time
	if d := s.srv.cfg.IdleKeepalive; d > 0 {
		t := time.NewTicker(d)
		defer t.Stop()
		tick = t.C
	}
	// Updates are only written while this connection's session is
	// confirmed. If it is not at the moment (the poll is late), the updates
	// stay queued and are tried again shortly; they must not wait for the
	// next change in the folder.
	var retry <-chan time.Time
	poll := func() error {
		retry = nil
		if v.Dead() {
			return nil
		}
		if !s.member.IsFresh() {
			if !s.member.Killed() {
				retry = time.After(time.Second)
			}
			return nil
		}
		return v.Poll(w, true)
	}
	if err := poll(); err != nil {
		return err
	}
	for {
		select {
		case <-stop:
			return nil
		case <-v.Notify():
			if err := poll(); err != nil {
				return err
			}
		case <-retry:
			if err := poll(); err != nil {
				return err
			}
		case <-tick:
			// A harmless line so that a proxy does not cut a silent
			// connection: the flag list of the folder, unchanged.
			if v.Dead() || !s.member.IsFresh() {
				continue
			}
			flags := append(append([]imap.Flag{}, systemFlagList...), v.Keywords()...)
			if err := w.WriteMailboxFlags(flags); err != nil {
				return err
			}
			s.srv.m.IdleKeepalives.Add(1)
		}
	}
}

// ---- fetch ---------------------------------------------------------------

func (s *session) ranges(set imap.NumSet) ([]mirror.Range, bool, error) {
	return toRanges(set, s.srv.cfg.MaxSetRanges)
}

type fetched struct {
	item mirror.Item
	raw  []byte
	err  error
}

// loadRaw fetches the raw mails of a batch, a few at a time.
func (s *session) loadRaw(ctx context.Context, v *mirror.View, batch []fetched) {
	const parallel = 4
	if len(batch) == 1 {
		b := &batch[0]
		b.raw, b.err = s.member.Raw(ctx, v.Folder(), v.UIDValidity(), b.item.Msg.UID, b.item.Msg.Size)
		return
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range batch {
		wg.Add(1)
		sem <- struct{}{}
		go func(b *fetched) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if recover() != nil {
					b.err = mirror.ErrUnavailable
				}
			}()
			b.raw, b.err = s.member.Raw(ctx, v.Folder(), v.UIDValidity(), b.item.Msg.UID, b.item.Msg.Size)
		}(&batch[i])
	}
	wg.Wait()
}

// heavy takes one of the few slots for work on a large mail (walking its
// structure, cutting out a part) and returns the function that gives it
// back.
func (s *session) heavy() func() {
	s.srv.parseSem <- struct{}{}
	return func() { <-s.srv.parseSem }
}

func (s *session) structure(p *parsed) imap.BodyStructure {
	if len(p.raw) > 256<<10 {
		defer s.heavy()()
	}
	return p.bodyStructure()
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	v := s.view()
	if v == nil {
		return errReset
	}
	if hook := s.srv.cfg.FetchHook; hook != nil {
		hook()
	}
	if err := s.fresh(); err != nil {
		return err
	}
	if len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0 {
		return errNoBinary
	}
	opts := *options
	if len(opts.BodySection) > maxSectionsPerFetch {
		return errSectionBudget
	}
	markSeen := false
	for _, bs := range opts.BodySection {
		if len(bs.Part) > maxSectionDepth {
			return errPartDepth
		}
		if !bs.Peek {
			markSeen = true
		}
	}
	set, byUID, err := s.ranges(numSet)
	if err != nil {
		return err
	}
	items, err := v.Resolve(byUID, set)
	if err != nil {
		return mapErr(err)
	}

	// BODY[...] without PEEK marks the mail as read, except on EXAMINE.
	if markSeen && !v.ReadOnly {
		var uids []uint32
		for i := range items {
			if !mirror.HasFlag(items[i].Msg.Flags, imap.FlagSeen) {
				uids = append(uids, items[i].Msg.UID)
			}
		}
		if len(uids) > 0 {
			if err := s.storeFlags(v, uids, []string{string(imap.FlagSeen)}, nil); err != nil {
				return err
			}
			if items, err = v.Resolve(byUID, set); err != nil {
				return mapErr(err)
			}
			opts.Flags = true
		}
	}

	needRaw := opts.Envelope || opts.BodyStructure != nil || len(opts.BodySection) > 0
	if !needRaw {
		for i := range items {
			if err := writeFetch(w, &items[i], nil, &opts, nil); err != nil {
				return err
			}
		}
		return nil
	}

	// Raw mails are fetched in small batches (bounded by count and bytes),
	// everything for one answer is computed first, and only then written:
	// nothing slow happens while a response is open. The bytes of a batch
	// count against the work memory budget until they are written.
	const (
		batchItems = 8
		batchBytes = 4 << 20
	)
	for i := 0; i < len(items); {
		j, size := i, int64(0)
		for j < len(items) && j-i < batchItems && (j == i || size+items[j].Msg.Size <= batchBytes) {
			size += items[j].Msg.Size + 4096
			j++
		}
		if err := s.fetchBatch(w, v, items[i:j], size, &opts); err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (s *session) fetchBatch(w *imapserver.FetchWriter, v *mirror.View, items []mirror.Item, size int64, opts *imap.FetchOptions) error {
	ctx, cancel := ctxFor(s.srv.cfg.RawTimeout)
	defer cancel()
	release, err := s.srv.work.Acquire(ctx, s.member.Hash(), size)
	if err != nil {
		return errBusy
	}
	defer release()
	// A long FETCH runs in many batches, and a batch may have waited for
	// memory. Mail is only handed out while this connection's own session
	// is confirmed: checked now, and again before every single mail.
	if err := s.fresh(); err != nil {
		return err
	}
	batch := make([]fetched, len(items))
	for k := range batch {
		batch[k].item = items[k]
	}
	s.loadRaw(ctx, v, batch)
	for k := range batch {
		b := &batch[k]
		if b.err == mirror.ErrGone {
			continue // removed meanwhile; the client hears EXPUNGE next
		}
		if b.err != nil {
			return mapErr(b.err)
		}
		if err := s.fresh(); err != nil {
			return err // nothing more once the session is over or unconfirmed
		}
		p := parseMessage(b.raw)
		if len(b.raw) > 256<<10 {
			// cutting a part out of a large mail: few at a time
			p.heavy = s.heavy
		}
		if err := writeFetch(w, &b.item, p, opts, s); err != nil {
			return err
		}
		b.raw = nil
	}
	return nil
}

func clampPartial(p *imap.SectionPartial) *imap.SectionPartial {
	if p == nil {
		return nil
	}
	cp := *p
	const max = int64(1) << 40
	if cp.Offset > max {
		cp.Offset = max
	}
	if cp.Size > max {
		cp.Size = max
	}
	return &cp
}

// writeFetch writes one FETCH response. All content is computed before the
// response is opened.
func writeFetch(w *imapserver.FetchWriter, it *mirror.Item, p *parsed, o *imap.FetchOptions, s *session) error {
	var (
		env      *imap.Envelope
		bs       imap.BodyStructure
		sections [][]byte
	)
	if p != nil {
		if o.Envelope {
			env = p.envelope()
		}
		if o.BodyStructure != nil {
			bs = s.structure(p)
		}
		for _, sec := range o.BodySection {
			cp := *sec
			cp.Partial = clampPartial(sec.Partial)
			b, err := p.section(&cp)
			if err != nil {
				return err
			}
			sections = append(sections, b)
		}
	}

	rw := w.CreateMessage(it.Seq)
	rw.WriteUID(imap.UID(it.Msg.UID))
	if o.Flags {
		rw.WriteFlags(it.Msg.Flags)
	}
	if o.InternalDate {
		rw.WriteInternalDate(time.UnixMilli(it.Msg.Date).UTC())
	}
	if o.RFC822Size {
		rw.WriteRFC822Size(it.Msg.Size)
	}
	if p != nil {
		if o.Envelope {
			rw.WriteEnvelope(env)
		}
		if o.BodyStructure != nil {
			rw.WriteBodyStructure(bs)
		}
		for i, sec := range o.BodySection {
			wc := rw.WriteBodySection(sec, int64(len(sections[i])))
			_, werr := wc.Write(sections[i])
			cerr := wc.Close()
			if werr != nil || cerr != nil {
				_ = rw.Close()
				if werr != nil {
					return werr
				}
				return cerr
			}
		}
	}
	return rw.Close()
}

// ---- store ---------------------------------------------------------------

// storeFlags sends a flag change to the edge (at most 500 uids per call)
// and applies the answer to the mirror.
// storeFlags changes flags at the edge in calls of the size the edge takes:
// at most edge.MaxStoreUIDs numbers and edge.MaxFlagsPerList flags per list.
func (s *session) storeFlags(v *mirror.View, uids []uint32, add, remove []string) error {
	for len(add) > 0 || len(remove) > 0 {
		a, r := add, remove
		if len(a) > edge.MaxFlagsPerList {
			a = a[:edge.MaxFlagsPerList]
		}
		if len(r) > edge.MaxFlagsPerList {
			r = r[:edge.MaxFlagsPerList]
		}
		add, remove = add[len(a):], remove[len(r):]
		for i := 0; i < len(uids); i += edge.MaxStoreUIDs {
			j := i + edge.MaxStoreUIDs
			if j > len(uids) {
				j = len(uids)
			}
			ctx, cancel := ctxFor(2 * s.srv.cfg.CallTimeout)
			err := v.Folder().Store(ctx, s.member, uids[i:j], a, r)
			cancel()
			if err != nil {
				return mapErr(err)
			}
		}
	}
	return nil
}

func cleanFlags(in []imap.Flag) ([]string, error) {
	if len(in) > 32 {
		return nil, errBadFlag
	}
	out := make([]string, 0, len(in))
	for _, f := range in {
		if strings.EqualFold(string(f), `\Recent`) {
			continue
		}
		if !mirror.ValidFlag(string(f)) {
			return nil, errBadFlag
		}
		out = append(out, mirror.CanonFlag(string(f)))
	}
	return out, nil
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	v := s.view()
	if v == nil {
		return errReset
	}
	if v.ReadOnly {
		return errReadOnly
	}
	if err := s.fresh(); err != nil {
		return err
	}
	want, err := cleanFlags(flags.Flags)
	if err != nil {
		return err
	}
	set, byUID, err := s.ranges(numSet)
	if err != nil {
		return err
	}
	items, err := v.Resolve(byUID, set)
	if err != nil {
		return mapErr(err)
	}
	switch flags.Op {
	case imap.StoreFlagsAdd, imap.StoreFlagsDel:
		if len(want) > 0 && len(items) > 0 {
			uids := make([]uint32, len(items))
			for i := range items {
				uids[i] = items[i].Msg.UID
			}
			var add, remove []string
			if flags.Op == imap.StoreFlagsAdd {
				add = want
			} else {
				remove = want
			}
			if err := s.storeFlags(v, uids, add, remove); err != nil {
				return err
			}
		}
	case imap.StoreFlagsSet:
		// FLAGS (...) replaces: per mail the difference to what it has now,
		// mails with the same difference in one call.
		type diff struct {
			add, remove []string
			uids        []uint32
		}
		groups := map[string]*diff{}
		wantSet := map[string]bool{}
		for _, f := range want {
			wantSet[f] = true
		}
		for i := range items {
			cur := map[string]bool{}
			var add, remove []string
			for _, f := range items[i].Msg.Flags {
				cur[string(f)] = true
				if !wantSet[string(f)] {
					remove = append(remove, string(f))
				}
			}
			for _, f := range want {
				if !cur[f] {
					add = append(add, f)
				}
			}
			if len(add)+len(remove) == 0 {
				continue
			}
			k := strings.Join(add, " ") + "|" + strings.Join(remove, " ")
			g := groups[k]
			if g == nil {
				g = &diff{add: add, remove: remove}
				groups[k] = g
			}
			g.uids = append(g.uids, items[i].Msg.UID)
		}
		for _, g := range groups {
			if err := s.storeFlags(v, g.uids, g.add, g.remove); err != nil {
				return err
			}
		}
	}
	if flags.Silent {
		return nil
	}
	if items, err = v.Resolve(byUID, set); err != nil {
		return mapErr(err)
	}
	for i := range items {
		rw := w.CreateMessage(items[i].Seq)
		rw.WriteUID(imap.UID(items[i].Msg.UID))
		rw.WriteFlags(items[i].Msg.Flags)
		if err := rw.Close(); err != nil {
			return err
		}
	}
	return nil
}

// ---- expunge, copy, move ---------------------------------------------------

// Expunge hides the mails flagged \Deleted (all, or those of a uid set) at
// the edge. The EXPUNGE responses come from the mirror after the command.
// On a folder opened with EXAMINE nothing happens.
func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	v := s.view()
	if v == nil || v.ReadOnly {
		return nil
	}
	if err := s.fresh(); err != nil {
		return err
	}
	var limit []mirror.Range
	if uids != nil {
		r, _, err := s.ranges(*uids)
		if err != nil {
			return err
		}
		if limit = r; limit == nil {
			limit = []mirror.Range{}
		}
	}
	del, err := v.Deleted(limit)
	if err != nil {
		return mapErr(err)
	}
	for i := 0; i < len(del); i += edge.MaxStoreUIDs {
		j := i + edge.MaxStoreUIDs
		if j > len(del) {
			j = len(del)
		}
		ctx, cancel := ctxFor(2 * s.srv.cfg.CallTimeout)
		err := v.Folder().Expunge(ctx, s.member, del[i:j])
		cancel()
		if err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (s *session) Copy(imap.NumSet, string) (*imap.CopyData, error) { return nil, errNoCopy }

// Move exists so that a client that tries MOVE anyway gets a clear NO; the
// capability is not announced.
func (s *session) Move(*imapserver.MoveWriter, imap.NumSet, string) error { return errNoCopy }

// ---- append --------------------------------------------------------------

// Append accepts only what is already there: a mail in Sent that was sent
// through us. The bytes are read (bounded) to get Message-ID and digest;
// they are not stored.
func (s *session) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	m := s.srv.m
	if name, ok := canonicalFolder(mailbox); !ok || name != edge.FolderSent {
		m.AppendsRejected.Add(1)
		return nil, errAppendOther
	}
	limit := int64(s.srv.cfg.AppendMaxBytes)
	if r.Size() > limit {
		m.AppendsRejected.Add(1)
		return nil, no(imap.ResponseCodeTooBig, "Message is too large")
	}
	bctx, bcancel := ctxFor(s.srv.cfg.CallTimeout)
	release, err := s.srv.work.Acquire(bctx, s.member.Hash(), r.Size())
	bcancel()
	if err != nil {
		return nil, errBusy
	}
	defer release()
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		m.AppendsRejected.Add(1)
		return nil, no(imap.ResponseCodeTooBig, "Message is too large")
	}
	if err := s.fresh(); err != nil {
		return nil, err
	}
	// Without a Message-ID the edge can look up there is nothing to ask:
	// such a mail was not sent through us.
	id := messageID(buf)
	if !edge.AppendableMessageID(id) {
		m.AppendsRejected.Add(1)
		s.srv.log.Event("imap_append_rejected", logx.Code("no_message_id"), logx.MB(s.member.Hash()))
		return nil, errAppendOther
	}
	sum := sha256.Sum256(buf)
	ctx, cancel := ctxFor(s.srv.cfg.CallTimeout)
	defer cancel()
	res, err := s.srv.edge.Append(ctx, s.member.Caller(), id, hex.EncodeToString(sum[:]))
	if err != nil {
		if edge.Is(err, 409, "unknown") || edge.Is(err, 403, "folder") {
			m.AppendsRejected.Add(1)
			s.srv.log.Event("imap_append_rejected", logx.Code(edge.CodeOf(err)), logx.MB(s.member.Hash()))
			return nil, errAppendOther
		}
		return nil, s.check(err)
	}
	m.AppendsKnown.Add(1)
	return &imap.AppendData{UID: imap.UID(res.UID), UIDValidity: res.UIDValidity}, nil
}

// ---- search --------------------------------------------------------------

func (s *session) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	v := s.view()
	if v == nil {
		return nil, errReset
	}
	if err := s.fresh(); err != nil {
		return nil, err
	}
	if options != nil && options.ReturnSave {
		return nil, errSearchRes
	}
	maxSeq, maxUID, err := v.Bounds()
	if err != nil {
		return nil, mapErr(err)
	}
	budget := &searchBudget{maxDepth: s.srv.cfg.SearchMaxDepth, nodes: s.srv.cfg.SearchMaxNodes, maxRanges: s.srv.cfg.MaxSetRanges}
	c, err := compile(criteria, 0, budget, maxSeq, maxUID)
	if err != nil {
		if err == errSearchContent {
			s.srv.m.SearchRejected.Add(1)
		}
		return nil, err
	}
	// Matches come in ascending order, so the result set is built as runs.
	var (
		data imap.SearchData
		runs [][2]uint32
	)
	err = v.Each(func(it mirror.Item) {
		if !c.match(&it) {
			return
		}
		num := it.Seq
		if kind == imapserver.NumKindUID {
			num = it.Msg.UID
		}
		if n := len(runs); n > 0 && runs[n-1][1]+1 == num {
			runs[n-1][1] = num
		} else {
			runs = append(runs, [2]uint32{num, num})
		}
		if data.Min == 0 {
			data.Min = num
		}
		data.Max = num
		data.Count++
	})
	if err != nil {
		return nil, mapErr(err)
	}
	if kind == imapserver.NumKindUID {
		set := make(imap.UIDSet, len(runs))
		for i, r := range runs {
			set[i] = imap.UIDRange{Start: imap.UID(r[0]), Stop: imap.UID(r[1])}
		}
		data.All = set
	} else {
		set := make(imap.SeqSet, len(runs))
		for i, r := range runs {
			set[i] = imap.SeqRange{Start: r[0], Stop: r[1]}
		}
		data.All = set
	}
	return &data, nil
}
