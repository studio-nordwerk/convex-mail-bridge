package bridge_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

func fill(e *env, address string, n int) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		raw := []byte(fmt.Sprintf("From: a@outside.test\r\nSubject: No. %d\r\nMessage-ID: <fill-%s-%d@outside.test>\r\n\r\nx\r\n", i, address, i))
		e.stub.Deliver(address, edgestub.Inbox, raw, now.Add(time.Duration(i)*time.Minute))
	}
}

// A folder larger than one page of the edge, with changes arriving while
// it is being read.
func TestLargeFolderLoadsInPagesAndCatchesUp(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 0, 0)
	fill(e, mara, 4500)
	e.stub.SetFault("/entries", edgestub.Fault{Delay: 120 * time.Millisecond})
	c := e.login(mara, nil)
	done := make(chan error, 1)
	go func() {
		_, err := c.Select("INBOX", nil).Wait()
		done <- err
	}()
	// while the pages are being read
	time.Sleep(150 * time.Millisecond)
	e.stub.Remove(mara, edgestub.Inbox, 10)   // on a page already read
	e.stub.Remove(mara, edgestub.Inbox, 4400) // on a page not read yet
	e.stub.SetFlags(mara, edgestub.Inbox, 20, []string{`\Flagged`}, nil)
	e.stub.SetFlags(mara, edgestub.Inbox, 4300, []string{`\Seen`}, nil)
	fill(e, mara, 3)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.stub.ClearFaults()
	if n := e.stub.Calls("/entries"); n != 3 {
		t.Fatalf("/entries calls = %d, want 3 pages", n)
	}
	compareWithStub(t, e, c, mara, edgestub.Inbox, "after a load with changes in between")
	// uid search over everything stays cheap and exact
	data, err := c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, nil).Wait()
	if err != nil || len(data.AllUIDs()) != 1 || data.AllUIDs()[0] != 20 {
		t.Fatalf("search = %v %v", data.AllUIDs(), err)
	}
}

// The mirror has a fixed row budget. Mirrors of mailboxes without a
// connection make room; mirrors in use are never pushed out, a load that
// does not fit is refused.
func TestMirrorBudget(t *testing.T) {
	e := newEnv(t, set("MIRROR_MAX_ENTRIES", "1000"))
	e.mailbox(mara, 0, 0)
	e.mailbox(jonas, 0, 0)
	fill(e, mara, 600)
	fill(e, jonas, 600)

	a := e.login(mara, nil)
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	_ = a.Logout().Wait()
	eventually(t, 2*time.Second, "mara's connection is gone", func() bool { return e.metric("mirror", "members") == 0 })

	// jonas needs the room: mara's idle mirror goes
	b := e.login(jonas, nil)
	if _, err := b.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select with eviction: %v", err)
	}
	if n := e.metric("mirror", "evictions"); n != 1 {
		t.Fatalf("evictions = %d", n)
	}
	if rows := e.metric("mirror", "rows"); rows > 1000 {
		t.Fatalf("mirror holds %d rows, budget 1000", rows)
	}
	// mara again while jonas is connected: does not fit, refused, counted
	a2 := e.login(mara, nil)
	_, err := a2.Select("INBOX", nil).Wait()
	wantNo(t, "select beyond the budget", err, "UNAVAILABLE")
	if n := e.metric("mirror", "rejected"); n < 1 {
		t.Fatalf("rejected = %d", n)
	}
	if rows := e.metric("mirror", "rows"); rows > 1000 {
		t.Fatalf("mirror holds %d rows, budget 1000", rows)
	}
	// jonas is untouched by all this
	compareWithStub(t, e, b, jonas, edgestub.Inbox, "the connected mailbox")
	_ = b.Logout().Wait()
	eventually(t, 2*time.Second, "only mara is left", func() bool { return e.metric("mirror", "members") == 1 })
	if _, err := a2.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select after room was made: %v", err)
	}
	if n := e.metric("mailboxes", e.mbHash(mara), "fullLoads"); n != 2 {
		t.Fatalf("mara fullLoads = %d", n)
	}
}

func TestRawCacheIsBounded(t *testing.T) {
	e := newEnv(t, set("RAW_CACHE_BYTES", "1048576"))
	e.mailbox(mara, 0, 0)
	body := make([]byte, 300<<10)
	for i := range body {
		body[i] = byte('a' + i%26)
		if i%70 == 69 {
			body[i] = '\n'
		}
	}
	for i := 0; i < 8; i++ {
		raw := append([]byte(fmt.Sprintf("From: a@outside.test\r\nSubject: big %d\r\n\r\n", i)), body...)
		e.stub.Deliver(mara, edgestub.Inbox, raw, time.Now())
	}
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	st := e.stub.State(mara, edgestub.Inbox)
	for round := 0; round < 2; round++ {
		msgs, err := c.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
		if err != nil || len(msgs) != 8 {
			t.Fatalf("fetch: %v", err)
		}
		for i, m := range msgs {
			if sha(bodySection(m, 0)) != st.Rows[i].Digest {
				t.Fatalf("mail %d differs", i+1)
			}
		}
		if b := e.metric("rawCache", "bytes"); b > 1<<20 {
			t.Fatalf("raw cache holds %d bytes, limit %d", b, 1<<20)
		}
	}
	if n := e.metric("rawCache", "evictions"); n == 0 {
		t.Fatal("nothing was evicted")
	}
}

// Mail bytes in work (here: SMTP DATA) share one budget; who does not fit
// waits for the others instead of growing the process.
func TestWorkMemoryBudget(t *testing.T) {
	// 32 MiB in total, 8 MiB per mailbox; one DATA reserves 5 MiB
	e := newEnv(t, set("WORK_MEM_BYTES", "33554432"), set("RAW_MAX_BYTES", "4194304"))
	e.mailbox(mara, 1, 0)
	e.stub.SetFault("/submit", edgestub.Fault{Delay: 300 * time.Millisecond})
	to := []string{"customer@outside.test"}
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func(i int) {
			c, err := smtp.Dial(e.b.SMTPAddr())
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			if err := c.Auth(sasl.NewPlainClient("", mara, e.pw[mara])); err != nil {
				errs <- err
				return
			}
			errs <- c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, fmt.Sprintf("parallel %d", i))))
		}(i)
	}
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("submission %d: %v", i, err)
		}
	}
	if n := e.metric("workMem", "waits"); n < 1 {
		t.Fatalf("nobody had to wait (waits = %d)", n)
	}
	if n := e.metric("workMem", "bytes"); n != 0 {
		t.Fatalf("%d bytes are still reserved", n)
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 3 {
		t.Fatalf("Sent has %d mails", n)
	}
}

// A connection that selects a folder and then neither idles nor asks would
// collect updates without end. It is closed; connections that keep up stay.
func TestSilentConnectionWithBacklogIsClosed(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 40, 0)
	silent := e.login(mara, nil)
	idler := e.login(mara, nil)
	worker := e.login(mara, nil)
	for _, c := range []*imapclient.Client{silent, idler, worker} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	idle, err := idler.Idle()
	if err != nil {
		t.Fatal(err)
	}
	var set imap.UIDSet
	set.AddRange(1, 40)
	for i := 0; i < 60; i++ { // 60 x 40 flag changes
		op := imap.StoreFlagsAdd
		if i%2 == 1 {
			op = imap.StoreFlagsDel
		}
		if err := worker.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close(); err != nil {
			t.Fatal(err)
		}
	}
	e.sync() // first look: behind
	e.sync() // second look: still behind, closed
	select {
	case <-silent.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("the silent connection stays open with thousands of pending updates")
	}
	if n := e.metric("mirror", "backlogKills"); n != 1 {
		t.Fatalf("backlogKills = %d", n)
	}
	if err := idle.Close(); err != nil {
		t.Fatalf("the idling connection was closed: %v", err)
	}
	_ = idle.Wait()
	compareWithStub(t, e, idler, mara, edgestub.Inbox, "idling connection")
	compareWithStub(t, e, worker, mara, edgestub.Inbox, "working connection")
}
