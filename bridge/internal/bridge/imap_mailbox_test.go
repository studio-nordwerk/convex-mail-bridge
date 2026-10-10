package bridge_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

func all() imap.SeqSet {
	var s imap.SeqSet
	s.AddRange(1, 0)
	return s
}

func flagsOf(fs []imap.Flag) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compareWithStub checks that FETCH 1:* (UID FLAGS RFC822.SIZE
// INTERNALDATE) on c shows exactly the living rows of the stub, in order.
func compareWithStub(t *testing.T, e *env, c *imapclient.Client, address, folder, who string) {
	t.Helper()
	st := e.stub.State(address, folder)
	var msgs []*imapclient.FetchMessageBuffer
	if len(st.Rows) > 0 || c.Mailbox() != nil && c.Mailbox().NumMessages > 0 {
		var err error
		msgs, err = c.Fetch(all(), &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}).Collect()
		if err != nil {
			t.Fatalf("%s: fetch: %v", who, err)
		}
	}
	if len(msgs) != len(st.Rows) {
		t.Fatalf("%s: %d mails, the edge has %d", who, len(msgs), len(st.Rows))
	}
	for i, m := range msgs {
		r := st.Rows[i]
		if m.SeqNum != uint32(i+1) {
			t.Fatalf("%s: position %d has sequence number %d", who, i+1, m.SeqNum)
		}
		if uint32(m.UID) != r.UID {
			t.Fatalf("%s: seq %d: uid %d, the edge has %d", who, i+1, m.UID, r.UID)
		}
		want := append([]string(nil), r.Flags...)
		sort.Strings(want)
		if got := flagsOf(m.Flags); !sameStrings(got, want) {
			t.Fatalf("%s: uid %d: flags %v, the edge has %v", who, r.UID, got, want)
		}
		if m.RFC822Size != r.Size {
			t.Fatalf("%s: uid %d: size %d, the edge has %d", who, r.UID, m.RFC822Size, r.Size)
		}
		if m.InternalDate.UnixMilli()/1000 != r.InternalDate/1000 {
			t.Fatalf("%s: uid %d: date %v, the edge has %v", who, r.UID, m.InternalDate, time.UnixMilli(r.InternalDate))
		}
	}
}

func TestListStatusSelect(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 7, 3)
	c := e.login(mara, nil)

	// plain LIST, no RETURN (SPECIAL-USE): the attribute is there anyway
	boxes, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 2 || boxes[0].Mailbox != "INBOX" || boxes[1].Mailbox != "Sent" {
		t.Fatalf("LIST = %+v", boxes)
	}
	hasSent := false
	for _, a := range boxes[1].Attrs {
		if a == imap.MailboxAttrSent {
			hasSent = true
		}
	}
	if !hasSent {
		t.Fatalf("Sent has no \\Sent: %v", boxes[1].Attrs)
	}
	for _, a := range boxes[0].Attrs {
		if a == imap.MailboxAttrSent {
			t.Fatal("INBOX has \\Sent")
		}
	}
	if l, _ := c.List("", "%", &imap.ListOptions{SelectSpecialUse: true}).Collect(); len(l) != 1 || l[0].Mailbox != "Sent" {
		t.Fatalf("LIST (SPECIAL-USE) = %+v", l)
	}
	// absurd patterns do not make the server work
	if l, err := c.List("", strings.Repeat("*a", 2000), nil).Collect(); err != nil || len(l) != 0 {
		t.Fatalf("long pattern: %v %v", l, err)
	}

	for _, name := range []string{"INBOX", "Sent"} {
		st := e.stub.State(mara, name)
		unseen := uint32(0)
		for _, r := range st.Rows {
			seen := false
			for _, f := range r.Flags {
				if f == `\Seen` {
					seen = true
				}
			}
			if !seen {
				unseen++
			}
		}
		data, err := c.Status(name, &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if *data.NumMessages != uint32(len(st.Rows)) || *data.NumUnseen != unseen || uint32(data.UIDNext) != st.UIDNext || data.UIDValidity != st.UIDValidity {
			t.Fatalf("STATUS %s = %d/%d/%d/%d, edge %d/%d/%d/%d", name, *data.NumMessages, *data.NumUnseen, data.UIDNext, data.UIDValidity,
				len(st.Rows), unseen, st.UIDNext, st.UIDValidity)
		}
		sel, err := c.Select(name, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if sel.NumMessages != uint32(len(st.Rows)) || uint32(sel.UIDNext) != st.UIDNext || sel.UIDValidity != st.UIDValidity {
			t.Fatalf("SELECT %s = %+v, edge %+v", name, sel, st)
		}
		compareWithStub(t, e, c, mara, name, "select "+name)
	}
	if _, err := c.Select("Archive", nil).Wait(); !isNo(err) {
		t.Fatalf("SELECT Archive: %v", err)
	}
	if _, err := c.Status("Trash", &imap.StatusOptions{NumMessages: true}).Wait(); !isNo(err) {
		t.Fatalf("STATUS Trash: %v", err)
	}
	ns, err := c.Namespace().Wait()
	if err != nil || len(ns.Personal) != 1 || ns.Personal[0].Delim != '/' {
		t.Fatalf("NAMESPACE = %+v %v", ns, err)
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func bodySection(m *imapclient.FetchMessageBuffer, i int) []byte {
	if i >= len(m.BodySection) {
		return nil
	}
	return m.BodySection[i].Bytes
}

func TestFetchBytesEqualRaw(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 4, 1)
	// a mail with HTML and an attachment, one with bare LF line ends, and
	// one whose header is broken: all three must come back byte for byte
	attach := bytes.Repeat([]byte("Attached contents. "), 9000) // about 170 KB
	multi := edgestub.BuildMail(edgestub.MailSpec{
		From: "Supplier <stock@outside.test>", To: []string{mara}, Subject: "Quote with attachment – café size",
		Text: "Attached.", HTML: "<p>See <b>attached</b>.</p>", AttachName: "quote.bin", AttachData: attach,
	})
	bareLF := []byte("From: a@outside.test\nTo: " + mara + "\nSubject: LF only\n\nLine one\nLine two\n")
	broken := []byte("From: b@outside.test\r\nthis is not a header line\r\nSubject: broken\r\n\r\nText despite the broken header line.\r\n")
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	uids := []uint32{
		e.stub.Deliver(mara, edgestub.Inbox, multi, now),
		e.stub.Deliver(mara, edgestub.Inbox, bareLF, now),
		e.stub.Deliver(mara, edgestub.Inbox, broken, now),
	}
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	st := e.stub.State(mara, edgestub.Inbox)

	full := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.Fetch(all(), &imap.FetchOptions{UID: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{full}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(st.Rows) {
		t.Fatalf("%d of %d", len(msgs), len(st.Rows))
	}
	for i, m := range msgs {
		got := bodySection(m, 0)
		if sha(got) != st.Rows[i].Digest {
			t.Fatalf("uid %d: BODY[] differs from the raw mail (%d vs %d bytes)", m.UID, len(got), len(st.Rows[i].Raw))
		}
		if int64(len(got)) != m.RFC822Size {
			t.Fatalf("uid %d: RFC822.SIZE %d, body %d", m.UID, m.RFC822Size, len(got))
		}
	}
	if n := e.metric("imap", "rawSizeMismatch"); n != 0 {
		t.Fatalf("rawSizeMismatch = %d", n)
	}

	// structure, envelope and parts of the multipart mail
	one := imap.UIDSetNum(imap.UID(uids[0]))
	hdr := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	text := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true}
	subj := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject", "From"}, Peek: true}
	part2 := &imap.FetchItemBodySection{Part: []int{2}, Peek: true}
	chunk := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 100, Size: 50}}
	got, err := c.Fetch(one, &imap.FetchOptions{
		Envelope: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{hdr, text, subj, part2, chunk},
	}).Collect()
	if err != nil || len(got) != 1 {
		t.Fatalf("fetch parts: %v (%d)", err, len(got))
	}
	m := got[0]
	if m.Envelope == nil || m.Envelope.Subject != "Quote with attachment – café size" || len(m.Envelope.From) != 1 || m.Envelope.From[0].Host != "outside.test" {
		t.Fatalf("envelope = %+v", m.Envelope)
	}
	mp, ok := m.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok || len(mp.Children) != 2 || mp.Subtype != "mixed" {
		t.Fatalf("structure = %#v", m.BodyStructure)
	}
	if inner, ok := mp.Children[0].(*imap.BodyStructureMultiPart); !ok || inner.Subtype != "alternative" || len(inner.Children) != 2 {
		t.Fatalf("first part = %#v", mp.Children[0])
	}
	att, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || att.Type != "application" || att.Disposition() == nil || att.Disposition().Params["filename"] != "quote.bin" {
		t.Fatalf("attachment = %#v", mp.Children[1])
	}
	h := m.FindBodySection(hdr)
	b := m.FindBodySection(text)
	if !bytes.Equal(append(append([]byte{}, h...), b...), multi) {
		t.Fatal("BODY[HEADER] + BODY[TEXT] is not the raw mail")
	}
	if s := string(m.FindBodySection(subj)); !strings.Contains(s, "Subject:") || !strings.Contains(s, "From:") || strings.Contains(s, "Message-ID") || !strings.HasSuffix(s, "\r\n\r\n") {
		t.Fatalf("HEADER.FIELDS = %q", s)
	}
	if p := m.FindBodySection(part2); uint32(len(p)) != att.Size || len(p) < len(attach) {
		t.Fatalf("BODY[2]: %d bytes, structure says %d", len(p), att.Size)
	}
	if p := m.FindBodySection(chunk); !bytes.Equal(p, multi[100:150]) {
		t.Fatalf("BODY[]<100.50> = %q", p)
	}

	// the broken header: still readable, as one text part
	got, err = c.Fetch(imap.UIDSetNum(imap.UID(uids[2])), &imap.FetchOptions{
		Envelope: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{{Part: []int{1}, Peek: true}},
	}).Collect()
	if err != nil || len(got) != 1 {
		t.Fatalf("broken header: %v", err)
	}
	sp, ok := got[0].BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok || sp.Type != "text" {
		t.Fatalf("broken header structure = %#v", got[0].BodyStructure)
	}
	if p := bodySection(got[0], 0); string(p) != "Text despite the broken header line.\r\n" {
		t.Fatalf("broken header BODY[1] = %q", p)
	}

	// second pass: everything comes from the cache
	before := e.stub.Calls("/raw")
	if _, err := c.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{full}}).Collect(); err != nil {
		t.Fatal(err)
	}
	if after := e.stub.Calls("/raw"); after != before {
		t.Fatalf("second pass fetched %d raw mails", after-before)
	}
}

// Two mailboxes whose folders look the same in every number (folder name,
// uidValidity, uids). B reads first and leaves mirror and cache warm; A
// must still only ever see its own mail.
func TestMailboxesAreSeparate(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 5, 2)
	e.mailbox(jonas, 5, 2)
	for _, f := range []string{edgestub.Inbox, edgestub.Sent} {
		e.stub.SetUIDValidity(mara, f, 424242)
		e.stub.SetUIDValidity(jonas, f, 424242)
	}
	full := &imap.FetchItemBodySection{Peek: true}
	read := func(c *imapclient.Client, folder string) map[uint32]string {
		t.Helper()
		if _, err := c.Select(folder, nil).Wait(); err != nil {
			t.Fatal(err)
		}
		msgs, err := c.Fetch(all(), &imap.FetchOptions{UID: true, Envelope: true, BodySection: []*imap.FetchItemBodySection{full}}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		out := map[uint32]string{}
		for _, m := range msgs {
			out[uint32(m.UID)] = sha(bodySection(m, 0))
		}
		return out
	}
	check := func(who, address string, got map[uint32]string, folder string) {
		t.Helper()
		st := e.stub.State(address, folder)
		if len(got) != len(st.Rows) {
			t.Fatalf("%s: %d mails, want %d", who, len(got), len(st.Rows))
		}
		for _, r := range st.Rows {
			if got[r.UID] != r.Digest {
				t.Fatalf("%s: uid %d in %s is not %s's mail", who, r.UID, folder, address)
			}
		}
	}

	cb := e.login(jonas, nil) // B first: warm
	for _, f := range []string{"INBOX", "Sent"} {
		check("jonas", jonas, read(cb, f), f)
	}
	ca := e.login(mara, nil) // A while B is still connected
	for _, f := range []string{"INBOX", "Sent"} {
		check("mara", mara, read(ca, f), f)
	}
	// A again from its own warm cache, B again, B after reconnecting
	for _, f := range []string{"INBOX", "Sent"} {
		check("mara again", mara, read(ca, f), f)
		check("jonas again", jonas, read(cb, f), f)
	}
	_ = cb.Logout().Wait()
	ca2 := e.login(mara, nil)
	check("mara second connection", mara, read(ca2, "INBOX"), "INBOX")
	cb2 := e.login(jonas, nil)
	check("jonas after reconnect", jonas, read(cb2, "INBOX"), "INBOX")

	// flags stay apart as well
	if err := ca.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{"$NurMara"}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	e.sync()
	for _, r := range e.stub.State(jonas, edgestub.Sent).Rows {
		for _, f := range r.Flags {
			if f == "$NurMara" {
				t.Fatal("flag landed in the other mailbox")
			}
		}
	}
	compareWithStub(t, e, cb2, jonas, "INBOX", "jonas after mara's store")
}

func TestStoreAndSeenOnBodyFetch(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 6, 0)
	c := e.login(mara, nil)
	other := e.login(mara, nil)
	for _, cl := range []*imapclient.Client{c, other} {
		if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	// uid 2 is unread in the fixture
	two := imap.UIDSetNum(2)
	res, err := c.Store(two, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged, "$Forwarded"}}, nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !sameStrings(flagsOf(res[0].Flags), []string{"$Forwarded", `\Flagged`}) {
		t.Fatalf("STORE answer = %+v", res)
	}
	row := func(uid uint32) edgestub.Row {
		for _, r := range e.stub.State(mara, edgestub.Inbox).Rows {
			if r.UID == uid {
				return r
			}
		}
		t.Fatalf("uid %d not in the edge", uid)
		return edgestub.Row{}
	}
	if got := row(2).Flags; !sameStrings(got, []string{"$Forwarded", `\Flagged`}) {
		t.Fatalf("edge flags = %v", got)
	}

	// BODY[] without PEEK: the mail is read afterwards, here and at the edge
	full := &imap.FetchItemBodySection{}
	msgs, err := c.Fetch(two, &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{full}}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetch: %v", err)
	}
	if !sameStrings(flagsOf(msgs[0].Flags), []string{"$Forwarded", `\Flagged`, `\Seen`}) {
		t.Fatalf("flags in the FETCH answer = %v", msgs[0].Flags)
	}
	if got := row(2).Flags; !sameStrings(got, []string{"$Forwarded", `\Flagged`, `\Seen`}) {
		t.Fatalf("edge flags after BODY[] = %v", got)
	}
	// with PEEK nothing changes
	stores := e.stub.Calls("/store")
	if _, err := c.Fetch(imap.UIDSetNum(3), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect(); err != nil {
		t.Fatal(err)
	}
	if e.stub.Calls("/store") != stores {
		t.Fatal("BODY.PEEK[] wrote to the edge")
	}

	// FLAGS (...) replaces, -FLAGS removes, .SILENT answers nothing
	if _, err := c.Store(two, &imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: []imap.Flag{imap.FlagAnswered}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := row(2).Flags; !sameStrings(got, []string{`\Answered`}) {
		t.Fatalf("edge flags after FLAGS (\\Answered) = %v", got)
	}
	// .SILENT: no answer is owed. The bridge still tells every connection
	// the new flags as an ordinary update (see applyStore), so if something
	// arrives it has to be the new state.
	silent, err := c.Store(two, &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagAnswered}}, nil).Collect()
	if err != nil || len(silent) > 1 || (len(silent) == 1 && len(silent[0].Flags) != 0) {
		t.Fatalf("silent store: %v %v", silent, err)
	}
	if got := row(2).Flags; len(got) != 0 {
		t.Fatalf("edge flags after -FLAGS = %v", got)
	}
	// flags that cannot be stored
	if err := c.Store(two, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{`\Invented`}}, nil).Close(); !isNo(err) {
		t.Fatalf("unknown system flag: %v", err)
	}

	// the second connection learns all of it with NOOP, and both match
	if err := other.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	compareWithStub(t, e, other, mara, edgestub.Inbox, "second connection")
	compareWithStub(t, e, c, mara, edgestub.Inbox, "first connection")
}

func TestExamineWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 4, 0)
	e.stub.SetFlags(mara, edgestub.Inbox, 1, []string{`\Deleted`}, nil)
	c := e.login(mara, nil)
	sel, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.PermanentFlags) != 0 {
		t.Fatalf("PERMANENTFLAGS on EXAMINE = %v", sel.PermanentFlags)
	}
	before := e.stub.State(mara, edgestub.Inbox)
	e.stub.ResetCalls()

	// BODY[] without PEEK: answered, but nothing is marked
	msgs, err := c.Fetch(all(), &imap.FetchOptions{Flags: true, BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	if err != nil || len(msgs) != 4 {
		t.Fatalf("fetch: %v (%d)", err, len(msgs))
	}
	if err := c.Store(all(), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); !isNo(err) {
		t.Fatalf("STORE on EXAMINE: %v", err)
	}
	if _, err := c.Expunge().Collect(); err != nil {
		t.Fatalf("EXPUNGE on EXAMINE: %v", err)
	}
	if err := c.UnselectAndExpunge().Wait(); err != nil { // CLOSE
		t.Fatalf("CLOSE on EXAMINE: %v", err)
	}
	if n := e.stub.Calls("/store") + e.stub.Calls("/expunge"); n != 0 {
		t.Fatalf("EXAMINE made %d writing calls", n)
	}
	after := e.stub.State(mara, edgestub.Inbox)
	if after.HighestModseq != before.HighestModseq || len(after.Rows) != len(before.Rows) {
		t.Fatalf("the folder changed: %d -> %d", before.HighestModseq, after.HighestModseq)
	}
}

func TestDeleteAndExpunge(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 8, 0)
	var expunged []uint32
	watcherOpts := &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Expunge: func(seq uint32) { expunged = append(expunged, seq) },
	}}
	c := e.login(mara, nil)
	watcher := e.login(mara, watcherOpts)
	for _, cl := range []*imapclient.Client{c, watcher} {
		if _, err := cl.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	del := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	var set imap.UIDSet
	set.AddNum(2, 3, 6)
	if err := c.Store(set, del, nil).Close(); err != nil {
		t.Fatal(err)
	}
	// \Deleted is an ordinary flag at the edge until EXPUNGE
	for _, r := range e.stub.State(mara, edgestub.Inbox).Rows {
		has := false
		for _, f := range r.Flags {
			has = has || f == `\Deleted`
		}
		if has != (r.UID == 2 || r.UID == 3 || r.UID == 6) {
			t.Fatalf("uid %d: \\Deleted = %v", r.UID, has)
		}
	}
	// UID EXPUNGE takes only the named one
	seqs, err := c.UIDExpunge(imap.UIDSetNum(3)).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 1 || seqs[0] != 3 {
		t.Fatalf("UID EXPUNGE 3 answered %v", seqs)
	}
	// EXPUNGE takes the rest: uid 2 (seq 2) and uid 6 (now seq 5)
	seqs, err = c.Expunge().Collect()
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if len(seqs) != 2 {
		t.Fatalf("EXPUNGE answered %v", seqs)
	}
	var left []uint32
	for _, r := range e.stub.State(mara, edgestub.Inbox).Rows {
		left = append(left, r.UID)
	}
	if fmt.Sprint(left) != "[1 4 5 7 8]" {
		t.Fatalf("edge has %v", left)
	}
	compareWithStub(t, e, c, mara, edgestub.Inbox, "after expunge")

	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if len(expunged) != 3 {
		t.Fatalf("second connection heard EXPUNGE %v", expunged)
	}
	compareWithStub(t, e, watcher, mara, edgestub.Inbox, "second connection after expunge")

	// CLOSE expunges what is flagged
	if err := c.Store(imap.UIDSetNum(8), del, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.UnselectAndExpunge().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.stub.State(mara, edgestub.Inbox).Rows); n != 4 {
		t.Fatalf("after CLOSE the edge has %d", n)
	}
}

func appendMail(c *imapclient.Client, folder string, raw []byte) (*imap.AppendData, error) {
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}})
	if _, err := cmd.Write(raw); err != nil {
		return nil, err
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	return cmd.Wait()
}

func TestAppend(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 2, 3)
	c := e.login(mara, nil)
	sent := e.stub.State(mara, edgestub.Sent)

	// a mail that is already in Sent: the existing uid, no second copy
	known := sent.Rows[1]
	data, err := appendMail(c, "Sent", known.Raw)
	if err != nil {
		t.Fatalf("append known: %v", err)
	}
	if uint32(data.UID) != known.UID || data.UIDValidity != sent.UIDValidity {
		t.Fatalf("APPENDUID %d %d, want %d %d", data.UIDValidity, data.UID, sent.UIDValidity, known.UID)
	}
	last := e.stub.Last()
	if last.AppendDigest != known.Digest {
		t.Fatalf("digest sent to the edge = %s, want %s", last.AppendDigest, known.Digest)
	}
	if last.AppendMessageID != "<"+known.MessageID+">" {
		t.Fatalf("messageId sent to the edge = %q", last.AppendMessageID)
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 3 {
		t.Fatalf("Sent has %d mails after APPEND", n)
	}

	// unknown mail, wrong folder, no Message-ID: NO [CANNOT], counted
	unknown := edgestub.BuildMail(edgestub.MailSpec{From: mara, To: []string{"x@outside.test"}, Subject: "never sent", Text: "x"})
	_, err = appendMail(c, "Sent", unknown)
	wantNo(t, "unknown", err, "CANNOT")
	_, err = appendMail(c, "INBOX", known.Raw)
	wantNo(t, "INBOX", err, "CANNOT")
	_, err = appendMail(c, "Elsewhere", known.Raw)
	wantNo(t, "other folder", err, "CANNOT")
	calls := e.stub.Calls("/append")
	_, err = appendMail(c, "Sent", []byte("Subject: without ID\r\n\r\nx\r\n"))
	wantNo(t, "no message id", err, "CANNOT")
	if e.stub.Calls("/append") != calls {
		t.Fatal("the edge was asked about a mail without Message-ID")
	}
	if n := e.metric("imap", "appendsRejected"); n != 4 {
		t.Fatalf("appendsRejected = %d", n)
	}
	if n := e.metric("imap", "appendsKnown"); n != 1 {
		t.Fatalf("appendsKnown = %d", n)
	}
	// the connection is still in step after the refused literals
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows) + len(e.stub.State(mara, edgestub.Inbox).Rows); n != 5 {
		t.Fatalf("the edge has %d mails after refused APPENDs", n)
	}
}

func TestCannotCommands(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 3, 1)
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	wantNo(t, "CREATE", c.Create("New", nil).Wait(), "CANNOT")
	wantNo(t, "DELETE", c.Delete("Sent").Wait(), "CANNOT")
	wantNo(t, "RENAME", c.Rename("Sent", "Renamed", nil).Wait(), "CANNOT")
	_, err := c.Copy(imap.SeqSetNum(1), "Sent").Wait()
	wantNo(t, "COPY", err, "CANNOT")
	_, err = c.Copy(imap.UIDSetNum(1), "Sent").Wait()
	wantNo(t, "UID COPY", err, "CANNOT")
	wantNo(t, "UNSUBSCRIBE", c.Unsubscribe("INBOX").Wait(), "CANNOT")
	wantNo(t, "SUBSCRIBE other", c.Subscribe("Neu").Wait(), "CANNOT")
	if err := c.Subscribe("Sent").Wait(); err != nil {
		t.Fatalf("SUBSCRIBE Sent (no change): %v", err)
	}
	// MOVE is not announced; a client that tries anyway gets the same NO.
	r := dialRaw(t, e.b.IMAPAddr())
	r.until("* OK")
	r.send("a LOGIN " + mara + " " + e.pw[mara] + "\r\n")
	r.until("a OK")
	r.send("b SELECT INBOX\r\n")
	r.until("b OK")
	for _, cmd := range []string{"MOVE 1 Sent", "UID MOVE 1 Sent"} {
		r.send("c " + cmd + "\r\n")
		lines := r.until("c ")
		if !strings.HasPrefix(lines[len(lines)-1], "c NO [CANNOT]") {
			t.Fatalf("%s: %q", cmd, lines)
		}
	}
	// ID is unknown to the library: BAD, the connection stays.
	r.send("d ID NIL\r\n")
	if lines := r.until("d "); !strings.HasPrefix(lines[len(lines)-1], "d BAD") {
		t.Fatalf("ID: %q", lines)
	}
	r.send("e NOOP\r\n")
	r.until("e OK")
	// nothing changed anywhere
	if n := len(e.stub.State(mara, edgestub.Inbox).Rows) + len(e.stub.State(mara, edgestub.Sent).Rows); n != 4 {
		t.Fatalf("the edge has %d mails", n)
	}
}
