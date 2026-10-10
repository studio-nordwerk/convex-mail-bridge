package bridge_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

// Version 1.1 of the contract: the mirror hangs on the mailbox identifier
// of /login, /raw carries the uidValidity, /changes carries uidNext.

func readAll(t *testing.T, c *imapclient.Client, folder string) map[uint32]string {
	t.Helper()
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(all(), &imap.FetchOptions{UID: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32]string{}
	for _, m := range msgs {
		out[uint32(m.UID)] = sha(bodySection(m, 0))
	}
	return out
}

func wantFolder(t *testing.T, e *env, who, address, folder string, got map[uint32]string) {
	t.Helper()
	st := e.stub.State(address, folder)
	if len(got) != len(st.Rows) {
		t.Fatalf("%s: %d mails in %s, the edge has %d", who, len(got), folder, len(st.Rows))
	}
	for _, r := range st.Rows {
		if got[r.UID] != r.Digest {
			t.Fatalf("%s: uid %d in %s is not the mail the edge has under that number", who, r.UID, folder)
		}
	}
}

// One address, first one mailbox, then another (the old one is gone, a new
// one was set up under the same address). The new mailbox looks like the
// old in every number the bridge could key on: same address, same folder
// names, same uidValidity, same uids, and its change counter is already
// beyond the old one's, so that no "went backwards" check can save the day.
// Nothing of the old mirror or cache may show.
func TestSameAddressNewMailboxSeesNothingOfTheOld(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 5, 2)
	oldID := e.stub.MailboxID(mara)
	oldHash := e.mbHash(mara)
	oldInbox := e.stub.State(mara, edgestub.Inbox)
	oldSent := e.stub.State(mara, edgestub.Sent)

	old := e.login(mara, nil)
	wantFolder(t, e, "old mailbox", mara, "INBOX", readAll(t, old, "INBOX"))
	wantFolder(t, e, "old mailbox", mara, "Sent", readAll(t, old, "Sent"))

	// the new mailbox under the same address
	e.stub.AddMailbox(mara)
	pw := e.stub.AddPassword(mara)
	e.pw[mara] = pw
	if e.stub.MailboxID(mara) == oldID {
		t.Fatal("fixture: the new mailbox has the old identifier")
	}
	e.stub.SetUIDValidity(mara, edgestub.Inbox, oldInbox.UIDValidity)
	e.stub.SetUIDValidity(mara, edgestub.Sent, oldSent.UIDValidity)
	when := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		raw := edgestub.BuildMail(edgestub.MailSpec{
			From: "new@outside.test", To: []string{mara}, Subject: fmt.Sprintf("New mailbox, mail %d", i+1),
			Text: fmt.Sprintf("This belongs to the new mailbox (%d). Longer than the old mail, so the size differs too.", i+1), Date: when,
		})
		e.stub.Deliver(mara, edgestub.Inbox, raw, when)
	}
	for _, uid := range []uint32{6, 7, 8} { // leaves uids 1..5, like the old one, with a higher change counter
		e.stub.Remove(mara, edgestub.Inbox, uid)
	}
	for i := 0; i < 2; i++ {
		raw := edgestub.BuildMail(edgestub.MailSpec{From: mara, To: []string{"x@outside.test"}, Subject: "New sent", Text: "new", Date: when})
		e.stub.Deliver(mara, edgestub.Sent, raw, when)
	}
	e.stub.SetFlags(mara, edgestub.Sent, 1, []string{`\Flagged`}, nil)
	e.stub.SetFlags(mara, edgestub.Sent, 2, []string{`\Flagged`}, nil)
	e.stub.SetFlags(mara, edgestub.Sent, 1, nil, []string{`\Flagged`})
	if n := e.stub.State(mara, edgestub.Inbox); n.UIDValidity != oldInbox.UIDValidity || len(n.Rows) != 5 || n.HighestModseq <= oldInbox.HighestModseq {
		t.Fatalf("fixture: new INBOX %+v does not shadow the old one", n)
	}

	raws := e.stub.Calls("/raw")
	fresh := e.login(mara, nil)
	wantFolder(t, e, "new mailbox", mara, "INBOX", readAll(t, fresh, "INBOX"))
	compareWithStub(t, e, fresh, mara, edgestub.Inbox, "new mailbox INBOX")
	wantFolder(t, e, "new mailbox", mara, "Sent", readAll(t, fresh, "Sent"))
	compareWithStub(t, e, fresh, mara, edgestub.Sent, "new mailbox Sent")
	if got := e.stub.Calls("/raw") - raws; got != 7 {
		t.Fatalf("%d raw mails were fetched for the new mailbox, want 7 (none from the old cache)", got)
	}
	newHash := e.mbHash(mara)
	if newHash == oldHash {
		t.Fatal("both mailboxes are counted under one hash")
	}
	if n := e.metric("mailboxes", newHash, "fullLoads"); n != 2 {
		t.Fatalf("the new mailbox was read in full %d times, want 2", n)
	}
	if n := e.metric("mailboxes", oldHash, "fullLoads"); n != 2 {
		t.Fatalf("the old mailbox has %d full loads", n)
	}
	// the connection of the old mailbox ends with its session; it never
	// sees the new mail
	e.sync()
	select {
	case <-old.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection of the replaced mailbox stays open")
	}
	if err := fresh.Noop().Wait(); err != nil {
		t.Fatalf("the connection of the new mailbox: %v", err)
	}
}

// Two addresses of one mailbox share its mirror.
func TestTwoAddressesOfOneMailboxShareTheMirror(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 6, 1)
	alias := "office@mailbox-one.test"
	e.stub.AddAlias(alias, mara)
	e.pw[alias] = e.pw[mara]

	a := e.login(mara, nil)
	wantFolder(t, e, "address", mara, "INBOX", readAll(t, a, "INBOX"))
	entries, raws := e.stub.Calls("/entries"), e.stub.Calls("/raw")
	b := e.login(alias, nil)
	wantFolder(t, e, "alias", mara, "INBOX", readAll(t, b, "INBOX"))
	if e.stub.Calls("/entries") != entries || e.stub.Calls("/raw") != raws {
		t.Fatal("the second address of the same mailbox read everything again")
	}
	// and they hear of each other's changes
	if err := a.Store(imap.UIDSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	compareWithStub(t, e, b, mara, edgestub.Inbox, "alias after a change through the address")
	if n := len(e.metrics()["mailboxes"].(map[string]any)); n != 1 {
		t.Fatalf("%d mailboxes are counted, want 1", n)
	}
}

// The folder is renumbered and the bridge does not know yet. The next raw
// fetch carries the old uidValidity; the edge refuses with 409 instead of
// handing out whatever mail has that number now. The mirror of the folder
// goes, the connections that have it open are closed, nothing is cached.
func TestRawWithOutdatedUIDValidity(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 6, 2)
	hash := e.mbHash(mara)
	full := &imap.FetchItemBodySection{Peek: true}
	fetch := func(c *imapclient.Client, uid uint32) error {
		_, err := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{full}}).Collect()
		return err
	}
	a := e.login(mara, nil)
	b := e.login(mara, nil)
	onSent := e.login(mara, nil)
	for _, c := range []*imapclient.Client{a, b} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := onSent.Select("Sent", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{1, 2} {
		if err := fetch(a, uid); err != nil { // these are in the cache now
			t.Fatal(err)
		}
	}
	if err := fetch(onSent, 1); err != nil {
		t.Fatal(err)
	}
	if n := e.metric("rawCache", "entries"); n != 3 {
		t.Fatalf("rawCache.entries = %d", n)
	}
	oldUIDV := e.stub.State(mara, edgestub.Inbox).UIDValidity

	// renumbered at the edge: the old uid 4 is uid 3 now
	e.stub.Remove(mara, edgestub.Inbox, 1)
	e.stub.Renumber(mara, edgestub.Inbox)

	err := fetch(a, 4)
	wantNo(t, "raw fetch with the old numbering", err, "UNAVAILABLE")
	for name, c := range map[string]*imapclient.Client{"the fetching connection": a, "the other connection on the folder": b} {
		select {
		case <-c.Closed():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s stays open after the folder was renumbered", name)
		}
	}
	if err := fetch(onSent, 2); err != nil {
		t.Fatalf("connection on another folder: %v", err)
	}
	if n := e.metric("mailboxes", hash, "uidValidityChanges"); n != 1 {
		t.Fatalf("uidValidityChanges = %d", n)
	}
	// only the two mails of Sent are left in the cache: nothing of the old
	// numbering, and nothing from the refused fetch
	if n := e.metric("rawCache", "entries"); n != 2 {
		t.Fatalf("rawCache.entries = %d after the renumbering", n)
	}

	fresh := e.login(mara, nil)
	sel, err := fresh.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sel.UIDValidity == oldUIDV {
		t.Fatal("the fresh connection still sees the old uidValidity")
	}
	wantFolder(t, e, "after renumbering", mara, "INBOX", readAll(t, fresh, "INBOX"))
	if n := e.metric("mailboxes", hash, "uidValidityChanges"); n != 1 {
		t.Fatalf("the change was counted twice (%d)", n)
	}
}

// UIDNEXT comes from the edge, also on the incremental path. Here numbers
// were given out that the bridge never saw a row for.
func TestUIDNextComesFromTheEdge(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 4, 0)
	c := e.login(mara, nil)
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil || sel.UIDNext != 5 {
		t.Fatalf("UIDNEXT = %d %v", sel.UIDNext, err)
	}
	_ = c.Logout().Wait()

	e.stub.SkipUIDs(mara, edgestub.Inbox, 7) // 5..11 are used up, without rows
	e.stub.SetFlags(mara, edgestub.Inbox, 2, []string{`\Seen`}, nil)
	entries := e.stub.Calls("/entries")
	c2 := e.login(mara, nil)
	sel, err = c2.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if e.stub.Calls("/entries") != entries {
		t.Fatal("the folder was read in full; the test is about the incremental path")
	}
	if want := e.stub.State(mara, edgestub.Inbox).UIDNext; uint32(sel.UIDNext) != want || want != 12 {
		t.Fatalf("UIDNEXT after SELECT = %d, the edge says %d", sel.UIDNext, want)
	}
	st, err := c2.Status("INBOX", &imap.StatusOptions{UIDNext: true}).Wait()
	if err != nil || st.UIDNext != sel.UIDNext {
		t.Fatalf("STATUS says UIDNEXT %d, SELECT said %d", st.UIDNext, sel.UIDNext)
	}
	// a new mail gets the number that was announced
	uid := e.stub.Deliver(mara, edgestub.Inbox, edgestub.BuildMail(edgestub.MailSpec{From: "a@outside.test", To: []string{mara}, Subject: "after the gap", Text: "x"}), time.Now())
	if uid != uint32(sel.UIDNext) {
		t.Fatalf("new mail has uid %d, announced was %d", uid, sel.UIDNext)
	}
	e.sync()
	if err := c2.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	compareWithStub(t, e, c2, mara, edgestub.Inbox, "after the gap")
}

// A login answer without a mailbox identifier is no login.
func TestLoginWithoutMailboxIdentifierIsRefused(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)
	e.stub.SetFault("/login", edgestub.Fault{Status: 200, Body: `{"session":"nws_without_mailbox","expiresAt":32503680000000}`, Count: 1})
	c := e.dial(nil)
	wantNo(t, "login without mailbox", c.Login(mara, pw).Wait(), "UNAVAILABLE")
	if n := e.metric("imap", "loginsOk"); n != 0 {
		t.Fatalf("loginsOk = %d", n)
	}
	if err := c.Login(mara, pw).Wait(); err != nil {
		t.Fatalf("proper login afterwards: %v", err)
	}
}
