package bridge_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/bridge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/config"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
)

// Invented mailboxes. Nothing here is a real address.
const (
	mara  = "mara@mailbox-one.test"
	jonas = "jonas@mailbox-two.test"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t      *testing.T
	stub   *edgestub.Stub
	srv    *httptest.Server
	b      *bridge.Bridge
	logs   *syncBuffer
	secret string
	pw     map[string]string
	// faultsExpected: this test makes the bridge send, or the edge answer,
	// something outside the contract on purpose.
	faultsExpected bool
}

type envOpt func(env map[string]string)

func set(key, value string) envOpt { return func(e map[string]string) { e[key] = value } }

// newEnv starts a test edge and a bridge in this process. The poller is
// off: tests call sync() when they want the bridge to look for changes,
// unless they pass withPoller.
func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	return newEnvWithHook(t, nil, opts...)
}

// newEnvWithHook is newEnv with a function that runs at the start of every
// IMAP FETCH.
func newEnvWithHook(t *testing.T, hook func(), opts ...envOpt) *env {
	t.Helper()
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, secret: "sec_" + hex.EncodeToString(buf), logs: &syncBuffer{}, pw: map[string]string{}}
	e.stub = edgestub.New(e.secret)
	e.srv = httptest.NewServer(e.stub)
	vars := map[string]string{
		"CONVEX_SITE_URL":        e.srv.URL,
		"MAIL_BRIDGE_SECRET":     e.secret,
		"IMAP_ADDR":              "127.0.0.1:0",
		"SMTP_ADDR":              "127.0.0.1:0",
		"ADMIN_ADDR":             "127.0.0.1:0",
		"PROXY_PROTOCOL":         "off",
		"SEQ_INTERVAL_MS":        "100",
		"IDLE_KEEPALIVE_SECONDS": "0",
		"SHUTDOWN_GRACE_SECONDS": "3",
		"IMAP_MAX_CONNS_PER_IP":  "200",
		"SMTP_MAX_CONNS_PER_IP":  "200",
		"test.poller":            "off",
	}
	for _, o := range opts {
		o(vars)
	}
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	if ms := vars["test.keepalive_ms"]; ms != "" {
		d, _ := time.ParseDuration(ms + "ms")
		cfg.IdleKeepalive = d
	}
	if ms := vars["test.preauth_ms"]; ms != "" {
		d, _ := time.ParseDuration(ms + "ms")
		cfg.PreauthTimeout = d
	}
	if ms := vars["test.smtp_read_ms"]; ms != "" {
		d, _ := time.ParseDuration(ms + "ms")
		cfg.SMTPReadTimeout = d
	}
	if ms := vars["test.fresh_ms"]; ms != "" {
		d, _ := time.ParseDuration(ms + "ms")
		cfg.Freshness = d
	}
	e.b, err = bridge.Start(cfg, bridge.Options{LogWriter: e.logs, NoPoller: vars["test.poller"] != "on", FetchHook: hook})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Every test is a test of the contract as well: the edge turns down
		// any body with a field it does not know or of more than 64 KiB, and
		// the bridge must not have produced a call it could not send.
		if !e.faultsExpected {
			if n := e.stub.Refused(); n != 0 {
				t.Errorf("the edge refused %d requests of the bridge as malformed or too large", n)
			}
			if n := e.b.M.EdgeOversize.Load() + e.b.M.EdgeNotSent.Load(); n != 0 {
				t.Errorf("the bridge built %d calls it could not send", n)
			}
		}
		e.b.Shutdown()
		e.srv.Close()
	})
	return e
}

func withPoller() envOpt { return set("test.poller", "on") }

// mailbox creates a mailbox with a device password and n mails in INBOX
// and m in Sent.
func (e *env) mailbox(address string, inbox, sent int) string {
	e.t.Helper()
	e.stub.AddMailbox(address)
	pw := e.stub.AddPassword(address)
	e.pw[address] = pw
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	local := strings.SplitN(address, "@", 2)[0]
	for i := 0; i < inbox; i++ {
		raw := edgestub.BuildMail(edgestub.MailSpec{
			From: fmt.Sprintf("Sender %d <sender%d@outside.test>", i, i), To: []string{address},
			Subject: fmt.Sprintf("Received %s no. %d", local, i+1),
			Date:    base.Add(time.Duration(i) * 6 * time.Hour),
			Text:    fmt.Sprintf("Content of mail %d to %s.\nSecond line.", i+1, local),
		})
		var flags []string
		if i%3 == 0 {
			flags = append(flags, `\Seen`)
		}
		e.stub.Deliver(address, edgestub.Inbox, raw, base.Add(time.Duration(i)*6*time.Hour), flags...)
	}
	for i := 0; i < sent; i++ {
		raw := edgestub.BuildMail(edgestub.MailSpec{
			From: address, To: []string{fmt.Sprintf("customer%d@outside.test", i)},
			Subject:   fmt.Sprintf("Sent %s no. %d", local, i+1),
			Date:      base.Add(time.Duration(i) * 9 * time.Hour),
			MessageID: fmt.Sprintf("sent-%s-%d@mail.mailbox.test", local, i+1),
			Text:      fmt.Sprintf("Reply %d from %s.", i+1, local),
		})
		e.stub.Deliver(address, edgestub.Sent, raw, base.Add(time.Duration(i)*9*time.Hour), `\Seen`)
	}
	return pw
}

// mbHash is the hash under which the bridge counts and logs a mailbox: of
// the identifier the edge hands out, not of the address.
func (e *env) mbHash(address string) string {
	return logx.HashMailbox(e.stub.MailboxID(address))
}

// sync lets the bridge look for changes once and waits until it is done.
func (e *env) sync() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	e.b.SyncNow(ctx)
}

func (e *env) dial(opts *imapclient.Options) *imapclient.Client {
	e.t.Helper()
	c, err := imapclient.DialInsecure(e.b.IMAPAddr(), opts)
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	e.t.Cleanup(func() { _ = c.Close() })
	return c
}

// login returns a logged-in client for a mailbox created with mailbox().
func (e *env) login(address string, opts *imapclient.Options) *imapclient.Client {
	e.t.Helper()
	c := e.dial(opts)
	if err := c.Login(address, e.pw[address]).Wait(); err != nil {
		e.t.Fatalf("login %s: %v", address, err)
	}
	return c
}

func (e *env) metrics() map[string]any {
	e.t.Helper()
	resp, err := http.Get("http://" + e.b.AdminAddr() + "/metrics")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// metric walks the metrics tree, e.g. metric("imap", "loginsOk").
func (e *env) metric(path ...string) int64 {
	e.t.Helper()
	var cur any = e.metrics()
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = m[p]
	}
	f, _ := cur.(float64)
	return int64(f)
}

// imapCode returns the response code of an IMAP error ("" if none).
func imapCode(err error) string {
	if e, ok := err.(*imap.Error); ok {
		return string(e.Code)
	}
	return ""
}

func isNo(err error) bool {
	e, ok := err.(*imap.Error)
	return ok && e.Type == imap.StatusResponseTypeNo
}

func wantNo(t *testing.T, what string, err error, code string) {
	t.Helper()
	if !isNo(err) || imapCode(err) != code {
		t.Fatalf("%s: want NO [%s], got %v", what, code, err)
	}
}

// rawConn is a plain TCP client for the tests that speak the protocol by
// hand.
type rawConn struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawConn{t: t, c: c, r: bufio.NewReader(c)}
}

func (r *rawConn) send(s string) {
	r.t.Helper()
	_ = r.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(r.c, s); err != nil {
		r.t.Fatalf("write: %v", err)
	}
}

// line reads one line (without CRLF); "" and an error at end of stream.
func (r *rawConn) line() (string, error) {
	_ = r.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	l, err := r.r.ReadString('\n')
	return strings.TrimRight(l, "\r\n"), err
}

// until reads lines until one starts with prefix and returns all of them.
func (r *rawConn) until(prefix string) []string {
	r.t.Helper()
	var lines []string
	for {
		l, err := r.line()
		if err != nil {
			r.t.Fatalf("waiting for %q: %v (got %q)", prefix, err, lines)
		}
		lines = append(lines, l)
		if strings.HasPrefix(l, prefix) {
			return lines
		}
	}
}

// closed reports whether the server closes the connection within d,
// returning what it sent before.
func (r *rawConn) closed(d time.Duration) (bool, string) {
	var got strings.Builder
	deadline := time.Now().Add(d)
	buf := make([]byte, 4096)
	for {
		_ = r.c.SetReadDeadline(deadline)
		n, err := r.r.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return false, got.String()
			}
			return true, got.String()
		}
	}
}

func eventually(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", d, what)
}
