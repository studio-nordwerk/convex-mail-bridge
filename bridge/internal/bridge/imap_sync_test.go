package bridge_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

// ---- search --------------------------------------------------------------

func uidsOf(d *imap.SearchData) []uint32 {
	var out []uint32
	for _, u := range d.AllUIDs() {
		out = append(out, uint32(u))
	}
	return out
}

func TestSearchFromMirror(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 12, 0) // dates: 1 Sep 08:00 plus 6 h each; every third is read
	e.stub.SetFlags(mara, edgestub.Inbox, 5, []string{`\Flagged`, "$Important"}, nil)
	big := edgestub.BuildMail(edgestub.MailSpec{From: "x@outside.test", To: []string{mara}, Subject: "big", Text: string(make([]byte, 20000))})
	bigUID := e.stub.Deliver(mara, edgestub.Inbox, big, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	rows := e.stub.State(mara, edgestub.Inbox).Rows
	has := func(r edgestub.Row, f string) bool {
		for _, x := range r.Flags {
			if x == f {
				return true
			}
		}
		return false
	}
	day := func(r edgestub.Row) time.Time {
		tm := time.UnixMilli(r.InternalDate).UTC()
		return time.Date(tm.Year(), tm.Month(), tm.Day(), 0, 0, 0, 0, time.UTC)
	}
	want := func(pred func(i int, r edgestub.Row) bool) []uint32 {
		var out []uint32
		for i, r := range rows {
			if pred(i, r) {
				out = append(out, r.UID)
			}
		}
		return out
	}
	d2 := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	var uset imap.UIDSet
	uset.AddRange(3, 6)
	uset.AddNum(11)
	var star imap.UIDSet
	star.AddRange(12, 0)
	var sset imap.SeqSet
	sset.AddRange(2, 4)

	cases := []struct {
		name string
		crit imap.SearchCriteria
		want []uint32
	}{
		{"ALL", imap.SearchCriteria{}, want(func(int, edgestub.Row) bool { return true })},
		{"UNSEEN", imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, want(func(_ int, r edgestub.Row) bool { return !has(r, `\Seen`) })},
		{"SEEN", imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, want(func(_ int, r edgestub.Row) bool { return has(r, `\Seen`) })},
		{"FLAGGED KEYWORD", imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged, "$important"}}, []uint32{5}},
		{"UID 3:6,11", imap.SearchCriteria{UID: []imap.UIDSet{uset}}, []uint32{3, 4, 5, 6, 11}},
		{"UID 12:*", imap.SearchCriteria{UID: []imap.UIDSet{star}}, []uint32{12, bigUID}},
		{"2:4", imap.SearchCriteria{SeqNum: []imap.SeqSet{sset}}, []uint32{2, 3, 4}},
		{"SINCE 2-Sep", imap.SearchCriteria{Since: d2}, want(func(_ int, r edgestub.Row) bool { return !day(r).Before(d2) })},
		{"BEFORE 2-Sep", imap.SearchCriteria{Before: d2}, want(func(_ int, r edgestub.Row) bool { return day(r).Before(d2) })},
		{"ON 2-Sep", imap.SearchCriteria{Since: d2, Before: d2.Add(24 * time.Hour)}, want(func(_ int, r edgestub.Row) bool { return day(r).Equal(d2) })},
		{"LARGER 10000", imap.SearchCriteria{Larger: 10000}, []uint32{bigUID}},
		{"SMALLER 10000", imap.SearchCriteria{Smaller: 10000}, want(func(_ int, r edgestub.Row) bool { return r.Size < 10000 })},
		{"NOT SEEN SINCE", imap.SearchCriteria{Since: d2, Not: []imap.SearchCriteria{{Flag: []imap.Flag{imap.FlagSeen}}}},
			want(func(_ int, r edgestub.Row) bool { return !day(r).Before(d2) && !has(r, `\Seen`) })},
		{"OR FLAGGED LARGER", imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{{Flag: []imap.Flag{imap.FlagFlagged}}, {Larger: 10000}}}}, []uint32{5, bigUID}},
	}
	if len(want(func(_ int, r edgestub.Row) bool { return day(r).Equal(d2) })) == 0 {
		t.Fatal("fixture has no mail on 2 Sep")
	}
	for _, tc := range cases {
		crit := tc.crit
		data, err := c.UIDSearch(&crit, nil).Wait()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := uidsOf(data); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// sequence numbers as result
	data, err := c.Search(&imap.SearchCriteria{Larger: 10000}, nil).Wait()
	if err != nil || len(data.AllSeqNums()) != 1 || data.AllSeqNums()[0] != uint32(len(rows)) {
		t.Fatalf("SEARCH LARGER = %v %v", data.AllSeqNums(), err)
	}
	if e.stub.Calls("/raw") != 0 {
		t.Fatal("search fetched raw mails")
	}
}

func TestSearchContentIsRefused(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 5, 0)
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	hdr := func(k, v string) imap.SearchCriteria {
		return imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: k, Value: v}}}
	}
	when := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	cases := map[string]imap.SearchCriteria{
		"SUBJECT":    hdr("Subject", "Received"),
		"FROM":       hdr("From", "sender"),
		"TO":         hdr("To", "mara"),
		"HEADER":     hdr("X-Anything", ""),
		"TEXT":       {Text: []string{"Content"}},
		"BODY":       {Body: []string{"Content"}},
		"SENTSINCE":  {SentSince: when},
		"SENTBEFORE": {SentBefore: when},
		// hidden inside combinations: refused as a whole, never half a result
		"UNSEEN SUBJECT":  {NotFlag: []imap.Flag{imap.FlagSeen}, Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "x"}}},
		"NOT TEXT":        {Not: []imap.SearchCriteria{{Text: []string{"x"}}}},
		"OR SEEN SUBJECT": {Or: [][2]imap.SearchCriteria{{{Flag: []imap.Flag{imap.FlagSeen}}, hdr("Subject", "x")}}},
	}
	for name, crit := range cases {
		crit := crit
		_, err := c.UIDSearch(&crit, nil).Wait()
		wantNo(t, name, err, "CANNOT")
	}
	if n := e.metric("imap", "searchRejected"); n != int64(len(cases)) {
		t.Fatalf("searchRejected = %d, want %d", n, len(cases))
	}
	// too deep: refused, not evaluated
	deep := imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}
	for i := 0; i < 40; i++ {
		deep = imap.SearchCriteria{Not: []imap.SearchCriteria{deep}}
	}
	_, err := c.UIDSearch(&deep, nil).Wait()
	wantNo(t, "deep", err, "LIMIT")
	// wide instead of deep: a thousand flags on one level cost just as much
	wide := imap.SearchCriteria{}
	for i := 0; i < 1000; i++ {
		wide.NotFlag = append(wide.NotFlag, imap.FlagSeen)
	}
	_, err = c.UIDSearch(&wide, nil).Wait()
	wantNo(t, "wide", err, "LIMIT")
	if e.stub.Calls("/raw") != 0 {
		t.Fatal("a refused search fetched raw mails")
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
}

// ---- IDLE ----------------------------------------------------------------

type watcher struct {
	mu      sync.Mutex
	exists  []uint32
	expunge []uint32
	flags   map[uint32][]string // by uid
	flagsLn int                 // untagged FLAGS lines (keepalive)
}

func (w *watcher) opts() *imapclient.Options {
	w.flags = map[uint32][]string{}
	return &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Expunge: func(seq uint32) {
			w.mu.Lock()
			w.expunge = append(w.expunge, seq)
			w.mu.Unlock()
		},
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			w.mu.Lock()
			if d.NumMessages != nil {
				w.exists = append(w.exists, *d.NumMessages)
			}
			if d.Flags != nil {
				w.flagsLn++
			}
			w.mu.Unlock()
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			buf, err := msg.Collect()
			if err != nil {
				return
			}
			w.mu.Lock()
			w.flags[uint32(buf.UID)] = flagsOf(buf.Flags)
			w.mu.Unlock()
		},
	}}
}

func (w *watcher) snapshot() (exists, expunge []uint32, flags map[uint32][]string, flagLines int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	flags = map[uint32][]string{}
	for k, v := range w.flags {
		flags[k] = v
	}
	return append([]uint32(nil), w.exists...), append([]uint32(nil), w.expunge...), flags, w.flagsLn
}

func TestIdleGetsChanges(t *testing.T) {
	e := newEnv(t, withPoller())
	e.mailbox(mara, 5, 0)
	w := &watcher{}
	c := e.login(mara, w.opts())
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	idle, err := c.Idle()
	if err != nil {
		t.Fatal(err)
	}

	raw := edgestub.BuildMail(edgestub.MailSpec{From: "new@outside.test", To: []string{mara}, Subject: "fresh", Text: "new"})
	newUID := e.stub.Deliver(mara, edgestub.Inbox, raw, time.Now())
	eventually(t, 5*time.Second, "EXISTS 6 during IDLE", func() bool {
		ex, _, _, _ := w.snapshot()
		return len(ex) > 0 && ex[len(ex)-1] == 6
	})

	e.stub.SetFlags(mara, edgestub.Inbox, 2, []string{`\Seen`}, nil) // read in the console
	eventually(t, 5*time.Second, "FETCH FLAGS for uid 2 during IDLE", func() bool {
		_, _, fl, _ := w.snapshot()
		return sameStrings(fl[2], []string{`\Seen`})
	})

	e.stub.Remove(mara, edgestub.Inbox, 3) // deleted for good in the app
	eventually(t, 5*time.Second, "EXPUNGE 3 during IDLE", func() bool {
		_, xp, _, _ := w.snapshot()
		return len(xp) == 1 && xp[0] == 3
	})

	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatal(err)
	}
	compareWithStub(t, e, c, mara, edgestub.Inbox, "after IDLE")
	msgs, err := c.Fetch(imap.UIDSetNum(imap.UID(newUID)), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil || len(msgs) != 1 || sha(bodySection(msgs[0], 0)) != sha(raw) {
		t.Fatalf("the new mail is not readable: %v", err)
	}
}

func TestIdleKeepaliveLine(t *testing.T) {
	e := newEnv(t, withPoller(), set("test.keepalive_ms", "150"))
	e.mailbox(mara, 2, 0)
	w := &watcher{}
	c := e.login(mara, w.opts())
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	_, _, _, base := w.snapshot()
	idle, err := c.Idle()
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "keepalive lines during IDLE", func() bool {
		_, _, _, n := w.snapshot()
		return n >= base+2
	})
	ex, xp, _, _ := w.snapshot()
	if len(ex) != 0 || len(xp) != 0 {
		t.Fatalf("keepalive changed the folder for the client: EXISTS %v EXPUNGE %v", ex, xp)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	_ = idle.Wait()
	if n := e.metric("imap", "idleKeepalives"); n < 2 {
		t.Fatalf("idleKeepalives = %d", n)
	}
	compareWithStub(t, e, c, mara, edgestub.Inbox, "after keepalive")
}

// ---- mirror life cycle ---------------------------------------------------

func TestReconnectUsesTheMirror(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 30, 4)
	hash := e.mbHash(mara)

	c := e.login(mara, nil)
	for _, f := range []string{"INBOX", "Sent"} {
		if _, err := c.Select(f, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Logout().Wait()
	if n := e.metric("mailboxes", hash, "fullLoads"); n != 2 {
		t.Fatalf("full loads after the first visit = %d", n)
	}
	entries := e.stub.Calls("/entries")

	// things happen while nobody is connected
	raw := edgestub.BuildMail(edgestub.MailSpec{From: "late@outside.test", To: []string{mara}, Subject: "meanwhile", Text: "x"})
	e.stub.Deliver(mara, edgestub.Inbox, raw, time.Now())
	e.stub.SetFlags(mara, edgestub.Inbox, 4, []string{`\Seen`}, nil)
	e.stub.Remove(mara, edgestub.Inbox, 7)

	for round := 0; round < 3; round++ {
		c := e.login(mara, nil)
		for _, f := range []string{"INBOX", "Sent"} {
			if _, err := c.Select(f, nil).Wait(); err != nil {
				t.Fatal(err)
			}
			compareWithStub(t, e, c, mara, f, fmt.Sprintf("reconnect %d %s", round, f))
		}
		_ = c.Logout().Wait()
	}
	if got := e.stub.Calls("/entries"); got != entries {
		t.Fatalf("reconnecting read the whole folder again (%d more /entries calls)", got-entries)
	}
	if n := e.metric("mailboxes", hash, "fullLoads"); n != 2 {
		t.Fatalf("full loads after reconnecting = %d", n)
	}
	if n := e.metric("mirror", "fullLoads"); n != 2 {
		t.Fatalf("mirror.fullLoads = %d", n)
	}
}

func TestResync410(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 10, 0)
	hash := e.mbHash(mara)
	w := &watcher{}
	c := e.login(mara, w.opts())
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// Changes pile up, then the edge forgets removed rows: the mirror is
	// too old for an incremental read.
	e.stub.Remove(mara, edgestub.Inbox, 2)
	e.stub.Remove(mara, edgestub.Inbox, 9)
	e.stub.SetFlags(mara, edgestub.Inbox, 5, []string{`\Flagged`}, nil)
	e.stub.Deliver(mara, edgestub.Inbox, edgestub.BuildMail(edgestub.MailSpec{From: "a@outside.test", To: []string{mara}, Subject: "after the cleanup", Text: "x"}), time.Now())
	e.stub.Prune(mara, edgestub.Inbox)

	e.sync()
	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("the connection did not survive the resync: %v", err)
	}
	if n := e.metric("mailboxes", hash, "resyncs"); n != 1 {
		t.Fatalf("resyncs = %d", n)
	}
	if n := e.metric("mailboxes", hash, "fullLoads"); n != 2 {
		t.Fatalf("fullLoads = %d", n)
	}
	if n := e.metric("mailboxes", hash, "uidValidityChanges"); n != 0 {
		t.Fatalf("uidValidityChanges = %d", n)
	}
	ex, xp, fl, _ := w.snapshot()
	if len(xp) != 2 || len(ex) == 0 || ex[len(ex)-1] != 9 || !sameStrings(fl[5], []string{`\Flagged`}) {
		t.Fatalf("after the resync the client heard EXISTS %v EXPUNGE %v FLAGS %v", ex, xp, fl)
	}
	compareWithStub(t, e, c, mara, edgestub.Inbox, "long connection after resync")
	fresh := e.login(mara, nil)
	if _, err := fresh.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	compareWithStub(t, e, fresh, mara, edgestub.Inbox, "fresh connection after resync")
}

func TestUIDValidityChangeClosesConnections(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 6, 2)
	e.mailbox(jonas, 3, 0)
	hash := e.mbHash(mara)
	inbox := e.login(mara, nil)
	sent := e.login(mara, nil)
	other := e.login(jonas, nil)
	if _, err := inbox.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := sent.Select("Sent", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	old := e.stub.State(mara, edgestub.Inbox).UIDValidity
	e.stub.Remove(mara, edgestub.Inbox, 1)
	e.stub.Renumber(mara, edgestub.Inbox)

	e.sync()
	select {
	case <-inbox.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection with the renumbered folder stays open")
	}
	// connections on other folders and mailboxes are not touched
	if err := sent.Noop().Wait(); err != nil {
		t.Fatalf("connection on Sent: %v", err)
	}
	if err := other.Noop().Wait(); err != nil {
		t.Fatalf("connection of another mailbox: %v", err)
	}
	if n := e.metric("mailboxes", hash, "uidValidityChanges"); n != 1 {
		t.Fatalf("uidValidityChanges = %d", n)
	}
	if n := e.metric("mirror", "uidValidityChanges"); n != 1 {
		t.Fatalf("mirror.uidValidityChanges = %d", n)
	}
	fresh := e.login(mara, nil)
	sel, err := fresh.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.UIDValidity == old || sel.UIDValidity != e.stub.State(mara, edgestub.Inbox).UIDValidity {
		t.Fatalf("uidValidity after renumbering = %d (old %d)", sel.UIDValidity, old)
	}
	compareWithStub(t, e, fresh, mara, edgestub.Inbox, "after renumbering")
	// raw mails come from the new numbering, not from the cache of the old
	msgs, err := fresh.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil || len(msgs) != 1 || sha(bodySection(msgs[0], 0)) != e.stub.State(mara, edgestub.Inbox).Rows[0].Digest {
		t.Fatalf("uid 1 after renumbering is not the edge's uid 1: %v", err)
	}
}

// ---- sessions ------------------------------------------------------------

func TestRevokeEndsExactlyThatConnection(t *testing.T) {
	e := newEnv(t)
	pw1 := e.mailbox(mara, 4, 0)
	pw2 := e.stub.AddPassword(mara)
	e.mailbox(jonas, 2, 0)

	phone := e.dial(nil)
	if err := phone.Login(mara, pw1).Wait(); err != nil {
		t.Fatal(err)
	}
	laptop := e.dial(nil)
	if err := laptop.Login(mara, pw2).Wait(); err != nil {
		t.Fatal(err)
	}
	other := e.login(jonas, nil)
	full := &imap.FetchItemBodySection{Peek: true}
	for _, c := range []*imapclient.Client{phone, laptop, other} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		// warm mirror and cache
		if _, err := c.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{full}}).Collect(); err != nil {
			t.Fatal(err)
		}
	}

	e.stub.RevokePassword(mara, pw1)
	e.sync()
	select {
	case <-phone.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection with the revoked password stays open")
	}
	// warm cache or not: nothing more for the revoked connection
	if _, err := phone.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{full}}).Collect(); err == nil {
		t.Fatal("the revoked connection still reads mail")
	}
	// the same mailbox with the other password, and the other mailbox, go on
	if _, err := laptop.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{full}}).Collect(); err != nil {
		t.Fatalf("second device of the same mailbox: %v", err)
	}
	if err := other.Noop().Wait(); err != nil {
		t.Fatalf("other mailbox: %v", err)
	}
	if n := e.metric("imap", "sessionsEnded"); n != 1 {
		t.Fatalf("sessionsEnded = %d", n)
	}
	// and the revoked password does not log in again
	again := e.dial(nil)
	wantNo(t, "revoked", again.Login(mara, pw1).Wait(), "AUTHENTICATIONFAILED")
}

// A session the edge rejects on an ordinary call (not the poll) ends the
// connection as well.
func TestRejectedSessionOnACallEndsTheConnection(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 3, 0)
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	e.stub.Disable(mara)
	err := c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close()
	if err == nil {
		t.Fatal("STORE worked on a mailbox that is switched off")
	}
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stays open")
	}
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 2, 0)
	e.mailbox(jonas, 2, 0)

	// the edge lets the session run out before its announced time
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	e.stub.ExpireSessions(mara)
	e.sync()
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("connection with an expired session stays open")
	}

	// the bridge enforces the announced end by itself, without the poll
	e.stub.SessionTTL = 400 * time.Millisecond
	short := e.login(jonas, nil)
	if _, err := short.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-short.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("connection stays open beyond expiresAt")
	}
	if n := e.metric("imap", "sessionsExpired"); n != 1 {
		t.Fatalf("sessionsExpired = %d", n)
	}
	// logging in again works
	e.stub.SessionTTL = time.Hour
	again := e.login(jonas, nil)
	if _, err := again.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
}

// Without a recent confirmation of its own session a connection gets
// nothing, not even what is already in memory.
func TestNoCachedReadsWithoutConfirmedSession(t *testing.T) {
	e := newEnv(t, set("test.fresh_ms", "200"))
	e.mailbox(mara, 3, 0)
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	full := &imap.FetchItemBodySection{Peek: true}
	opts := &imap.FetchOptions{Flags: true, BodySection: []*imap.FetchItemBodySection{full}}
	if _, err := c.Fetch(all(), opts).Collect(); err != nil {
		t.Fatal(err)
	}
	raws := e.stub.Calls("/raw")

	// the edge cannot be asked: no answer from mirror or cache
	e.stub.SetFault("/seq", edgestub.Fault{Status: 500, Body: `{"error":"down"}`})
	time.Sleep(300 * time.Millisecond)
	_, err := c.Fetch(all(), opts).Collect()
	wantNo(t, "fetch without confirmation", err, "UNAVAILABLE")
	_, err = c.Fetch(all(), &imap.FetchOptions{Flags: true}).Collect()
	wantNo(t, "flags without confirmation", err, "UNAVAILABLE")
	_, err = c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	wantNo(t, "search without confirmation", err, "UNAVAILABLE")

	// the edge is back: one question, then the cache answers
	e.stub.ClearFaults()
	if _, err := c.Fetch(all(), opts).Collect(); err != nil {
		t.Fatalf("after the edge is back: %v", err)
	}
	if e.stub.Calls("/raw") != raws {
		t.Fatal("the cache was not used")
	}

	// revoked while everything is warm and nobody polls: the next command
	// asks, gets a no, and the connection ends
	e.stub.RevokePassword(mara, e.pw[mara])
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Fetch(all(), opts).Collect(); err == nil {
		t.Fatal("revoked connection read from the warm cache")
	}
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("revoked connection stays open")
	}
}

// ---- raw mails -----------------------------------------------------------

func TestRawFailuresAreUnavailable(t *testing.T) {
	e := newEnv(t, set("EDGE_LONG_TIMEOUT_MS", "300"))
	e.mailbox(mara, 4, 0)
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	fetch := func(uid uint32) error {
		_, err := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
		return err
	}
	e.stub.SetFault("/raw", edgestub.Fault{Status: 500, Body: `{"error":"boom"}`, Count: 1})
	wantNo(t, "500", fetch(1), "UNAVAILABLE")
	e.stub.SetFault("/raw", edgestub.Fault{Delay: 2 * time.Second, Count: 1})
	start := time.Now()
	wantNo(t, "timeout", fetch(1), "UNAVAILABLE")
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("the fetch hung for %v", d)
	}
	e.stub.SetFault("/raw", edgestub.Fault{Cut: true, Count: 1})
	wantNo(t, "truncated", fetch(1), "UNAVAILABLE")
	// nothing broken was cached, and the connection is fine
	if err := fetch(1); err != nil {
		t.Fatalf("after the faults: %v", err)
	}
	// a mail that is gone at the edge but still in the mirror: no answer
	// for it, the rest is delivered
	e.stub.Remove(mara, edgestub.Inbox, 3)
	msgs, err := c.Fetch(all(), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil || len(msgs) != 3 {
		t.Fatalf("fetch with a vanished mail: %d mails, %v", len(msgs), err)
	}
}

// One connection that panics goes down alone.
func TestPanicEndsOnlyThatConnection(t *testing.T) {
	var arm sync.Mutex
	armed := false
	e2 := newEnvWithHook(t, func() {
		arm.Lock()
		boom := armed
		armed = false
		arm.Unlock()
		if boom {
			panic("test: deliberate panic in FETCH")
		}
	})
	e2.mailbox(mara, 3, 0)
	e2.mailbox(jonas, 3, 0)
	victim := e2.login(mara, nil)
	sibling := e2.login(mara, nil)
	other := e2.login(jonas, nil)
	for _, c := range []*imapclient.Client{victim, sibling, other} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	arm.Lock()
	armed = true
	arm.Unlock()
	if _, err := victim.Fetch(all(), &imap.FetchOptions{Flags: true}).Collect(); err == nil {
		t.Fatal("the panicking FETCH answered")
	}
	select {
	case <-victim.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the panicking connection stays open")
	}
	compareWithStub(t, e2, sibling, mara, edgestub.Inbox, "same mailbox after the panic")
	compareWithStub(t, e2, other, jonas, edgestub.Inbox, "other mailbox after the panic")
	if n := e2.metric("imap", "panics"); n != 1 {
		t.Fatalf("panics = %d", n)
	}
	if n := e2.metric("panics"); n != 1 {
		t.Fatalf("total panics = %d", n)
	}
	// and new connections work
	again := e2.login(mara, nil)
	if _, err := again.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
}
