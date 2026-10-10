package bridge_test

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func TestLoginOK(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 3, 1)
	c := e.login(mara, nil)
	// (the client library sets its state a moment after Wait returns)
	eventually(t, 2*time.Second, "client is authenticated", func() bool { return c.State() == imap.ConnStateAuthenticated })
	if got := e.stub.Last().LoginIP; got != "127.0.0.1" {
		t.Fatalf("clientIp sent to the edge = %q", got)
	}
	if n := e.metric("imap", "loginsOk"); n != 1 {
		t.Fatalf("loginsOk = %d", n)
	}
	caps := c.Caps()
	for _, bad := range []imap.Cap{imap.CapMove, imap.CapID, imap.CapESearch, imap.CapCondStore, imap.CapLiteralPlus} {
		if caps.Has(bad) {
			t.Errorf("capability %s is announced", bad)
		}
	}
	for _, want := range []imap.Cap{imap.CapIMAP4rev1, imap.CapUIDPlus, imap.CapSpecialUse, imap.CapNamespace, imap.CapIdle} {
		if !caps.Has(want) {
			t.Errorf("capability %s is missing", want)
		}
	}
}

func TestLoginWrongRevokedUnknownLookAlike(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)
	revoked := e.stub.AddPassword(mara)
	e.stub.RevokePassword(mara, revoked)

	cases := []struct{ name, user, pass string }{
		{"wrong password", mara, pw + "x"},
		{"revoked password", mara, revoked},
		{"unknown address", "nobody@mailbox-one.test", pw},
		{"empty-ish password", mara, " "},
	}
	var texts []string
	for _, tc := range cases {
		c := e.dial(nil)
		err := c.Login(tc.user, tc.pass).Wait()
		wantNo(t, tc.name, err, "AUTHENTICATIONFAILED")
		texts = append(texts, err.(*imap.Error).Text)
	}
	for _, x := range texts {
		if x != texts[0] {
			t.Fatalf("answers differ: %q", texts)
		}
	}
	if n := e.metric("imap", "loginsFailed"); n != 4 {
		t.Fatalf("loginsFailed = %d", n)
	}
}

func TestLoginThrottledAndPreparing(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)
	e.stub.ThrottleAfter = 2

	for i := 0; i < 2; i++ {
		c := e.dial(nil)
		wantNo(t, "wrong", c.Login(mara, "mb_wrong").Wait(), "AUTHENTICATIONFAILED")
	}
	// The next wrong attempt is held back, and the answer is not "wrong
	// password".
	c := e.dial(nil)
	wantNo(t, "throttled", c.Login(mara, "mb_wrong").Wait(), "UNAVAILABLE")
	if n := e.metric("imap", "loginsThrottled"); n != 1 {
		t.Fatalf("loginsThrottled = %d", n)
	}
	// While the edge's waiting time runs, the bridge answers the same
	// attempt by itself: repeating it does not cost a call, on IMAP or SMTP.
	calls := e.stub.Calls("/login")
	for i := 0; i < 4; i++ {
		c := e.dial(nil)
		wantNo(t, "throttled again", c.Login(mara, "mb_wrong").Wait(), "UNAVAILABLE")
	}
	if sc, err := smtp.Dial(e.b.SMTPAddr()); err == nil {
		if err := sc.Auth(sasl.NewPlainClient("", mara, "mb_wrong")); err == nil {
			t.Fatal("SMTP login passed with the wrong password")
		}
		_ = sc.Close()
	}
	if got := e.stub.Calls("/login") - calls; got != 0 {
		t.Fatalf("%d more /login calls for the attempt that was throttled", got)
	}
	// Another wrong password is not something the bridge has an answer for:
	// the edge is asked, and says "later" again.
	c = e.dial(nil)
	wantNo(t, "other wrong password", c.Login(mara, "mb_also_wrong").Wait(), "UNAVAILABLE")
	if got := e.stub.Calls("/login") - calls; got != 1 {
		t.Fatalf("%d /login calls for a new wrong password, want 1", got)
	}
	// A valid device password is never throttled: neither by the edge nor
	// by what the bridge remembers, although address and client are the
	// very ones that were just told to wait. (A second device behind the
	// same connection must not be locked out by the first one's old
	// password.)
	c = e.dial(nil)
	if err := c.Login(mara, pw).Wait(); err != nil {
		t.Fatalf("the right password while wrong ones are throttled: %v", err)
	}
	sc := e.smtpLogin(mara)
	_ = sc.Close()
	if n := e.stub.Answered("/login", 429); n != 2 {
		t.Fatalf("the edge said 429 %d times, want 2", n)
	}
	// The waiting time may come in the header alone.
	e.stub.ThrottleHeaderOnly = true
	c = e.dial(nil)
	wantNo(t, "throttled, header only", c.Login(mara, "mb_third").Wait(), "UNAVAILABLE")
	calls = e.stub.Calls("/login")
	c = e.dial(nil)
	wantNo(t, "throttled, header only, again", c.Login(mara, "mb_third").Wait(), "UNAVAILABLE")
	if got := e.stub.Calls("/login") - calls; got != 0 {
		t.Fatalf("%d more /login calls after a 429 with Retry-After", got)
	}

	e.mailbox(jonas, 1, 0)
	e.stub.SetPreparing(jonas, true)
	c2 := e.dial(nil)
	err := c2.Login(jonas, e.pw[jonas]).Wait()
	wantNo(t, "preparing", err, "UNAVAILABLE")
	if !strings.Contains(err.(*imap.Error).Text, "prepared") {
		t.Fatalf("text = %q", err.(*imap.Error).Text)
	}
	e.stub.SetPreparing(jonas, false)
	if err := c2.Login(jonas, e.pw[jonas]).Wait(); err != nil {
		t.Fatalf("after preparing: %v", err)
	}
}

func TestLoginEdgeDown(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)
	e.srv.Close()
	c := e.dial(nil)
	wantNo(t, "edge down", c.Login(mara, pw).Wait(), "UNAVAILABLE")
}

func TestAuthenticatePlainAuthzid(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)
	e.mailbox(jonas, 1, 0)

	// foreign authorization identity: refused, without asking the edge
	before := e.stub.Calls("/login")
	c := e.dial(nil)
	err := c.Authenticate(sasl.NewPlainClient(jonas, mara, pw))
	wantNo(t, "foreign authzid", err, "AUTHORIZATIONFAILED")
	if e.stub.Calls("/login") != before {
		t.Fatal("the edge was asked although the authzid was foreign")
	}

	// empty and identical identity: fine
	c2 := e.dial(nil)
	if err := c2.Authenticate(sasl.NewPlainClient("", mara, pw)); err != nil {
		t.Fatalf("empty authzid: %v", err)
	}
	c3 := e.dial(nil)
	if err := c3.Authenticate(sasl.NewPlainClient(mara, mara, pw)); err != nil {
		t.Fatalf("same authzid: %v", err)
	}
	// wrong password through AUTHENTICATE
	c4 := e.dial(nil)
	wantNo(t, "wrong", c4.Authenticate(sasl.NewPlainClient("", mara, "mb_wrong")), "AUTHENTICATIONFAILED")
}

func TestThreeFailedAttemptsCloseTheConnection(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	r := dialRaw(t, e.b.IMAPAddr())
	r.until("* OK")
	for i := 1; i <= 3; i++ {
		r.send("a" + string(rune('0'+i)) + " LOGIN " + mara + " mb_wrong\r\n")
		lines := r.until("a" + string(rune('0'+i)) + " ")
		if !strings.Contains(lines[len(lines)-1], "NO [AUTHENTICATIONFAILED]") {
			t.Fatalf("attempt %d: %q", i, lines)
		}
	}
	if closed, _ := r.closed(3 * time.Second); !closed {
		t.Fatal("connection stays open after three failed attempts")
	}
	if n := e.metric("imap", "authLockouts"); n != 1 {
		t.Fatalf("authLockouts = %d", n)
	}
	// Malformed AUTHENTICATE answers count as well.
	r2 := dialRaw(t, e.b.IMAPAddr())
	r2.until("* OK")
	for i := 1; i <= 3; i++ {
		tag := "b" + string(rune('0'+i))
		r2.send(tag + " AUTHENTICATE PLAIN\r\n")
		r2.until("+")
		r2.send("bm9wZQ==\r\n") // "nope": not three parts
		r2.until(tag + " ")
	}
	if closed, _ := r2.closed(3 * time.Second); !closed {
		t.Fatal("connection stays open after three malformed attempts")
	}
}
