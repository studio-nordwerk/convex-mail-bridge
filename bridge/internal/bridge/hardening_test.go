package bridge_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

// ---- before login ----------------------------------------------------------

func TestPreauthLongLineClosesTheConnection(t *testing.T) {
	e := newEnv(t)
	r := dialRaw(t, e.b.IMAPAddr())
	r.until("* OK")
	r.send("a LOGIN " + strings.Repeat("A", 3000))
	closed, got := r.closed(3 * time.Second)
	if !closed {
		t.Fatal("an overlong line before login keeps the connection open")
	}
	if strings.Contains(got, "a ") {
		t.Fatalf("the server answered the overlong line: %q", got)
	}
	if n := e.metric("imap", "preauthViolations"); n != 1 {
		t.Fatalf("preauthViolations = %d", n)
	}
	// the same for SMTP
	s := dialRaw(t, e.b.SMTPAddr())
	s.until("220 ")
	s.send("EHLO " + strings.Repeat("a", 3000))
	if closed, _ := s.closed(3 * time.Second); !closed {
		t.Fatal("SMTP: an overlong line before login keeps the connection open")
	}
	if n := e.metric("smtp", "preauthViolations"); n != 1 {
		t.Fatalf("smtp preauthViolations = %d", n)
	}
}

func TestPreauthLiterals(t *testing.T) {
	e := newEnv(t)
	pw := e.mailbox(mara, 1, 0)

	// a literal that is not tiny: closed, and never asked to go on
	for _, marker := range []string{"{5000}", "{5000+}", "{99999999999999999999}"} {
		r := dialRaw(t, e.b.IMAPAddr())
		r.until("* OK")
		r.send("a LOGIN " + marker + "\r\n")
		closed, got := r.closed(3 * time.Second)
		if !closed {
			t.Fatalf("%s before login keeps the connection open", marker)
		}
		if strings.Contains(got, "+") {
			t.Fatalf("%s: the server asked for the literal: %q", marker, got)
		}
	}
	// tiny literals work, synchronizing and not
	r := dialRaw(t, e.b.IMAPAddr())
	r.until("* OK")
	r.send(fmt.Sprintf("a LOGIN {%d}\r\n", len(mara)))
	r.until("+ ")
	r.send(fmt.Sprintf("%s {%d+}\r\n%s\r\n", mara, len(pw), pw))
	if lines := r.until("a "); !strings.HasPrefix(lines[len(lines)-1], "a OK") {
		t.Fatalf("login with literals: %q", lines)
	}
	r.send("b NOOP\r\n")
	r.until("b OK")
}

func TestPreauthByteBudget(t *testing.T) {
	e := newEnv(t, set("PREAUTH_MAX_BYTES", "1024"))
	r := dialRaw(t, e.b.IMAPAddr())
	r.until("* OK")
	// Harmless commands, but never a login: after 1024 bytes it is over.
	for i := 0; i < 200; i++ {
		_ = r.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(r.c, "n NOOP\r\n"); err != nil {
			break
		}
	}
	if closed, _ := r.closed(3 * time.Second); !closed {
		t.Fatal("endless commands before login keep the connection open")
	}
	if n := e.metric("imap", "preauthViolations"); n != 1 {
		t.Fatalf("preauthViolations = %d", n)
	}
}

func TestPreauthTimeout(t *testing.T) {
	e := newEnv(t, set("test.preauth_ms", "300"))
	e.mailbox(mara, 1, 0)
	lazy := dialRaw(t, e.b.IMAPAddr())
	lazy.until("* OK")
	busy := dialRaw(t, e.b.IMAPAddr()) // keeps talking but never logs in
	busy.until("* OK")
	in := e.login(mara, nil)

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		busy.send("x NOOP\r\n")
		time.Sleep(50 * time.Millisecond)
	}
	closed, got := lazy.closed(2 * time.Second)
	if !closed || !strings.Contains(got, "* BYE") {
		t.Fatalf("silent connection: closed=%v %q", closed, got)
	}
	if closed, _ := busy.closed(2 * time.Second); !closed {
		t.Fatal("a connection that keeps sending NOOP is never closed")
	}
	// the logged-in connection has no such limit
	time.Sleep(400 * time.Millisecond)
	if err := in.Noop().Wait(); err != nil {
		t.Fatalf("logged-in connection was closed by the login timer: %v", err)
	}
	if n := e.metric("imap", "preauthTimeouts"); n != 2 {
		t.Fatalf("preauthTimeouts = %d", n)
	}
	// SMTP
	s := dialRaw(t, e.b.SMTPAddr())
	s.until("220 ")
	closed, got = s.closed(2 * time.Second)
	if !closed || !strings.Contains(got, "421 ") {
		t.Fatalf("SMTP silent connection: closed=%v %q", closed, got)
	}
}

// ---- connection limits -------------------------------------------------------

func TestConnectionLimitPerClient(t *testing.T) {
	e := newEnv(t, set("IMAP_MAX_CONNS_PER_IP", "3"), set("SMTP_MAX_CONNS_PER_IP", "2"))
	var open []*rawConn
	for i := 0; i < 3; i++ {
		r := dialRaw(t, e.b.IMAPAddr())
		r.until("* OK")
		open = append(open, r)
	}
	over := dialRaw(t, e.b.IMAPAddr())
	closed, got := over.closed(3 * time.Second)
	if !closed || !strings.HasPrefix(got, "* BYE") {
		t.Fatalf("fourth connection: closed=%v %q", closed, got)
	}
	if n := e.metric("imap", "rejectedPerIp"); n != 1 {
		t.Fatalf("rejectedPerIp = %d", n)
	}
	// a slot comes back when a connection ends
	_ = open[0].c.Close()
	eventually(t, 3*time.Second, "slot is free again", func() bool { return e.metric("imap", "connsOpen") == 2 })
	again := dialRaw(t, e.b.IMAPAddr())
	again.until("* OK")

	for i := 0; i < 2; i++ {
		s := dialRaw(t, e.b.SMTPAddr())
		s.until("220 ")
	}
	s := dialRaw(t, e.b.SMTPAddr())
	closed, got = s.closed(3 * time.Second)
	if !closed || !strings.HasPrefix(got, "421 ") {
		t.Fatalf("third SMTP connection: closed=%v %q", closed, got)
	}
}

func TestConnectionLimitTotal(t *testing.T) {
	e := newEnv(t, set("IMAP_MAX_CONNS", "2"))
	for i := 0; i < 2; i++ {
		r := dialRaw(t, e.b.IMAPAddr())
		r.until("* OK")
	}
	over := dialRaw(t, e.b.IMAPAddr())
	closed, got := over.closed(3 * time.Second)
	if !closed || !strings.HasPrefix(got, "* BYE") {
		t.Fatalf("third connection: closed=%v %q", closed, got)
	}
	if n := e.metric("imap", "rejectedTotal"); n != 1 {
		t.Fatalf("rejectedTotal = %d", n)
	}
}

// ---- PROXY protocol ------------------------------------------------------------

func proxyV2(src, dst net.IP, sport, dport uint16) []byte {
	b := []byte("\r\n\r\n\x00\r\nQUIT\n")
	b = append(b, 0x21) // version 2, PROXY
	if src.To4() != nil {
		b = append(b, 0x11, 0, 12)
		b = append(b, src.To4()...)
		b = append(b, dst.To4()...)
	} else {
		b = append(b, 0x21, 0, 36)
		b = append(b, src.To16()...)
		b = append(b, dst.To16()...)
	}
	b = binary.BigEndian.AppendUint16(b, sport)
	b = binary.BigEndian.AppendUint16(b, dport)
	return b
}

func TestProxyProtocolRequired(t *testing.T) {
	e := newEnv(t, set("PROXY_PROTOCOL", "required"), set("IMAP_MAX_CONNS_PER_IP", "2"))
	pw := e.mailbox(mara, 1, 0)

	// no header, or something that only looks like one: closed, no greeting
	for name, first := range map[string]string{
		"imap command":    "a NOOP\r\n",
		"garbage":         "PROXY TCP4 nonsense\r\n",
		"unknown":         "PROXY UNKNOWN\r\n",
		"family mismatch": "PROXY TCP4 2001:db8::1 2001:db8::2 1 2\r\n",
		"overlong":        "PROXY TCP4 " + strings.Repeat("1", 200) + "\r\n",
		"v2 bad version":  "\r\n\r\n\x00\r\nQUIT\n\x31\x11\x00\x0c" + strings.Repeat("\x01", 12),
	} {
		r := dialRaw(t, e.b.IMAPAddr())
		r.send(first)
		closed, got := r.closed(3 * time.Second)
		if !closed || got != "" {
			t.Fatalf("%s: closed=%v, server sent %q", name, closed, got)
		}
	}
	if n := e.metric("imap", "rejectedProxy"); n != 6 {
		t.Fatalf("rejectedProxy = %d", n)
	}

	// version 1: the client address reaches the edge
	r := dialRaw(t, e.b.IMAPAddr())
	r.send("PROXY TCP4 203.0.113.7 10.0.0.9 51234 993\r\n")
	r.until("* OK")
	r.send("a LOGIN " + mara + " " + pw + "\r\n")
	r.until("a OK")
	if got := e.stub.Last().LoginIP; got != "203.0.113.7" {
		t.Fatalf("clientIp at the edge = %q", got)
	}

	// version 2, IPv6
	r6 := dialRaw(t, e.b.IMAPAddr())
	if _, err := r6.c.Write(proxyV2(net.ParseIP("2001:db8:7::15"), net.ParseIP("2001:db8::1"), 40000, 993)); err != nil {
		t.Fatal(err)
	}
	r6.until("* OK")
	r6.send("a LOGIN " + mara + " " + pw + "\r\n")
	r6.until("a OK")
	if got := e.stub.Last().LoginIP; got != "2001:db8:7::15" {
		t.Fatalf("clientIp at the edge = %q", got)
	}

	// limits count the client address of the header (IPv6: the /64)
	second := dialRaw(t, e.b.IMAPAddr())
	second.send("PROXY TCP4 203.0.113.7 10.0.0.9 51235 993\r\n")
	second.until("* OK")
	third := dialRaw(t, e.b.IMAPAddr())
	third.send("PROXY TCP4 203.0.113.7 10.0.0.9 51236 993\r\n")
	if closed, got := third.closed(3 * time.Second); !closed || !strings.HasPrefix(got, "* BYE") {
		t.Fatalf("third connection of one client: closed=%v %q", closed, got)
	}
	neighbour := dialRaw(t, e.b.IMAPAddr())
	neighbour.send("PROXY TCP4 203.0.113.8 10.0.0.9 51237 993\r\n")
	neighbour.until("* OK")
	same64 := dialRaw(t, e.b.IMAPAddr())
	_, _ = same64.c.Write(proxyV2(net.ParseIP("2001:db8:7::16"), net.ParseIP("2001:db8::1"), 40001, 993))
	same64.until("* OK")
	same64b := dialRaw(t, e.b.IMAPAddr())
	_, _ = same64b.c.Write(proxyV2(net.ParseIP("2001:db8:7::17"), net.ParseIP("2001:db8::1"), 40002, 993))
	if closed, got := same64b.closed(3 * time.Second); !closed || !strings.HasPrefix(got, "* BYE") {
		t.Fatalf("third connection from one IPv6 /64: closed=%v %q", closed, got)
	}

	// the operator can see where the headers come from (the proxy)
	if n := e.metric("proxyPeers", "127.0.0.0/16"); n < 5 {
		t.Fatalf("proxyPeers = %v", e.metrics()["proxyPeers"])
	}

	// SMTP needs the header as well
	s := dialRaw(t, e.b.SMTPAddr())
	s.send("EHLO test.example.test\r\n")
	if closed, got := s.closed(3 * time.Second); !closed || got != "" {
		t.Fatalf("SMTP without header: closed=%v %q", closed, got)
	}
	s2 := dialRaw(t, e.b.SMTPAddr())
	s2.send("PROXY TCP4 203.0.113.50 10.0.0.9 51000 465\r\n")
	s2.until("220 ")
}

func TestProxyTrustedPeers(t *testing.T) {
	e := newEnv(t, set("PROXY_PROTOCOL", "required"), set("PROXY_TRUSTED_CIDRS", "10.9.0.0/16"))
	r := dialRaw(t, e.b.IMAPAddr())
	r.send("PROXY TCP4 203.0.113.7 10.0.0.9 51234 993\r\n")
	if closed, got := r.closed(3 * time.Second); !closed || got != "" {
		t.Fatalf("header from an untrusted peer was accepted: closed=%v %q", closed, got)
	}
	e2 := newEnv(t, set("PROXY_PROTOCOL", "required"), set("PROXY_TRUSTED_CIDRS", "10.9.0.0/16, 127.0.0.0/8"))
	r2 := dialRaw(t, e2.b.IMAPAddr())
	r2.send("PROXY TCP4 203.0.113.7 10.0.0.9 51234 993\r\n")
	r2.until("* OK")
}

// ---- after login ---------------------------------------------------------------

func rawLogin(t *testing.T, e *env, address string) *rawConn {
	t.Helper()
	r := dialRaw(t, e.b.IMAPAddr())
	r.until("* OK")
	r.send("a LOGIN " + address + " " + e.pw[address] + "\r\n")
	r.until("a OK")
	return r
}

func TestLimitsAfterLogin(t *testing.T) {
	e := newEnv(t, set("IMAP_MAX_LINE_BYTES", "4096"), set("IMAP_MAX_SET_RANGES", "16"), set("APPEND_MAX_BYTES", "100000"))
	e.mailbox(mara, 6, 1)
	before := e.stub.State(mara, edgestub.Inbox).HighestModseq

	// overlong command: closed
	r := rawLogin(t, e, mara)
	r.send("b NOOP " + strings.Repeat("x", 6000) + "\r\n")
	if closed, _ := r.closed(3 * time.Second); !closed {
		t.Fatal("an overlong command keeps the connection open")
	}

	// a non-synchronizing literal beyond what LITERAL- allows: closed at
	// once, before a single byte of it is read as anything
	r = rawLogin(t, e, mara)
	r.send("b APPEND Sent {5000+}\r\n")
	if closed, got := r.closed(3 * time.Second); !closed || strings.Contains(got, "b ") {
		t.Fatalf("big non-synchronizing literal: closed=%v %q", closed, got)
	}

	// The classic smuggling attempt: a literal the server refuses without
	// reading it, with commands inside. The connection closes; the inner
	// lines are never run.
	r = rawLogin(t, e, mara)
	r.send("b SELECT INBOX\r\n")
	r.until("b OK")
	inner := "x1 STORE 1:* +FLAGS (\\Deleted)\r\nx2 EXPUNGE\r\n"
	r.send(fmt.Sprintf("c NOOP {%d+}\r\n%s", len(inner), inner))
	closed, got := r.closed(3 * time.Second)
	if !closed || strings.Contains(got, "x1 ") || strings.Contains(got, "x2 ") {
		t.Fatalf("refused literal: closed=%v %q", closed, got)
	}
	if after := e.stub.State(mara, edgestub.Inbox).HighestModseq; after != before {
		t.Fatal("commands inside a refused literal were run")
	}

	// Padding the announcement with zeros does not get it past the guard.
	r = rawLogin(t, e, mara)
	r.send("b SELECT INBOX\r\n")
	r.until("b OK")
	r.send(fmt.Sprintf("c NOOP {%s%d+}\r\n%s", strings.Repeat("0", 60), len(inner), inner))
	closed, got = r.closed(3 * time.Second)
	if !closed || strings.Contains(got, "x1 ") || strings.Contains(got, "x2 ") {
		t.Fatalf("refused literal with padded size: closed=%v %q", closed, got)
	}
	if after := e.stub.State(mara, edgestub.Inbox).HighestModseq; after != before {
		t.Fatal("commands inside a refused literal were run")
	}

	// An announcement hidden in a quoted string that runs across a line
	// end, to make the guard skip the real one behind it: closed.
	r = rawLogin(t, e, mara)
	r.send("b SELECT INBOX\r\n")
	r.until("b OK")
	r.send("c APPEND \"{64+}\r\n\" {4097+}\r\n" + inner + strings.Repeat("x", 4097))
	closed, got = r.closed(3 * time.Second)
	if !closed || strings.Contains(got, "x1 ") || strings.Contains(got, "x2 ") {
		t.Fatalf("quoted string across lines: closed=%v %q", closed, got)
	}
	if after := e.stub.State(mara, edgestub.Inbox).HighestModseq; after != before {
		t.Fatal("commands behind a quoted string across lines were run")
	}
	// Quotes and braces inside one line are ordinary text.
	r = rawLogin(t, e, mara)
	r.send("b SELECT INBOX\r\n")
	r.until("b OK")
	r.send("c UID SEARCH KEYWORD \"a{5}\\\"b\"\r\n")
	r.until("c ")
	r.send("d NOOP\r\n")
	r.until("d OK")

	// The same content inside a literal the server does read (APPEND of an
	// unknown mail): refused as a mail, and still nothing is run.
	r = rawLogin(t, e, mara)
	r.send("b SELECT INBOX\r\n")
	r.until("b OK")
	r.send(fmt.Sprintf("c APPEND Sent {%d+}\r\n%s\r\n", len(inner), inner))
	if lines := r.until("c "); !strings.HasPrefix(lines[len(lines)-1], "c NO [CANNOT]") {
		t.Fatalf("APPEND: %q", lines)
	}
	r.send("d NOOP\r\n")
	if lines := r.until("d "); len(lines) != 1 || !strings.HasPrefix(lines[0], "d OK") {
		t.Fatalf("after the APPEND: %q", lines)
	}

	// A synchronizing literal beyond the APPEND limit: NO [TOOBIG] without
	// asking for the data, and the connection stays in step.
	r.send("e APPEND Sent {99999999}\r\n")
	if lines := r.until("e "); !strings.HasPrefix(lines[len(lines)-1], "e NO [TOOBIG]") {
		t.Fatalf("oversized APPEND: %q", lines)
	}
	r.send("f NOOP\r\n")
	r.until("f OK")

	// Pipelined: commands and a synchronizing literal sent in one go,
	// without waiting. Everything is answered in order.
	mail := "Message-ID: <never-sent@outside.test>\r\n\r\nx\r\n"
	r.send(fmt.Sprintf("g FETCH 1:3 (FLAGS)\r\nh APPEND Sent {%d}\r\n%s\r\ni UID SEARCH ALL\r\n", len(mail), mail))
	r.until("g OK")
	if lines := r.until("h "); !strings.HasPrefix(lines[len(lines)-1], "h NO [CANNOT]") {
		t.Fatalf("pipelined APPEND: %q", lines)
	}
	if lines := r.until("i OK"); !strings.HasPrefix(lines[0], "* SEARCH 1 2 3 4 5 6") {
		t.Fatalf("pipelined SEARCH: %q", lines)
	}

	// The same with a non-synchronizing literal behind another command: the
	// answer to the first command must not be mistaken for a refusal of the
	// literal.
	r.send(fmt.Sprintf("p1 NOOP\r\np2 APPEND Sent {%d+}\r\n%s\r\np3 NOOP\r\n", len(mail), mail))
	r.until("p1 OK")
	if lines := r.until("p2 "); !strings.HasPrefix(lines[len(lines)-1], "p2 NO [CANNOT]") {
		t.Fatalf("pipelined APPEND with a non-synchronizing literal: %q", lines)
	}
	r.until("p3 OK")

	// too many ranges, a body section nested too deep, an absurd partial
	var set []string
	for i := 1; i <= 40; i += 2 {
		set = append(set, fmt.Sprint(i))
	}
	r.send("j UID FETCH " + strings.Join(set, ",") + " (FLAGS)\r\n")
	if lines := r.until("j "); !strings.HasPrefix(lines[len(lines)-1], "j NO [LIMIT]") {
		t.Fatalf("many ranges: %q", lines)
	}
	r.send("k FETCH 1 (BODY.PEEK[" + strings.Repeat("1.", 40) + "1])\r\n")
	if lines := r.until("k "); !strings.HasPrefix(lines[len(lines)-1], "k BAD") {
		t.Fatalf("deep section: %q", lines)
	}
	r.send("l FETCH 1 (BODY.PEEK[]<5.9223372036854775807>)\r\n")
	if lines := r.until("l "); !strings.HasPrefix(lines[len(lines)-1], "l OK") {
		t.Fatalf("huge partial: %q", lines)
	}
	r.send("m FETCH 1 (BINARY.PEEK[1])\r\n")
	if lines := r.until("m "); !strings.HasPrefix(lines[len(lines)-1], "m NO [CANNOT]") {
		t.Fatalf("BINARY: %q", lines)
	}
	r.send("n STATUS INBOX (MESSAGES SIZE DELETED)\r\n")
	if lines := r.until("n "); !strings.HasPrefix(lines[len(lines)-1], "n NO") {
		t.Fatalf("STATUS SIZE: %q", lines)
	}
	r.send("o NOOP\r\n")
	r.until("o OK")
	if n := e.metric("imap", "panics"); n != 0 {
		t.Fatalf("panics = %d", n)
	}
	if n := e.metric("imap", "inputViolations"); n != 5 {
		t.Fatalf("inputViolations = %d", n)
	}
}

// Mail from strangers must not be able to hurt the server that shows it.
func TestHostileMailContent(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 1, 0)
	now := time.Now()
	deliver := func(raw string) uint32 {
		return e.stub.Deliver(mara, edgestub.Inbox, []byte(raw), now)
	}
	// 3000 nested multiparts
	var deep strings.Builder
	deep.WriteString("From: x@outside.test\r\nSubject: deep\r\n")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&deep, "Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
	}
	deep.WriteString("Content-Type: text/plain\r\n\r\nbottom\r\n")
	// 300 nested message/rfc822
	var nested strings.Builder
	nested.WriteString("From: x@outside.test\r\nSubject: nested\r\n")
	for i := 0; i < 300; i++ {
		nested.WriteString("Content-Type: message/rfc822\r\n\r\nSubject: inside\r\n")
	}
	nested.WriteString("Content-Type: text/plain\r\n\r\ndeep inside\r\n")
	uids := []uint32{
		deliver(deep.String()),
		deliver(nested.String()),
		deliver("From: x@outside.test\r\nContent-Type: multipart/mixed; boundary=empty\r\n\r\nnot a single part\r\n"),
		deliver("From: x@outside.test\r\nContent-Type: multipart/mixed\r\n\r\nwithout boundary\r\n"),
		deliver("Subject: " + strings.Repeat("A", 300000) + "\r\n\r\nhuge header line\r\n"),
		deliver(" starts with a space\r\nFrom: x@outside.test\r\n\r\nText\r\n"),
		deliver("just one line, nothing else"),
		deliver(""),
		deliver("From: =?utf-8?B?" + strings.Repeat("/", 5000) + "?= <a@b.test>\r\nTo: " + strings.Repeat("(", 5000) + "\r\nDate: never\r\n\r\nx\r\n"),
	}
	c := e.login(mara, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	st := e.stub.State(mara, edgestub.Inbox)
	msgs, err := c.Fetch(all(), &imap.FetchOptions{
		UID: true, Envelope: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection: []*imap.FetchItemBodySection{
			{Peek: true},
			{Peek: true, Part: []int{1}},
			{Peek: true, Part: []int{1, 1, 1, 1, 1, 1, 1, 1}},
			{Peek: true, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject"}},
			{Peek: true, Part: []int{1}, Specifier: imap.PartSpecifierMIME},
		},
	}).Collect()
	if err != nil {
		t.Fatalf("fetching hostile mail: %v", err)
	}
	if len(msgs) != len(st.Rows) {
		t.Fatalf("%d answers for %d mails", len(msgs), len(st.Rows))
	}
	for i, m := range msgs {
		if sha(bodySection(m, 0)) != st.Rows[i].Digest {
			t.Fatalf("uid %d: BODY[] is not the raw mail", m.UID)
		}
		if m.BodyStructure == nil {
			t.Fatalf("uid %d: no structure", m.UID)
		}
	}
	// the multipart without parts is shown as one part and its body is there
	for _, m := range msgs {
		if uint32(m.UID) == uids[2] {
			if _, ok := m.BodyStructure.(*imap.BodyStructureSinglePart); !ok {
				t.Fatalf("empty multipart: %#v", m.BodyStructure)
			}
		}
	}
	if n := e.metric("panics"); n != 0 {
		t.Fatalf("panics = %d", n)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
}

// ---- logs ------------------------------------------------------------------------

func TestLogsCarryNoSecrets(t *testing.T) {
	e := newEnv(t, withPoller())
	pw := e.mailbox(mara, 4, 1)
	e.mailbox(jonas, 2, 0)
	wrong := "mb_this_password_is_wrong"

	// a bit of everything
	bad := e.dial(nil)
	_ = bad.Login(mara, wrong).Wait()
	_ = bad.Authenticate(sasl.NewPlainClient(jonas, mara, pw))
	c := e.login(mara, nil)
	_, _ = c.Select("INBOX", nil).Wait()
	_, _ = c.Fetch(all(), &imap.FetchOptions{Envelope: true, BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	_ = c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close()
	_, _ = c.UIDSearch(&imap.SearchCriteria{Text: []string{"Secretword-in-the-search"}}, nil).Wait()
	_, _ = appendMail(c, "Sent", edgestub.BuildMail(edgestub.MailSpec{From: mara, To: []string{"x@outside.test"}, Subject: "Subject-in-append", Text: "x"}))
	_, _ = c.Select("Nonexistent-folder-name", nil).Wait()
	sc := e.smtpDial()
	_ = sc.Auth(sasl.NewPlainClient("", mara, wrong))
	sc = e.smtpLogin(mara)
	to := []string{"addressee@outside.test"}
	_ = sc.SendMail(mara, to, bytes.NewReader(outgoing(mara, to, "Subject-in-submit")))
	_ = sc.Mail(jonas, nil)
	raw := dialRaw(t, e.b.IMAPAddr())
	raw.until("* OK")
	raw.send("a LOGIN " + mara + " " + strings.Repeat("Z", 3000) + "\r\n")
	raw.closed(2 * time.Second)
	raw2 := rawLogin(t, e, jonas)
	raw2.send("b SEARCH SINCE no-date-" + wrong + "\r\n")
	raw2.until("b ")
	raw2.send("c UNKNOWN " + wrong + "\r\n")
	raw2.until("c ")
	e.stub.SetFault("/folders", edgestub.Fault{Status: 500, Body: `{"error":"x","message":"` + mara + `"}`, Count: 1})
	_, _ = c.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
	e.stub.RevokePassword(mara, pw)
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("revoked connection stays open")
	}

	logs := e.logs.String()
	if strings.Count(logs, "\n") < 8 {
		t.Fatalf("hardly any log lines:\n%s", logs)
	}
	forbidden := map[string]string{
		"device password":       pw,
		"second password":       e.pw[jonas],
		"wrong password":        wrong,
		"shared secret":         e.secret,
		"address":               mara,
		"other address":         jonas,
		"local part":            "mara@",
		"domain":                "mailbox-one",
		"recipient":             "addressee",
		"subject of sent mail":  "Subject-in-submit",
		"subject of append":     "Subject-in-append",
		"subject in the folder": "Received mara",
		"mail text":             "Content of mail",
		"search word":           "Secretword",
		"folder name":           "Nonexistent",
		"client address":        "127.0.0.1",
		"long password":         "ZZZZZZ",
	}
	for _, tok := range e.stub.SessionTokens() {
		forbidden["session "+tok[:8]] = tok
		forbidden["session part "+tok[:8]] = tok[4:20]
	}
	for what, s := range forbidden {
		if strings.Contains(logs, s) {
			t.Errorf("the log contains the %s", what)
		}
	}
	// every line is a small JSON object with time and event, and mailboxes
	// appear as their hash only; the mailbox identifier itself never
	hash := e.mbHash(mara)
	if id := e.stub.MailboxID(mara); strings.Contains(logs, id) {
		t.Error("the log contains the mailbox identifier")
	}
	sawHash := false
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if obj["t"] == nil || obj["ev"] == nil {
			t.Fatalf("log line without time or event: %q", line)
		}
		for k, v := range obj {
			if s, ok := v.(string); ok && len(s) > 64 {
				t.Fatalf("log field %s is long: %q", k, line)
			}
		}
		if obj["mb"] == hash {
			sawHash = true
		}
	}
	if !sawHash {
		t.Fatal("no log line carries the mailbox hash")
	}
}

// ---- operations ------------------------------------------------------------------

func TestHealthAndMetrics(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 3, 0)
	c := e.login(mara, nil)
	_, _ = c.Select("INBOX", nil).Wait()
	_, _ = c.Fetch(all(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()

	resp, err := http.Get("http://" + e.b.AdminAddr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok\n" {
		t.Fatalf("/healthz = %d %q", resp.StatusCode, body)
	}
	hash := e.mbHash(mara)
	for _, path := range [][]string{
		{"imap", "connsOpen"}, {"imap", "loginsOk"},
		{"edge", "/login", "count"}, {"edge", "/entries", "count"}, {"edge", "/raw", "count"}, {"edge", "/raw", "bytesIn"},
		{"mirror", "fullLoads"}, {"mirror", "rows"}, {"rawCache", "bytes"},
		{"mailboxes", hash, "fullLoads"}, {"mailboxes", hash, "edgeCalls"}, {"mailboxes", hash, "rawBytes"},
	} {
		if e.metric(path...) <= 0 {
			t.Errorf("metric %v is not counted", path)
		}
	}
	raw, _ := json.Marshal(e.metrics())
	if strings.Contains(string(raw), "mailbox-one") || strings.Contains(string(raw), "mara") || strings.Contains(string(raw), e.stub.MailboxID(mara)) {
		t.Fatal("/metrics names a mailbox")
	}
}

func TestShutdownSaysGoodbye(t *testing.T) {
	e := newEnv(t)
	e.mailbox(mara, 2, 0)
	in := rawLogin(t, e, mara)
	in.send("b SELECT INBOX\r\n")
	in.until("b OK")
	in.send("c IDLE\r\n")
	in.until("+ ")
	pre := dialRaw(t, e.b.IMAPAddr())
	pre.until("* OK")
	sm := dialRaw(t, e.b.SMTPAddr())
	sm.until("220 ")
	sm.send("EHLO test.example.test\r\n")
	sm.until("250 ")
	imapAddr := e.b.IMAPAddr()

	done := make(chan struct{})
	go func() { e.b.Shutdown(); close(done) }()
	for name, r := range map[string]*rawConn{"idling": in, "not logged in": pre} {
		closed, got := r.closed(5 * time.Second)
		if !closed || !strings.Contains(got, "* BYE") {
			t.Fatalf("%s IMAP connection: closed=%v %q", name, closed, got)
		}
	}
	if closed, got := sm.closed(5 * time.Second); !closed || !strings.Contains(got, "421 ") {
		t.Fatalf("SMTP connection: closed=%v %q", closed, got)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown does not return")
	}
	if c, err := net.DialTimeout("tcp", imapAddr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("new connections are still accepted")
	}
}

// A PROXY header is believed from any peer as long as PROXY_TRUSTED_CIDRS is
// empty. The bridge says so on every start, and only then.
func TestStartWarnsAboutUntrustedProxyPeers(t *testing.T) {
	const warning = `"code":"proxy_peers_untrusted"`
	open := newEnv(t, set("PROXY_PROTOCOL", "required"))
	if !strings.Contains(open.logs.String(), warning) {
		t.Errorf("required without trusted networks: no warning in %q", open.logs.String())
	}
	pinned := newEnv(t, set("PROXY_PROTOCOL", "required"), set("PROXY_TRUSTED_CIDRS", "127.0.0.0/8"))
	if strings.Contains(pinned.logs.String(), warning) {
		t.Errorf("trusted networks set: unexpected warning in %q", pinned.logs.String())
	}
	off := newEnv(t)
	if strings.Contains(off.logs.String(), warning) {
		t.Errorf("PROXY protocol off: unexpected warning in %q", off.logs.String())
	}
}
