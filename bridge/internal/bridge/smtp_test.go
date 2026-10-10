package bridge_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

func (e *env) smtpDial() *smtp.Client {
	e.t.Helper()
	c, err := smtp.Dial(e.b.SMTPAddr())
	if err != nil {
		e.t.Fatalf("smtp dial: %v", err)
	}
	e.t.Cleanup(func() { _ = c.Close() })
	return c
}

func (e *env) smtpLogin(address string) *smtp.Client {
	e.t.Helper()
	c := e.smtpDial()
	if err := c.Auth(sasl.NewPlainClient("", address, e.pw[address])); err != nil {
		e.t.Fatalf("smtp auth: %v", err)
	}
	return c
}

func smtpCode(err error) int {
	if se, ok := err.(*smtp.SMTPError); ok {
		return se.Code
	}
	return 0
}

func outgoing(from string, to []string, subject string) []byte {
	return edgestub.BuildMail(edgestub.MailSpec{From: from, To: to, Subject: subject, Text: "Sent from the mail client.", Date: time.Now()})
}

func TestSMTPSubmitSent(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	c := e.smtpLogin(mara)
	if size, ok := c.MaxMessageSize(); !ok || size != 5<<20 {
		t.Fatalf("SIZE in EHLO = %d %v", size, ok)
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS is announced")
	}
	if !c.SupportsAuth("PLAIN") {
		t.Fatal("AUTH PLAIN is not announced")
	}
	to := []string{"customer@outside.test", "colleague@outside.test"}
	raw := outgoing(mara, to, "Quote")
	if err := c.SendMail(mara, to, bytes.NewReader(raw)); err != nil {
		t.Fatalf("send: %v", err)
	}
	last := e.stub.Last()
	if last.SubmitFrom != mara || fmt.Sprint(last.SubmitTo) != fmt.Sprint(to) {
		t.Fatalf("envelope at the edge = %q -> %v", last.SubmitFrom, last.SubmitTo)
	}
	sent := e.stub.State(mara, edgestub.Sent)
	if len(sent.Rows) != 1 || sent.Rows[0].Digest != sha(raw) {
		t.Fatalf("Sent at the edge: %d rows, bytes equal: %v", len(sent.Rows), len(sent.Rows) == 1 && sent.Rows[0].Digest == sha(raw))
	}
	if n := e.metric("smtp", "submitSent"); n != 1 {
		t.Fatalf("submitSent = %d", n)
	}

	// The mail program now files its copy: the same mail, no second one.
	ic := e.login(mara, nil)
	data, err := appendMail(ic, "Sent", raw)
	if err != nil {
		t.Fatalf("APPEND of the sent copy: %v", err)
	}
	if uint32(data.UID) != sent.Rows[0].UID {
		t.Fatalf("APPENDUID %d, sent mail has %d", data.UID, sent.Rows[0].UID)
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 1 {
		t.Fatalf("Sent has %d mails", n)
	}
	// a second mail on the same connection
	raw2 := outgoing(mara, to[:1], "Addendum")
	if err := c.SendMail(mara, to[:1], bytes.NewReader(raw2)); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 2 {
		t.Fatalf("Sent has %d mails", n)
	}
}

func TestSMTPUnknownOutcomeIs250(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	e.stub.SubmitHook = func(string, []string, []byte) *edgestub.SubmitResult {
		return &edgestub.SubmitResult{Status: "unknown"}
	}
	c := e.smtpLogin(mara)
	to := []string{"customer@outside.test"}
	if err := c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "unclear"))); err != nil {
		t.Fatalf("an unclear outcome must be a 250 for the mail program: %v", err)
	}
	if n := e.metric("smtp", "submitUnknown"); n != 1 {
		t.Fatalf("submitUnknown = %d", n)
	}
}

func TestSMTPEdgeErrors(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	to := []string{"customer@outside.test"}
	var next *edgestub.SubmitResult
	e.stub.SubmitHook = func(string, []string, []byte) *edgestub.SubmitResult { return next }

	cases := []edgestub.SubmitResult{
		{HTTP: 422, Error: "sender", SMTPCode: 550, Message: "Envelope does not match the headers"},
		{HTTP: 413, Error: "size", SMTPCode: 552, Message: "too large for the provider"},
		{HTTP: 422, Error: "content", SMTPCode: 554, Message: "signed mail cannot be sent"},
		{HTTP: 503, Error: "later", SMTPCode: 451, Message: "provider busy\r\n250 injected"},
	}
	for i := range cases {
		next = &cases[i]
		c := e.smtpLogin(mara)
		err := c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, fmt.Sprintf("Error %d", i))))
		if smtpCode(err) != cases[i].SMTPCode {
			t.Fatalf("case %d: got %v, want %d", i, err, cases[i].SMTPCode)
		}
		msg := err.(*smtp.SMTPError).Message
		if strings.ContainsAny(msg, "\r\n") || !strings.Contains(msg, strings.Fields(cases[i].Message)[0]) {
			t.Fatalf("case %d: message %q", i, msg)
		}
	}
	// no usable answer at all: temporary, and no retry by the bridge
	next = nil
	e.stub.SetFault("/submit", edgestub.Fault{Status: 500, Body: "broken"})
	calls := e.stub.Calls("/submit")
	c := e.smtpLogin(mara)
	err := c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "500")))
	if smtpCode(err) != 451 {
		t.Fatalf("edge 500: %v", err)
	}
	if got := e.stub.Calls("/submit") - calls; got != 1 {
		t.Fatalf("the bridge called /submit %d times", got)
	}
	e.stub.ClearFaults()
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 0 {
		t.Fatalf("Sent has %d mails after refused submissions", n)
	}
	if e.metric("smtp", "submitRejected") != 3 || e.metric("smtp", "submitDeferred") != 2 {
		t.Fatalf("rejected %d deferred %d", e.metric("smtp", "submitRejected"), e.metric("smtp", "submitDeferred"))
	}
}

func TestSMTPRules(t *testing.T) {
	e := newEnv(t, set("SMTP_MAX_MESSAGE_BYTES", "20000"))
	e.mailbox(mara, 1, 0)
	e.mailbox(jonas, 1, 0)
	to := []string{"customer@outside.test"}

	// without login: nothing is accepted
	c := e.smtpDial()
	if err := c.Mail(mara, nil); smtpCode(err) != 530 {
		t.Fatalf("MAIL without login: %v", err)
	}
	if err := c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "without login"))); err == nil {
		t.Fatal("mail accepted without login")
	}
	if n := e.stub.Calls("/submit"); n != 0 {
		t.Fatalf("/submit called %d times without login", n)
	}

	// MAIL FROM has to be the logged-in address
	c = e.smtpLogin(mara)
	for _, from := range []string{jonas, "foreign@outside.test", ""} {
		if err := c.Mail(from, nil); smtpCode(err) != 550 {
			t.Fatalf("MAIL FROM:<%s>: %v", from, err)
		}
	}
	if n := e.metric("smtp", "senderRejected"); n != 3 {
		t.Fatalf("senderRejected = %d", n)
	}
	// own address in another spelling is fine
	if err := c.Mail(strings.ToUpper(mara[:1])+mara[1:], nil); err != nil {
		t.Fatalf("MAIL FROM own address: %v", err)
	}
	// at most 50 recipients
	for i := 0; i < 50; i++ {
		if err := c.Rcpt(fmt.Sprintf("rcpt%d@outside.test", i), nil); err != nil {
			t.Fatalf("recipient %d: %v", i+1, err)
		}
	}
	if err := c.Rcpt("one-too-many@outside.test", nil); smtpCode(err) != 452 {
		t.Fatalf("51st recipient: %v", err)
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	// broken recipient
	if err := c.Mail(mara, nil); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"no-at-sign", "rëcipient@outside.test", "a b@outside.test"} {
		if err := c.Rcpt(bad, nil); smtpCode(err)/100 != 5 {
			t.Fatalf("recipient %q: %v", bad, err)
		}
	}
	_ = c.Reset()

	// too large: announced size, and actual size
	if err := c.Mail(mara, &smtp.MailOptions{Size: 30000}); smtpCode(err) != 552 {
		t.Fatalf("MAIL SIZE=30000: %v", err)
	}
	big := edgestub.BuildMail(edgestub.MailSpec{From: mara, To: to, Subject: "too big", Text: strings.Repeat("0123456789 0123456789 0123456789\n", 1000)})
	c2 := e.smtpLogin(mara)
	if err := c2.SendMail(mara, to, bytes.NewReader(big)); smtpCode(err) != 552 {
		t.Fatalf("DATA beyond the limit: %v", err)
	}
	// a line far beyond what mail allows: refused for good, not "try again"
	long := edgestub.BuildMail(edgestub.MailSpec{From: mara, To: to, Subject: "long line", Text: strings.Repeat("x", 5000)})
	c3 := e.smtpLogin(mara)
	if err := c3.SendMail(mara, to, bytes.NewReader(long)); smtpCode(err)/100 != 5 {
		t.Fatalf("DATA with an overlong line: %v", err)
	}
	if n := e.stub.Calls("/submit"); n != 0 {
		t.Fatalf("/submit called %d times", n)
	}

	// VRFY tells nothing, EXPN does not exist
	r := dialRaw(t, e.b.SMTPAddr())
	r.until("220 ")
	r.send("EHLO test.example.test\r\n")
	ehlo := strings.Join(r.until("250 "), "\n")
	if strings.Contains(ehlo, "STARTTLS") || !strings.Contains(ehlo, "AUTH PLAIN") || !strings.Contains(ehlo, "SIZE 20000") {
		t.Fatalf("EHLO = %q", ehlo)
	}
	r.send("VRFY " + mara + "\r\n")
	known, _ := r.line()
	r.send("VRFY nobody@mailbox-one.test\r\n")
	unknown, _ := r.line()
	if !strings.HasPrefix(known, "252 ") || known != unknown {
		t.Fatalf("VRFY distinguishes addresses: %q / %q", known, unknown)
	}
	r.send("EXPN list\r\n")
	if l, _ := r.line(); !strings.HasPrefix(l, "502 ") {
		t.Fatalf("EXPN: %q", l)
	}
}

func TestSMTPAuthFailures(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)
	e.mailbox(jonas, 1, 0)

	c := e.smtpDial()
	if err := c.Auth(sasl.NewPlainClient("", mara, "mb_wrong")); smtpCode(err) != 535 {
		t.Fatalf("wrong password: %v", err)
	}
	c = e.smtpDial()
	if err := c.Auth(sasl.NewPlainClient(jonas, mara, pw)); smtpCode(err) != 535 {
		t.Fatalf("foreign authzid: %v", err)
	}
	e.stub.SetPreparing(jonas, true)
	c = e.smtpDial()
	if err := c.Auth(sasl.NewPlainClient("", jonas, e.pw[jonas])); smtpCode(err) != 454 {
		t.Fatalf("preparing: %v", err)
	}

	// three failed attempts on one connection: closed
	r := dialRaw(t, e.b.SMTPAddr())
	r.until("220 ")
	r.send("EHLO test.example.test\r\n")
	r.until("250 ")
	for i := 0; i < 3; i++ {
		r.send("AUTH PLAIN AG1hcmFAcG9zdGZhY2gtZWlucy50ZXN0AG53bV9mYWxzY2g=\r\n")
		if l, err := r.line(); err != nil || !strings.HasPrefix(l, "535 ") {
			t.Fatalf("attempt %d: %q %v", i+1, l, err)
		}
	}
	if closed, _ := r.closed(3 * time.Second); !closed {
		t.Fatal("SMTP connection stays open after three failed attempts")
	}
	if n := e.metric("smtp", "authLockouts"); n != 1 {
		t.Fatalf("authLockouts = %d", n)
	}
}

// The device password is revoked between login and DATA: the edge rejects
// the session, the mail is not accepted, the connection ends.
func TestSMTPRevokedSession(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	c := e.smtpLogin(mara)
	e.stub.RevokePassword(mara, e.pw[mara])
	to := []string{"customer@outside.test"}
	err := c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "after revocation")))
	if smtpCode(err) != 535 {
		t.Fatalf("submission with a revoked session: %v", err)
	}
	if err := c.Noop(); err == nil {
		t.Fatal("the connection stays open")
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 0 {
		t.Fatalf("Sent has %d mails", n)
	}
}

// What arrives through SMTP shows up in Sent of an open IMAP connection.
func TestSMTPThenIMAPSeesSent(t *testing.T) {
	e := newEnv(t, withPoller())
	e.mailbox(mara, 1, 0)
	w := &watcher{}
	ic := e.login(mara, w.opts())
	if _, err := ic.Select("Sent", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	c := e.smtpLogin(mara)
	to := []string{"customer@outside.test"}
	raw := outgoing(mara, to, "visible")
	if err := c.SendMail(mara, to, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "sent mail in the Sent folder", func() bool {
		_ = ic.Noop().Wait()
		ex, _, _, _ := w.snapshot()
		return len(ex) > 0 && ex[len(ex)-1] == 1
	})
	msgs, err := ic.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil || len(msgs) != 1 || sha(bodySection(msgs[0], 0)) != sha(raw) {
		t.Fatalf("sent mail over IMAP: %v", err)
	}
}

// An SMTP connection ends with its session, like an IMAP connection: when
// the device password is revoked (seen by the poll) and when the session
// runs out.
func TestSMTPConnectionEndsWithItsSession(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	e.mailbox(jonas, 1, 0)
	revoked := e.smtpLogin(mara)
	other := e.smtpLogin(jonas)
	e.stub.RevokePassword(mara, e.pw[mara])
	e.sync()
	eventually(t, 5*time.Second, "revoked SMTP connection is closed", func() bool { return revoked.Noop() != nil })
	if err := other.Noop(); err != nil {
		t.Fatalf("SMTP connection of another mailbox: %v", err)
	}
	if n := e.metric("smtp", "sessionsEnded"); n != 1 {
		t.Fatalf("sessionsEnded = %d", n)
	}

	e.stub.SessionTTL = 300 * time.Millisecond
	pw2 := e.stub.AddPassword(mara)
	short := e.smtpDial()
	if err := short.Auth(sasl.NewPlainClient("", mara, pw2)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "SMTP connection ends at expiresAt", func() bool { return short.Noop() != nil })
	if n := e.metric("smtp", "sessionsExpired"); n != 1 {
		t.Fatalf("sessionsExpired = %d", n)
	}
}

// With BDAT a transfer can be left open. It has a fixed time, and its
// memory reservation ends with it.
func TestSMTPOpenTransferIsCutOff(t *testing.T) {
	e := newEnv(t, set("test.smtp_read_ms", "400"))
	e.mailbox(mara, 1, 0)
	r := dialRaw(t, e.b.SMTPAddr())
	r.until("220 ")
	r.send("EHLO test.example.test\r\n")
	r.until("250 ")
	auth := "\x00" + mara + "\x00" + e.pw[mara]
	r.send("AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte(auth)) + "\r\n")
	r.until("235 ")
	r.send("MAIL FROM:<" + mara + ">\r\nRCPT TO:<customer@outside.test>\r\n")
	r.until("250 ")
	r.until("250 ")
	r.send("BDAT 4\r\nabcd")
	r.until("250 ")
	eventually(t, 2*time.Second, "memory is reserved for the open transfer", func() bool { return e.metric("workMem", "bytes") > 0 })
	// keep the connection busy without ever finishing
	stop := time.Now().Add(1500 * time.Millisecond)
	closed := false
	for time.Now().Before(stop) && !closed {
		_ = r.c.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := r.c.Write([]byte("BDAT 0\r\n")); err != nil {
			closed = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ok, _ := r.closed(2 * time.Second); !ok && !closed {
		t.Fatal("an unfinished transfer keeps its connection")
	}
	eventually(t, 3*time.Second, "reservation is released", func() bool { return e.metric("workMem", "bytes") == 0 })
	// the mailbox can send normally again
	c := e.smtpLogin(mara)
	to := []string{"customer@outside.test"}
	if err := c.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "afterwards"))); err != nil {
		t.Fatalf("normal submission afterwards: %v", err)
	}
	if n := len(e.stub.State(mara, edgestub.Sent).Rows); n != 1 {
		t.Fatalf("Sent has %d mails", n)
	}
}
