package bridge_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

// The addendum to the contract after the reviews of the edge: unknown
// fields and large bodies are refused there, an unknown folder is 404, a
// state from the future is 410, 429 carries Retry-After, a valid password
// is never throttled. And what 400, 413 and 500 mean for the mail program.
//
// That the bridge sends exactly the fields of the contract is checked by
// every test of this package: the test edge refuses anything else, and the
// harness fails a test at whose end the edge has refused something.

func (e *env) status(path string, code int) int64 {
	return e.metric("edge", path, "status", fmt.Sprint(code))
}

// 400, 413 and 500 are no fault of the mail program: it hears "temporarily
// unavailable", nothing is repeated behind its back, and each status has
// its own counter.
func TestEdgeFaultsAreUnavailableAndCounted(t *testing.T) {
	e := newEnv(t)
	e.faultsExpected = true
	pw := e.mailbox(mara, 3, 1)
	fault := func(path string, status int) {
		code := map[int]string{400: "request", 413: "too_large", 500: "internal"}[status]
		e.stub.SetFault(path, edgestub.Fault{Status: status, Count: 1,
			Body: `{"error":"` + code + `","message":"Request for Mara <mara@workshop-one.test> invalid"}`})
	}

	// login
	for _, status := range []int{400, 413, 500} {
		fault("/login", status)
		calls := e.stub.Calls("/login")
		c := e.dial(nil)
		wantNo(t, fmt.Sprintf("login with %d", status), c.Login(mara, pw).Wait(), "UNAVAILABLE")
		if got := e.stub.Calls("/login") - calls; got != 1 {
			t.Fatalf("login with %d: %d calls", status, got)
		}
		// the same connection can try again; nothing sticks
		if err := c.Login(mara, pw).Wait(); err != nil {
			t.Fatalf("login after %d: %v", status, err)
		}
	}

	// reading a folder, changing flags, fetching a mail
	c := e.login(mara, nil)
	fault("/entries", 400)
	_, err := c.Select("INBOX", nil).Wait()
	wantNo(t, "SELECT with 400", err, "UNAVAILABLE")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT after the fault: %v", err)
	}
	fault("/store", 413)
	calls := e.stub.Calls("/store")
	err = c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close()
	wantNo(t, "STORE with 413", err, "UNAVAILABLE")
	if got := e.stub.Calls("/store") - calls; got != 1 {
		t.Fatalf("STORE with 413: %d calls", got)
	}
	if flags := e.stub.State(mara, edgestub.Inbox).Rows[0].Flags; strings.Contains(strings.Join(flags, " "), "Flagged") {
		t.Fatalf("flags changed although the edge refused: %v", flags)
	}
	fault("/raw", 500)
	_, err = c.Fetch(imap.UIDSetNum(2), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	wantNo(t, "FETCH with 500", err, "UNAVAILABLE")
	fault("/expunge", 500)
	if err := c.Store(imap.UIDSetNum(3), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	wantNo(t, "EXPUNGE with 500", c.Expunge().Close(), "UNAVAILABLE")
	fault("/folders", 400)
	_, err = c.Status("Sent", &imap.StatusOptions{NumMessages: true}).Wait()
	wantNo(t, "STATUS with 400", err, "UNAVAILABLE")
	fault("/append", 413)
	sent := e.stub.State(mara, edgestub.Sent).Rows[0]
	ac := c.Append("Sent", int64(len(sent.Raw)), nil)
	_, _ = ac.Write(sent.Raw)
	_ = ac.Close()
	_, err = ac.Wait()
	wantNo(t, "APPEND with 413", err, "UNAVAILABLE")
	// the connection is still good
	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("NOOP after the faults: %v", err)
	}

	// SMTP: 451 for the mail, a temporary failure for the login
	to := []string{"customer@outside.test"}
	for _, status := range []int{400, 413, 500} {
		fault("/submit", status)
		calls := e.stub.Calls("/submit")
		sc := e.smtpLogin(mara)
		err := sc.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, fmt.Sprintf("Error %d", status))))
		if smtpCode(err) != 451 {
			t.Fatalf("/submit with %d: %v", status, err)
		}
		if got := e.stub.Calls("/submit") - calls; got != 1 {
			t.Fatalf("/submit with %d: %d calls", status, got)
		}
		_ = sc.Close()
	}
	fault("/login", 500)
	sc := e.smtpDial()
	if code := smtpCode(sc.Auth(sasl.NewPlainClient("", mara, pw))); code < 400 || code > 499 {
		t.Fatalf("SMTP login with 500: code %d", code)
	}
	// a 500 is temporary even if it names a permanent SMTP answer
	e.stub.SetFault("/submit", edgestub.Fault{Status: 500, Count: 1, Body: `{"error":"internal","smtpCode":550,"message":"x"}`})
	sc = e.smtpLogin(mara)
	if err := sc.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "500 with 550"))); smtpCode(err) != 451 {
		t.Fatalf("/submit with 500 and smtpCode 550: %v", err)
	}

	// counters: one per status over all paths, and per path
	want := map[string]int64{"http400": 4, "http413": 4, "http500": 6, "oversize": 0, "notSent": 0}
	for k, n := range want {
		if got := e.metric("edgeFaults", k); got != n {
			t.Errorf("edgeFaults.%s = %d, want %d (%v)", k, got, n, e.metrics()["edgeFaults"])
		}
	}
	per := map[string]map[int]int64{
		"/login": {400: 1, 413: 1, 500: 2}, "/entries": {400: 1}, "/store": {413: 1}, "/raw": {500: 1},
		"/expunge": {500: 1}, "/folders": {400: 1}, "/append": {413: 1}, "/submit": {400: 1, 413: 1, 500: 2},
	}
	for path, by := range per {
		for status, n := range by {
			if got := e.status(path, status); got != n {
				t.Errorf("edge.%s.status.%d = %d, want %d", path, status, got, n)
			}
		}
	}
	if e.status("/login", 200) < 4 {
		t.Errorf("edge./login.status.200 = %d", e.status("/login", 200))
	}
	// the log names path and status of each, and nothing of what the edge
	// wrote in its message
	logs := e.logs.String()
	if got := strings.Count(logs, `"ev":"edge_fault"`); got != 14 {
		t.Errorf("%d edge_fault lines, want 14", got)
	}
	for _, want := range []string{`"path":"/store"`, `"status":413`, `"code":"too_large"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %s", want)
		}
	}
	if strings.Contains(logs, "workshop-one") || strings.Contains(logs, "Request") {
		t.Error("the log repeats the message of the edge")
	}
}

// A rejection of /submit that names its SMTP answer is the edge speaking to
// the mail program. It has its status counter, but it is no fault.
func TestSubmitRejectionWithSMTPCodeIsNoFault(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	to := []string{"customer@outside.test"}
	e.stub.SubmitHook = func(string, []string, []byte) *edgestub.SubmitResult {
		return &edgestub.SubmitResult{HTTP: 413, Error: "size", SMTPCode: 552, Message: "too large"}
	}
	sc := e.smtpLogin(mara)
	if err := sc.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "too big"))); smtpCode(err) != 552 {
		t.Fatalf("got %v", err)
	}
	if e.status("/submit", 413) != 1 || e.metric("edgeFaults", "http413") != 0 {
		t.Fatalf("status counter %d, fault counter %d", e.status("/submit", 413), e.metric("edgeFaults", "http413"))
	}
	if strings.Contains(e.logs.String(), "edge_fault") {
		t.Fatal("a designed rejection was logged as a fault")
	}
}

// clientIp at the edge is a bare address whatever the PROXY header looks
// like; the test edge refuses anything else with 400.
func TestClientIPReachesTheEdgeBare(t *testing.T) {
	e := newEnv(t, set("PROXY_PROTOCOL", "required"))
	pw := e.mailbox(mara, 1, 0)
	cases := map[string]string{
		"PROXY TCP4 198.51.100.23 10.0.0.9 51234 993\r\n":       "198.51.100.23",
		"PROXY TCP6 2001:db8:aa::9 2001:db8::1 51234 993\r\n":   "2001:db8:aa::9",
		"PROXY TCP6 ::ffff:198.51.100.24 ::1 51234 993\r\n":     "198.51.100.24",
		"PROXY TCP6 2001:DB8:0:0:0:0:0:5 2001:db8::1 1 993\r\n": "2001:db8::5",
	}
	for header, want := range cases {
		r := dialRaw(t, e.b.IMAPAddr())
		r.send(header)
		r.until("* OK")
		r.send("a LOGIN " + mara + " " + pw + "\r\n")
		r.until("a OK")
		if got := e.stub.Last().LoginIP; got != want {
			t.Fatalf("%q: clientIp at the edge = %q, want %q", strings.TrimSpace(header), got, want)
		}
	}
	// SMTP hands over the same form
	s := dialRaw(t, e.b.SMTPAddr())
	s.send("PROXY TCP6 2001:db8:bb::1 2001:db8::1 51000 465\r\n")
	s.until("220 ")
	s.send("EHLO probe.example.test\r\n")
	s.until("250 ")
	s.send("AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00"+mara+"\x00"+pw)) + "\r\n")
	s.until("235 ")
	if got := e.stub.Last().LoginIP; got != "2001:db8:bb::1" {
		t.Fatalf("SMTP: clientIp at the edge = %q", got)
	}
	// an address with a zone is not a client address: no connection
	before := e.metric("imap", "rejectedProxy")
	z := dialRaw(t, e.b.IMAPAddr())
	z.send("PROXY TCP6 fe80::1%eth0 2001:db8::1 51234 993\r\n")
	if closed, got := z.closed(3 * time.Second); !closed || got != "" {
		t.Fatalf("zone in the header: closed=%v %q", closed, got)
	}
	if e.metric("imap", "rejectedProxy") != before+1 {
		t.Fatal("zone in the header was not counted as rejected")
	}
	if n := e.stub.Answered("/login", 400); n != 0 {
		t.Fatalf("the edge refused %d logins", n)
	}
}

// Input the edge would not even take as a login attempt is answered as a
// failed login by the bridge, and never becomes a 400 there.
func TestLoginInputTheEdgeWouldRefuseIsNotSent(t *testing.T) {
	e := newEnv(t, set("MAX_AUTH_FAILURES", "20"))
	e.mailbox(mara, 1, 0)
	c := e.dial(nil)
	calls := e.stub.Calls("/login")
	for name, cred := range map[string][2]string{
		"empty password":  {mara, ""},
		"long password":   {mara, strings.Repeat("p", 257)},
		"long address":    {strings.Repeat("a", 321) + "@x.test", "pw"},
		"blank address":   {"   ", "pw"},
		"only odd blanks": {string(rune(0xfeff)), "pw"},
	} {
		wantNo(t, name, c.Login(cred[0], cred[1]).Wait(), "AUTHENTICATIONFAILED")
	}
	if got := e.stub.Calls("/login") - calls; got != 0 {
		t.Fatalf("%d logins reached the edge", got)
	}
	// at the limits it is the edge that says no
	wantNo(t, "longest password", c.Login(mara, strings.Repeat("p", 256)).Wait(), "AUTHENTICATIONFAILED")
	wantNo(t, "longest address", c.Login(strings.Repeat("a", 313)+"@x.test", "pw").Wait(), "AUTHENTICATIONFAILED")
	if got := e.stub.Calls("/login") - calls; got != 2 {
		t.Fatalf("%d logins reached the edge, want 2", got)
	}
}

// STORE with more flags than the edge takes in one list, and with a flag
// longer than it takes: split, or turned away, never a 400.
func TestStoreKeepsToTheFlagLimitsOfTheEdge(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 2, 0)
	// a mail that carries more keywords than one list holds
	many := make([]string, 0, 70)
	for i := 0; i < 70; i++ {
		many = append(many, fmt.Sprintf("kw%02d", i))
	}
	for i := 0; i < len(many); i += 35 {
		e.stub.SetFlags(mara, edgestub.Inbox, 1, many[i:i+35], nil)
	}
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// FLAGS (\Seen) replaces: all 70 keywords have to go, in two calls
	calls := e.stub.Calls("/store")
	if err := c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
		t.Fatalf("STORE FLAGS: %v", err)
	}
	if got := e.stub.Calls("/store") - calls; got != 2 {
		t.Fatalf("%d /store calls, want 2", got)
	}
	if flags := e.stub.State(mara, edgestub.Inbox).Rows[0].Flags; len(flags) != 1 || flags[0] != `\Seen` {
		t.Fatalf("flags at the edge: %v", flags)
	}
	// a keyword of 100 characters is fine, one of 101 is not a flag here
	ok := imap.Flag(strings.Repeat("k", 100))
	if err := c.Store(imap.UIDSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{ok}}, nil).Close(); err != nil {
		t.Fatalf("keyword of 100 characters: %v", err)
	}
	calls = e.stub.Calls("/store")
	long := imap.Flag(strings.Repeat("k", 101))
	if err := c.Store(imap.UIDSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{long}}, nil).Close(); err == nil {
		t.Fatal("keyword of 101 characters accepted")
	}
	if got := e.stub.Calls("/store") - calls; got != 0 {
		t.Fatalf("the long keyword reached the edge (%d calls)", got)
	}
}

// APPEND of a mail whose Message-ID the edge cannot look up: no such mail
// was sent through us, and the edge is not asked.
func TestAppendWithUnusableMessageIDIsNotSent(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 0, 1)
	c := e.login(mara, nil)
	for name, id := range map[string]string{
		"none":     "",
		"empty":    "<>",
		"too long": "<" + strings.Repeat("a", 501) + "@x.test>",
	} {
		raw := "From: " + mara + "\r\nTo: customer@outside.test\r\nSubject: x\r\n"
		if id != "" {
			raw += "Message-ID: " + id + "\r\n"
		}
		raw += "\r\nText\r\n"
		ac := c.Append("Sent", int64(len(raw)), nil)
		_, _ = ac.Write([]byte(raw))
		_ = ac.Close()
		_, err := ac.Wait()
		wantNo(t, "APPEND "+name, err, "CANNOT")
	}
	if n := e.stub.Calls("/append"); n != 0 {
		t.Fatalf("%d /append calls", n)
	}
}

// The edge does not have a folder (404 "folder"): the mirror of it goes,
// connections that have it open end, the answer is "unavailable", and the
// other folder is not touched.
func TestFolderTheEdgeDoesNotHave(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 3, 2)
	inSent := e.login(mara, nil)
	if _, err := inSent.Select("Sent", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := inSent.Fetch(all(), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect(); err != nil {
		t.Fatal(err)
	}
	inInbox := e.login(mara, nil)
	if _, err := inInbox.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	e.stub.DropFolder(mara, edgestub.Sent)
	e.sync()
	eventually(t, 5*time.Second, "the connection in Sent ends", func() bool { return inSent.Noop().Wait() != nil })
	if n := e.metric("mirror", "noFolder"); n != 1 {
		t.Fatalf("mirror.noFolder = %d", n)
	}
	if e.status("/changes", 404) != 1 {
		t.Fatalf("edge./changes.status.404 = %d", e.status("/changes", 404))
	}
	if n := e.metric("rawCache", "entries"); n != 0 {
		t.Fatalf("%d mails of the folder still cached", n)
	}

	c := e.login(mara, nil)
	_, err := c.Select("Sent", nil).Wait()
	wantNo(t, "SELECT of the missing folder", err, "UNAVAILABLE")
	if e.status("/entries", 404) != 1 {
		t.Fatalf("edge./entries.status.404 = %d", e.status("/entries", 404))
	}
	_, err = c.Status("Sent", &imap.StatusOptions{NumMessages: true}).Wait()
	wantNo(t, "STATUS of the missing folder", err, "UNAVAILABLE")
	// INBOX goes on, for the old connection and for a new one
	if err := inInbox.Noop().Wait(); err != nil {
		t.Fatalf("the connection in INBOX: %v", err)
	}
	wantFolder(t, e, "new connection", mara, edgestub.Inbox, readAll(t, c, "INBOX"))
	// a 404 is an answer of the contract, not a fault
	if n := e.metric("edgeFaults", "http400") + e.metric("edgeFaults", "http500"); n != 0 {
		t.Fatalf("%d faults counted", n)
	}
}

// The folder was set back at the edge (a backup, say): the state the mirror
// holds is one the folder no longer has, the edge answers 410, and the
// mirror is read again. Numbers that were given out before come back with
// other mails; nothing of the old ones may show, not from the cache either.
func TestStateFromTheFutureIsReadAgain(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 5, 0)
	a := e.login(mara, nil)
	before := readAll(t, a, "INBOX") // the bodies of all five are cached now
	if len(before) != 5 {
		t.Fatalf("%d mails", len(before))
	}

	e.stub.Rewind(mara, edgestub.Inbox, 2)
	e.sync()
	// same numbering, three mails left: the open connection learns it
	eventually(t, 5*time.Second, "two mails leave the open folder", func() bool {
		_ = a.Noop().Wait()
		return a.Mailbox() != nil && a.Mailbox().NumMessages == 3
	})
	if e.status("/changes", 410) != 1 || e.metric("mirror", "resyncs") != 1 {
		t.Fatalf("410 counted %d times, resyncs %d", e.status("/changes", 410), e.metric("mirror", "resyncs"))
	}
	if n := e.metric("rawCache", "entries"); n != 0 {
		t.Fatalf("%d mails still cached after the folder was set back", n)
	}

	// the numbers 4 and 5 are given out again, to other mails
	for i := 0; i < 2; i++ {
		e.stub.Deliver(mara, edgestub.Inbox, edgestub.BuildMail(edgestub.MailSpec{
			From: "new@outside.test", To: []string{mara}, Subject: fmt.Sprintf("After the reset %d", i),
			Date: time.Date(2026, 10, 1, 9, i, 0, 0, time.UTC), Text: "A different mail under an old UID.",
		}), time.Date(2026, 10, 1, 9, i, 0, 0, time.UTC))
	}
	e.sync()
	st := e.stub.State(mara, edgestub.Inbox)
	if len(st.Rows) != 5 || st.Rows[4].UID != 5 {
		t.Fatalf("the test edge did not reuse the numbers: %d rows", len(st.Rows))
	}
	after := readAll(t, a, "INBOX")
	wantFolder(t, e, "open connection", mara, edgestub.Inbox, after)
	for _, uid := range []uint32{4, 5} {
		if after[uid] == before[uid] {
			t.Fatalf("uid %d still shows the mail from before", uid)
		}
	}
	fresh := e.login(mara, nil)
	wantFolder(t, e, "new connection", mara, edgestub.Inbox, readAll(t, fresh, "INBOX"))
}

// The same, but a number is reused before the bridge looks again: one
// number, two different mails. No open view can be repaired; it is closed,
// and nothing cached under the number survives. Once found through the 410
// (the state of the edge is below the mirror's), once through an ordinary
// change (the state is above it again, only size and date can tell).
func TestReusedNumberClosesOpenViews(t *testing.T) {
	other := func(i int) []byte {
		return edgestub.BuildMail(edgestub.MailSpec{
			From: "new@outside.test", To: []string{mara}, Subject: fmt.Sprintf("Under the old UID %d", i),
			Date: time.Date(2026, 10, 2, 9, i, 0, 0, time.UTC), Text: "This mail is longer than the old one and carries its UID.",
		})
	}
	for _, how := range []string{"resync", "delta"} {
		t.Run(how, func(t *testing.T) {
			e := newEnv(t)
			e.mailbox(mara, 5, 0)
			a := e.login(mara, nil)
			before := readAll(t, a, "INBOX")
			since := e.stub.State(mara, edgestub.Inbox).HighestModseq

			e.stub.Rewind(mara, edgestub.Inbox, 2)
			e.stub.Deliver(mara, edgestub.Inbox, other(0), time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
			if how == "delta" {
				// changes until the state of the edge is beyond the mirror's
				// and the new mail under number 4 is among what changed
				for i := 0; e.stub.State(mara, edgestub.Inbox).HighestModseq <= since+1; i++ {
					e.stub.SetFlags(mara, edgestub.Inbox, 1, []string{fmt.Sprintf("k%d", i)}, nil)
				}
				e.stub.SetFlags(mara, edgestub.Inbox, 4, []string{`\Flagged`}, nil)
			} else if got := e.stub.State(mara, edgestub.Inbox).HighestModseq; got >= since {
				t.Fatalf("the state of the test edge is %d, not below %d", got, since)
			}
			e.sync()
			eventually(t, 5*time.Second, "the open connection ends", func() bool { return a.Noop().Wait() != nil })
			if n := e.metric("mirror", "inconsistent"); n != 1 {
				t.Fatalf("mirror.inconsistent = %d", n)
			}
			want410 := int64(0)
			if how == "resync" {
				want410 = 1
			}
			if got := e.status("/changes", 410); got != want410 {
				t.Fatalf("410 counted %d times, want %d", got, want410)
			}
			fresh := e.login(mara, nil)
			after := readAll(t, fresh, "INBOX")
			wantFolder(t, e, "new connection", mara, edgestub.Inbox, after)
			if len(after) != 4 || after[4] == before[4] {
				t.Fatalf("uid 4 still shows the mail from before (%d mails)", len(after))
			}
		})
	}
}

// /seq with entries that cannot be sessions: the edge answers them as
// invalid at their place, not with an error. (What the bridge sends is
// always what /login gave it; this pins the behaviour of the test edge the
// poller relies on.)
func TestPollerEndsOnlyTheConnectionsWhoseSessionIsGone(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	e.mailbox(jonas, 1, 0)
	a := e.login(mara, &imapclient.Options{})
	b := e.login(jonas, nil)
	e.stub.ExpireSessions(mara)
	e.sync()
	eventually(t, 5*time.Second, "the connection of the expired session ends", func() bool { return a.Noop().Wait() != nil })
	if err := b.Noop().Wait(); err != nil {
		t.Fatalf("the other connection: %v", err)
	}
	if e.stub.Answered("/seq", 400) != 0 {
		t.Fatal("/seq was refused")
	}
}

// What /metrics shows for the edge: per path the answers by status, and the
// fault counters, all present from the start.
func TestMetricsShowAnswersByStatus(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 2, 0)
	c := e.login(mara, nil)
	readAll(t, c, "INBOX")
	wantNo(t, "wrong password", e.dial(nil).Login(mara, "mb_wrong").Wait(), "AUTHENTICATIONFAILED")

	raw, _ := json.Marshal(e.metrics())
	var m struct {
		Edge map[string]struct {
			Count  int64            `json:"count"`
			Errors int64            `json:"errors"`
			Status map[string]int64 `json:"status"`
		} `json:"edge"`
		EdgeFaults map[string]int64 `json:"edgeFaults"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	login := m.Edge["/login"]
	if login.Count != 2 || login.Errors != 1 || login.Status["200"] != 1 || login.Status["401"] != 1 {
		t.Fatalf("/login: %+v", login)
	}
	if got := m.Edge["/raw"]; got.Status["200"] != 2 || got.Count != 2 {
		t.Fatalf("/raw: %+v", got)
	}
	for _, k := range []string{"http400", "http413", "http500", "oversize", "notSent"} {
		if n, ok := m.EdgeFaults[k]; !ok || n != 0 {
			t.Fatalf("edgeFaults.%s = %d (present: %v)", k, n, ok)
		}
	}
	// no answer at all has its own key
	e.srv.Close()
	wantNo(t, "edge down", e.dial(nil).Login(mara, e.pw[mara]).Wait(), "UNAVAILABLE")
	if e.metric("edge", "/login", "status", "none") != 1 {
		t.Fatalf("status.none = %d", e.metric("edge", "/login", "status", "none"))
	}
}

// What the probe against a real edge does first with every folder, on a
// folder without mails: UID FETCH 1:* is an empty answer, not an error.
func TestListingAnEmptyFolder(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 0, 0)
	c := e.login(mara, nil)
	for _, folder := range []string{"INBOX", "Sent"} {
		sel, err := c.Select(folder, nil).Wait()
		if err != nil || sel.NumMessages != 0 || sel.UIDNext != 1 {
			t.Fatalf("SELECT %s: %v %+v", folder, err, sel)
		}
		msgs, err := c.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}).Collect()
		if err != nil || len(msgs) != 0 {
			t.Fatalf("UID FETCH 1:* in empty %s: %v, %d rows", folder, err, len(msgs))
		}
		st, err := c.Status(folder, &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true}).Wait()
		if err != nil || *st.NumMessages != 0 || *st.NumUnseen != 0 {
			t.Fatalf("STATUS %s: %v", folder, err)
		}
	}
}

// A mail is asked for at the edge; before the answer is back, the folder is
// set back and the number goes to another mail, one of the same size and
// date, so that nothing but the bytes tells the two apart. The late answer
// is the old mail. It must not end up in the cache, where the next
// connection would find it under the number of the new one.
func TestLateRawAnswerIsNotKeptAfterTheFolderWasSetBack(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 2, 0)
	when := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	mail := func(word string) []byte {
		return edgestub.BuildMail(edgestub.MailSpec{
			From: "sender@outside.test", To: []string{mara}, Subject: "Same length",
			Date: when, MessageID: "same-length@mail.example.test", Text: "This mail is the " + word + ".",
		})
	}
	oldMail, newMail := mail("old"), mail("new")
	if len(oldMail) != len(newMail) || sha(oldMail) == sha(newMail) {
		t.Fatal("the two mails have to differ in content only")
	}
	e.stub.Deliver(mara, edgestub.Inbox, oldMail, when) // number 3
	e.stub.Deliver(mara, edgestub.Inbox, mail("fourth, which only counts"), when)

	a := e.login(mara, nil)
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// the answer for number 3 is made up now and leaves in 700 ms
	e.stub.SetFault("/raw", edgestub.Fault{Late: true, Delay: 700 * time.Millisecond, Count: 1})
	late := make(chan string, 1)
	go func() {
		msgs, err := a.Fetch(imap.UIDSetNum(3), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
		if err != nil || len(msgs) != 1 {
			late <- ""
			return
		}
		late <- sha(bodySection(msgs[0], 0))
	}()
	// (not "the call has arrived": the edge has to have read the old mail
	// before the folder is set back, or the late answer is the new one)
	eventually(t, 5*time.Second, "the edge has made up its answer", func() bool { return e.stub.Answered("/raw", 200) == 1 })

	// meanwhile: numbers 3 and 4 are gone, and 3 is given to the other mail
	e.stub.Rewind(mara, edgestub.Inbox, 2)
	e.stub.Deliver(mara, edgestub.Inbox, newMail, when)
	e.sync()
	if e.status("/changes", 410) != 1 || e.metric("mirror", "resyncs") != 1 {
		t.Fatalf("the folder was not read again: 410 %d times, resyncs %d", e.status("/changes", 410), e.metric("mirror", "resyncs"))
	}
	// the late answer is in, and it was the old mail
	if got := <-late; got != sha(oldMail) {
		t.Fatalf("the late answer was not the old mail (the test does not test what it should)")
	}

	fresh := e.login(mara, nil)
	got := readAll(t, fresh, "INBOX")
	if got[3] == sha(oldMail) {
		t.Fatal("number 3 shows the mail from before the folder was set back: a late answer was cached")
	}
	wantFolder(t, e, "new connection", mara, edgestub.Inbox, got)
}

// APPEND of a mail whose Message-ID is empty for the edge (it trims the
// way JavaScript does) is answered here, not with a 400 from there.
func TestAppendWithAMessageIDOfBlanksIsNotSent(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 0, 1)
	c := e.login(mara, nil)
	raw := "From: " + mara + "\r\nTo: customer@outside.test\r\nSubject: x\r\nMessage-ID: <" + string(rune(0xfeff)) + ">\r\n\r\nText\r\n"
	ac := c.Append("Sent", int64(len(raw)), nil)
	_, _ = ac.Write([]byte(raw))
	_ = ac.Close()
	_, err := ac.Wait()
	wantNo(t, "APPEND", err, "CANNOT")
	if n := e.stub.Calls("/append"); n != 0 {
		t.Fatalf("%d /append calls", n)
	}
}
