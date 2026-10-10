// Command edgeprobe checks a running bridge over IMAP. test/against-edge.sh
// starts the bridge against a given edge (a local Convex with the real
// code) and runs this program next to it.
//
// It assumes nothing about the edge beyond the contract
// (docs/edge-contract.md): what a mailbox holds is
// read through IMAP and compared with itself, with the counters of the
// edge (STATUS) and with the numbers given from outside (EXPECT_INBOX,
// EXPECT_SENT). It prints what it checks, one line per check, and ends
// with status 1 if any check failed.
//
// It never prints an address, a password, a subject or a mail. Mails are
// named by folder and number only.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type settings struct {
	imapAddr   string
	metricsURL string

	address, password             string
	secondAddress, secondPassword string

	expectInbox, expectSent int // -1: no expectation given
	maxMails                int
	allowExpunge            bool
	expungeUID              uint32
	skipIdle                bool
	idleWait, idleGrace     time.Duration
	triggerHint             string
}

func envInt(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s is not a number from 0", name)
	}
	return n, nil
}

func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "ja", "on":
		return true
	}
	return false
}

func load() (*settings, error) {
	s := &settings{
		imapAddr:       os.Getenv("PROBE_IMAP_ADDR"),
		metricsURL:     os.Getenv("PROBE_METRICS_URL"),
		address:        os.Getenv("MAILBOX_ADDRESS"),
		password:       os.Getenv("DEVICE_PASSWORD"),
		secondAddress:  os.Getenv("SECOND_ADDRESS"),
		secondPassword: os.Getenv("SECOND_PASSWORD"),
		allowExpunge:   envOn("ALLOW_EXPUNGE"),
		skipIdle:       envOn("SKIP_IDLE"),
		triggerHint:    os.Getenv("PROBE_TRIGGER_HINT"),
	}
	if s.imapAddr == "" {
		s.imapAddr = "127.0.0.1:1143"
	}
	if s.metricsURL == "" {
		s.metricsURL = "http://127.0.0.1:8080/metrics"
	}
	if s.address == "" || s.password == "" {
		return nil, errors.New("MAILBOX_ADDRESS and DEVICE_PASSWORD must be set")
	}
	if (s.secondAddress == "") != (s.secondPassword == "") {
		return nil, errors.New("SECOND_ADDRESS and SECOND_PASSWORD only together")
	}
	var err error
	if s.expectInbox, err = envInt("EXPECT_INBOX", -1); err != nil {
		return nil, err
	}
	if s.expectSent, err = envInt("EXPECT_SENT", -1); err != nil {
		return nil, err
	}
	if s.maxMails, err = envInt("MAX_MAILS", 200); err != nil {
		return nil, err
	}
	uid, err := envInt("EXPUNGE_UID", 0)
	if err != nil {
		return nil, err
	}
	s.expungeUID = uint32(uid)
	wait, err := envInt("IDLE_WAIT_SECONDS", 900)
	if err != nil {
		return nil, err
	}
	grace, err := envInt("IDLE_GRACE_SECONDS", 30)
	if err != nil {
		return nil, err
	}
	s.idleWait, s.idleGrace = time.Duration(wait)*time.Second, time.Duration(grace)*time.Second
	return s, nil
}

// ---- output --------------------------------------------------------------

type probe struct {
	*settings
	start                   time.Time
	passed, failed, skipped int
	signal                  chan time.Time // a line on standard input: "the mail is in"
	conns                   []*conn
}

func (p *probe) section(title string) { fmt.Printf("\n== %s ==\n", title) }

func (p *probe) ok(format string, a ...any) {
	p.passed++
	fmt.Printf("ok     %s\n", fmt.Sprintf(format, a...))
}

func (p *probe) fail(format string, a ...any) {
	p.failed++
	fmt.Printf("FAIL   %s\n", fmt.Sprintf(format, a...))
}

func (p *probe) skip(format string, a ...any) {
	p.skipped++
	fmt.Printf("skip   %s\n", fmt.Sprintf(format, a...))
}

func (p *probe) info(format string, a ...any) { fmt.Printf("       %s\n", fmt.Sprintf(format, a...)) }

// check is ok or fail by a condition.
func (p *probe) check(cond bool, format string, a ...any) bool {
	if cond {
		p.ok(format, a...)
	} else {
		p.fail(format, a...)
	}
	return cond
}

func secs(d time.Duration) string {
	return fmt.Sprintf("%.3f s", d.Seconds())
}

// describe turns an error into a short text. IMAP answers of the bridge
// carry a response code and a fixed text, never an address.
func describe(err error) string {
	if err == nil {
		return "no error"
	}
	var ie *imap.Error
	if errors.As(err, &ie) {
		if ie.Code != "" {
			return fmt.Sprintf("%s [%s] %s", ie.Type, ie.Code, ie.Text)
		}
		return fmt.Sprintf("%s %s", ie.Type, ie.Text)
	}
	return err.Error()
}

func codeOf(err error) string {
	var ie *imap.Error
	if errors.As(err, &ie) {
		return string(ie.Code)
	}
	return ""
}

// ---- connections ---------------------------------------------------------

type existsEvent struct {
	n  uint32
	at time.Time
}

// conn is one IMAP connection with what the server sent on its own.
type conn struct {
	c *imapclient.Client

	mu       sync.Mutex
	exists   []existsEvent
	expunged []uint32
	pushed   map[imap.UID][]imap.Flag // flags from unsolicited FETCH, by uid
	pushes   int
	news     chan struct{}
}

func (p *probe) dial() (*conn, error) {
	cn := &conn{pushed: map[imap.UID][]imap.Flag{}, news: make(chan struct{}, 1)}
	wake := func() {
		select {
		case cn.news <- struct{}{}:
		default:
		}
	}
	opts := &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil {
				cn.mu.Lock()
				cn.exists = append(cn.exists, existsEvent{n: *d.NumMessages, at: time.Now()})
				cn.mu.Unlock()
				wake()
			}
		},
		Expunge: func(seq uint32) {
			cn.mu.Lock()
			cn.expunged = append(cn.expunged, seq)
			cn.mu.Unlock()
			wake()
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			buf, err := msg.Collect()
			if err != nil {
				return
			}
			cn.mu.Lock()
			cn.pushes++
			if buf.UID != 0 {
				cn.pushed[buf.UID] = buf.Flags
			}
			cn.mu.Unlock()
			wake()
		},
	}}
	c, err := imapclient.DialInsecure(p.imapAddr, opts)
	if err != nil {
		return nil, err
	}
	if err := c.WaitGreeting(); err != nil {
		_ = c.Close()
		return nil, err
	}
	cn.c = c
	p.conns = append(p.conns, cn)
	return cn, nil
}

// login opens a connection and logs in.
func (p *probe) login(address, password string) (*conn, error) {
	cn, err := p.dial()
	if err != nil {
		return nil, err
	}
	if err := cn.c.Login(address, password).Wait(); err != nil {
		_ = cn.c.Close()
		return nil, err
	}
	return cn, nil
}

func (cn *conn) close() {
	if cn == nil || cn.c == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = cn.c.Logout().Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	_ = cn.c.Close()
}

func (cn *conn) reset() {
	cn.mu.Lock()
	cn.exists, cn.expunged, cn.pushes = nil, nil, 0
	cn.pushed = map[imap.UID][]imap.Flag{}
	cn.mu.Unlock()
}

func hasFlag(flags []imap.Flag, f imap.Flag) bool {
	for _, x := range flags {
		if strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}

// ---- reading a folder ------------------------------------------------------

type row struct {
	uid   imap.UID
	flags []imap.Flag
	size  int64
	date  time.Time
}

type listing struct {
	folder      string
	exists      uint32
	uidNext     imap.UID
	uidValidity uint32
	rows        []row
}

func (l *listing) find(uid imap.UID) *row {
	for i := range l.rows {
		if l.rows[i].uid == uid {
			return &l.rows[i]
		}
	}
	return nil
}

func (l *listing) unseen() int {
	n := 0
	for _, r := range l.rows {
		if !hasFlag(r.flags, imap.FlagSeen) {
			n++
		}
	}
	return n
}

var allUIDs = imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}

// list selects a folder and reads UID, FLAGS, RFC822.SIZE and INTERNALDATE
// of every mail.
func list(cn *conn, folder string) (*listing, error) {
	sel, err := cn.c.Select(folder, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("SELECT: %s", describe(err))
	}
	l := &listing{folder: folder, exists: sel.NumMessages, uidNext: sel.UIDNext, uidValidity: sel.UIDValidity}
	msgs, err := cn.c.Fetch(allUIDs, &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("UID FETCH 1:*: %s", describe(err))
	}
	for _, m := range msgs {
		l.rows = append(l.rows, row{uid: m.UID, flags: m.Flags, size: m.RFC822Size, date: m.InternalDate})
	}
	return l, nil
}

type status struct {
	messages, unseen uint32
	uidNext          imap.UID
	uidValidity      uint32
}

func statusOf(cn *conn, folder string) (*status, error) {
	d, err := cn.c.Status(folder, &imap.StatusOptions{NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true}).Wait()
	if err != nil {
		return nil, err
	}
	if d.NumMessages == nil || d.NumUnseen == nil {
		return nil, errors.New("STATUS without MESSAGES or UNSEEN")
	}
	return &status{messages: *d.NumMessages, unseen: *d.NumUnseen, uidNext: d.UIDNext, uidValidity: d.UIDValidity}, nil
}

// flagsOf asks for the flags of one mail; found is false if the folder has
// no mail with that number.
func flagsOf(cn *conn, uid imap.UID) (flags []imap.Flag, found bool, err error) {
	msgs, err := cn.c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		return nil, false, err
	}
	for _, m := range msgs {
		if m.UID == uid {
			return m.Flags, true, nil
		}
	}
	return nil, false, nil
}

// ---- metrics ---------------------------------------------------------------

type metrics map[string]any

func (p *probe) metrics() (metrics, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Get(p.metricsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/metrics answers %d", resp.StatusCode)
	}
	var m metrics
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func (m metrics) num(path ...string) int64 {
	var cur any = map[string]any(m)
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = mm[k]
	}
	f, _ := cur.(float64)
	return int64(f)
}

// calls returns how often a path of the edge was called.
func (m metrics) calls(path string) int64 { return m.num("edge", path, "count") }

// answered returns how often a path was answered with a status.
func (m metrics) answered(path string, status int) int64 {
	return m.num("edge", path, "status", strconv.Itoa(status))
}

// ---- the checks --------------------------------------------------------------

func main() {
	s, err := load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgeprobe:", err)
		os.Exit(2)
	}
	p := &probe{settings: s, start: time.Now(), signal: make(chan time.Time, 1)}
	go func() {
		// One line on standard input means: the mail for the IDLE check has
		// been delivered. against-edge.sh writes it when the trigger file
		// appears or Enter is pressed.
		r := bufio.NewReader(os.Stdin)
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
			select {
			case p.signal <- time.Now():
			default:
			}
		}
	}()
	p.run()
	for _, cn := range p.conns {
		_ = cn.c.Close()
	}
	fmt.Printf("\n== Result ==\n%d checks passed, %d failed, %d skipped, duration %s\n",
		p.passed, p.failed, p.skipped, secs(time.Since(p.start)))
	if p.failed > 0 {
		os.Exit(1)
	}
}

func (p *probe) run() {
	m0, err := p.metrics()
	if err != nil {
		p.fail("cannot read /metrics of the bridge: %v", err)
		m0 = metrics{}
	}

	// ---- login ---------------------------------------------------------
	p.section("Login")
	if cn, err := p.dial(); err != nil {
		p.fail("no connection to the bridge: %v", err)
		return
	} else {
		// A password nobody has: random, in the form of a device password.
		buf := make([]byte, 20)
		_, _ = rand.Read(buf)
		t := time.Now()
		err := cn.c.Login(p.address, "mb_"+hex.EncodeToString(buf)).Wait()
		switch {
		case err == nil:
			p.fail("an invented password logs in")
		case codeOf(err) == "AUTHENTICATIONFAILED":
			p.ok("wrong password is rejected: %s (%s)", describe(err), secs(time.Since(t)))
		default:
			p.fail("wrong password: expected NO [AUTHENTICATIONFAILED], got %s", describe(err))
			p.explainLogin()
		}
		cn.close()
	}
	t := time.Now()
	a, err := p.login(p.address, p.password)
	if err != nil {
		p.fail("correct password does not log in: %s", describe(err))
		p.explainLogin()
		return
	}
	p.ok("correct password logs in (%s)", secs(time.Since(t)))
	var caps []string
	for c := range a.c.Caps() {
		caps = append(caps, string(c))
	}
	sort.Strings(caps)
	p.info("Capabilities: %s", strings.Join(caps, " "))

	// ---- LIST and STATUS -------------------------------------------------
	p.section("Folders")
	boxes, err := a.c.List("", "*", nil).Collect()
	if err != nil {
		p.fail("LIST: %s", describe(err))
		return
	}
	var names []string
	sentSpecial := false
	seen := map[string]bool{}
	for _, b := range boxes {
		var attrs []string
		for _, at := range b.Attrs {
			attrs = append(attrs, string(at))
			if b.Mailbox == "Sent" && at == imap.MailboxAttrSent {
				sentSpecial = true
			}
		}
		names = append(names, fmt.Sprintf("%s (%s)", b.Mailbox, strings.Join(attrs, " ")))
		seen[b.Mailbox] = true
	}
	p.check(seen["INBOX"] && seen["Sent"] && len(boxes) == 2, "LIST names exactly INBOX and Sent: %s", strings.Join(names, ", "))
	p.check(sentSpecial, "Sent carries the role \\Sent")

	st := map[string]*status{}
	for _, f := range []string{"INBOX", "Sent"} {
		s, err := statusOf(a, f)
		if err != nil {
			p.fail("STATUS %s: %s", f, describe(err))
			return
		}
		st[f] = s
		p.info("STATUS %s: MESSAGES %d, UNSEEN %d, UIDNEXT %d, UIDVALIDITY %d", f, s.messages, s.unseen, s.uidNext, s.uidValidity)
	}

	// ---- SELECT, counts, listing ---------------------------------------------
	lists := map[string]*listing{}
	for _, f := range []string{"INBOX", "Sent"} {
		p.section("SELECT " + f)
		t := time.Now()
		l, err := list(a, f)
		if err != nil {
			p.fail("%s: %v", f, err)
			return
		}
		lists[f] = l
		p.ok("SELECT %s and UID FETCH 1:* (UID FLAGS RFC822.SIZE INTERNALDATE): EXISTS %d, UIDNEXT %d, UIDVALIDITY %d (%s)",
			f, l.exists, l.uidNext, l.uidValidity, secs(time.Since(t)))
		expect := p.expectInbox
		if f == "Sent" {
			expect = p.expectSent
		}
		if expect >= 0 {
			p.check(int(l.exists) == expect, "%s has %d messages, expected %d", f, l.exists, expect)
		} else {
			p.skip("no expectation set for %s (EXPECT_%s)", f, strings.ToUpper(f))
		}
		p.check(len(l.rows) == int(l.exists), "%s: UID FETCH 1:* returns %d rows at EXISTS %d", f, len(l.rows), l.exists)
		p.checkRows(l, f)
		// what SELECT shows (from the rows) against what STATUS says (the
		// counters of the edge)
		s := st[f]
		p.check(s.messages == l.exists && s.uidNext == l.uidNext && s.uidValidity == l.uidValidity,
			"%s: STATUS and SELECT agree (MESSAGES %d/%d, UIDNEXT %d/%d, UIDVALIDITY %d/%d)",
			f, s.messages, l.exists, s.uidNext, l.uidNext, s.uidValidity, l.uidValidity)
		p.check(int(s.unseen) == l.unseen(), "%s: UNSEEN per STATUS %d, without \\Seen in the listing %d", f, s.unseen, l.unseen())
	}

	// ---- every mail --------------------------------------------------------
	digests := map[string]map[imap.UID]string{}
	for _, f := range []string{"INBOX", "Sent"} {
		p.section("Messages in " + f)
		d, ok := p.readMails(a, lists[f])
		digests[f] = d
		if !ok {
			// the connection may be gone after a protocol error: a new one
			a.close()
			if a, err = p.login(p.address, p.password); err != nil {
				p.fail("repeated login: %s", describe(err))
				return
			}
		}
	}

	// ---- flags -----------------------------------------------------------------
	p.section("Flags in INBOX")
	if _, err := a.c.Select("INBOX", nil).Wait(); err != nil {
		p.fail("SELECT INBOX: %s", describe(err))
		return
	}
	b, err := p.login(p.address, p.password) // the second connection, watching
	if err != nil {
		p.fail("second connection: %s", describe(err))
		return
	}
	if _, err := b.c.Select("INBOX", nil).Wait(); err != nil {
		p.fail("second connection, SELECT INBOX: %s", describe(err))
		return
	}
	s3, err := p.login(p.address, p.password) // a third, for STATUS (the counters of the edge)
	if err != nil {
		p.fail("third connection: %s", describe(err))
		return
	}
	inbox := lists["INBOX"]
	if len(inbox.rows) == 0 {
		p.fail("INBOX is empty: STORE cannot be checked")
	} else {
		// \Seen on an unread mail and back (or, if all are read, off and on)
		target := inbox.rows[len(inbox.rows)-1]
		unread := false
		for i := len(inbox.rows) - 1; i >= 0; i-- {
			if !hasFlag(inbox.rows[i].flags, imap.FlagSeen) {
				target, unread = inbox.rows[i], true
				break
			}
		}
		if !unread {
			p.info("no unread message in INBOX: \\Seen is removed from a read one and set again")
		}
		first, second := imap.StoreFlagsAdd, imap.StoreFlagsDel
		if !unread {
			first, second = second, first
		}
		p.toggle(a, b, s3, target.uid, imap.FlagSeen, first, true)
		p.toggle(a, b, s3, target.uid, imap.FlagSeen, second, true)

		// \Flagged on and off (or off and on)
		first, second = imap.StoreFlagsAdd, imap.StoreFlagsDel
		if hasFlag(target.flags, imap.FlagFlagged) {
			first, second = second, first
		}
		p.toggle(a, b, s3, target.uid, imap.FlagFlagged, first, false)
		p.toggle(a, b, s3, target.uid, imap.FlagFlagged, second, false)

		// and nothing else moved: the listing is what it was
		if again, err := list(a, "INBOX"); err != nil {
			p.fail("INBOX after the flags: %v", err)
		} else {
			p.check(sameListing(inbox, again), "INBOX is as before after setting and reverting (UIDs, sizes, flags)")
		}
	}

	// ---- IDLE --------------------------------------------------------------------
	p.section("IDLE")
	var arrived imap.UID
	if p.skipIdle {
		p.skip("IDLE (SKIP_IDLE set)")
	} else {
		arrived = p.idle(a)
	}

	// ---- EXPUNGE -----------------------------------------------------------------
	p.section("EXPUNGE")
	if !p.allowExpunge {
		p.skip("EXPUNGE (only with ALLOW_EXPUNGE=1)")
	} else {
		p.expunge(a, b, s3, arrived)
	}
	b.close()
	s3.close()

	// ---- reconnect ---------------------------------------------------------------
	p.section("Reconnecting")
	p.reconnect(digests["INBOX"])

	// ---- second mailbox -------------------------------------------------------------
	p.section("Second mailbox")
	if p.secondAddress == "" {
		p.skip("Separation of mailboxes (SECOND_ADDRESS and SECOND_PASSWORD not set)")
	} else {
		p.separation(a, digests)
	}
	a.close()

	// ---- counters --------------------------------------------------------------------
	p.section("Bridge counters (/metrics)")
	p.counters(m0)
}

// explainLogin says, from the counters of the bridge, what the edge
// answered to /login so far.
func (p *probe) explainLogin() {
	m, err := p.metrics()
	if err != nil {
		return
	}
	e, _ := m["edge"].(map[string]any)
	l, _ := e["/login"].(map[string]any)
	st, _ := l["status"].(map[string]any)
	meaning := map[string]string{
		"none": "no answer: edge at EDGE_URL not reachable (hostname, port, from inside the container?)",
		"200":  "logged in",
		"400":  "the edge considers the request invalid",
		"401":  "rejected: address or password (then the bridge says AUTHENTICATIONFAILED) or the secret, which is wrong or shorter than 32 characters at the edge (then UNAVAILABLE)",
		"404":  "the edge does not know /bridge/v1/login (code not rolled out, wrong port?)",
		"429":  "throttled: too many failed attempts from this address",
		"500":  "internal error of the edge",
		"503":  "the mailbox is still being prepared",
	}
	var codes []string
	for c := range st {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		n, _ := st[c].(float64)
		text := meaning[c]
		if text == "" {
			text = "unexpected answer"
		}
		p.info("/login at the edge so far: %d times %s (%s)", int64(n), c, text)
	}
}

// checkRows checks the listing of a folder against itself.
func (p *probe) checkRows(l *listing, f string) {
	ascending, below, sized, dated := true, true, true, true
	var prev imap.UID
	var total int64
	for _, r := range l.rows {
		if r.uid <= prev {
			ascending = false
		}
		prev = r.uid
		if r.uid >= l.uidNext {
			below = false
		}
		if r.size <= 0 {
			sized = false
		}
		if r.date.IsZero() {
			dated = false
		}
		total += r.size
	}
	p.check(ascending, "%s: UIDs strictly ascending", f)
	p.check(below, "%s: all UIDs are below UIDNEXT %d", f, l.uidNext)
	p.check(sized, "%s: every message has a size above 0 (%d bytes together)", f, total)
	p.check(dated, "%s: every message has an internal date", f)
}

func sameListing(a, b *listing) bool {
	if a.exists != b.exists || a.uidNext != b.uidNext || a.uidValidity != b.uidValidity || len(a.rows) != len(b.rows) {
		return false
	}
	for i := range a.rows {
		x, y := a.rows[i], b.rows[i]
		if x.uid != y.uid || x.size != y.size || !x.date.Equal(y.date) || !sameFlags(x.flags, y.flags) {
			return false
		}
	}
	return true
}

func sameFlags(a, b []imap.Flag) bool {
	norm := func(in []imap.Flag) string {
		var l []string
		for _, f := range in {
			l = append(l, strings.ToLower(string(f)))
		}
		sort.Strings(l)
		return strings.Join(l, " ")
	}
	return norm(a) == norm(b)
}

// readMails fetches BODY.PEEK[], ENVELOPE and BODYSTRUCTURE of the mails of
// a folder (the newest MAX_MAILS), one command per mail so that a failure
// names its mail. It returns the digests by number and whether the
// connection is still usable.
func (p *probe) readMails(cn *conn, l *listing) (map[imap.UID]string, bool) {
	digests := map[imap.UID]string{}
	rows := l.rows
	if len(rows) == 0 {
		p.skip("%s is empty: no message to read", l.folder)
		return digests, true
	}
	if len(rows) > p.maxMails {
		p.info("%s has %d messages; the newest %d are read (MAX_MAILS)", l.folder, len(rows), p.maxMails)
		rows = rows[len(rows)-p.maxMails:]
	}
	if _, err := cn.c.Select(l.folder, nil).Wait(); err != nil {
		p.fail("SELECT %s: %s", l.folder, describe(err))
		return digests, false
	}
	opts := &imap.FetchOptions{
		UID: true, RFC822Size: true, Envelope: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{{Peek: true}},
	}
	var (
		bad, multipart, withID, withDate int
		bytes                            int64
		largest                          int64
	)
	t := time.Now()
	for _, r := range rows {
		msgs, err := cn.c.Fetch(imap.UIDSetNum(r.uid), opts).Collect()
		if err != nil {
			p.fail("%s UID %d: BODY.PEEK[] ENVELOPE BODYSTRUCTURE not readable: %s", l.folder, r.uid, describe(err))
			return digests, false
		}
		if len(msgs) != 1 || msgs[0].UID != r.uid {
			p.fail("%s UID %d: FETCH returns %d responses", l.folder, r.uid, len(msgs))
			bad++
			continue
		}
		m := msgs[0]
		var body []byte
		if len(m.BodySection) == 1 {
			body = m.BodySection[0].Bytes
		}
		switch {
		case len(m.BodySection) != 1:
			p.fail("%s UID %d: BODY[] missing from the response", l.folder, r.uid)
			bad++
		case int64(len(body)) != r.size || m.RFC822Size != r.size:
			p.fail("%s UID %d: BODY[] has %d bytes, RFC822.SIZE says %d (in the listing %d)", l.folder, r.uid, len(body), m.RFC822Size, r.size)
			bad++
		case m.Envelope == nil:
			p.fail("%s UID %d: ENVELOPE missing", l.folder, r.uid)
			bad++
		case m.BodyStructure == nil:
			p.fail("%s UID %d: BODYSTRUCTURE missing", l.folder, r.uid)
			bad++
		}
		if m.Envelope != nil {
			if m.Envelope.MessageID != "" {
				withID++
			}
			if !m.Envelope.Date.IsZero() {
				withDate++
			}
		}
		if _, ok := m.BodyStructure.(*imap.BodyStructureMultiPart); ok {
			multipart++
		}
		sum := sha256.Sum256(body)
		digests[r.uid] = hex.EncodeToString(sum[:])
		bytes += int64(len(body))
		if int64(len(body)) > largest {
			largest = int64(len(body))
		}
	}
	n := len(rows)
	p.check(bad == 0, "%s: %d messages read, for %d the length of BODY[] matches RFC822.SIZE, ENVELOPE and BODYSTRUCTURE are readable (%d bytes, largest %d, %s)",
		l.folder, n, n-bad, bytes, largest, secs(time.Since(t)))
	p.info("%s: %d multipart, %d with Message-ID, %d with date in ENVELOPE", l.folder, multipart, withID, withDate)
	// reading with PEEK must not have marked anything as read
	after, err := list(cn, l.folder)
	if err != nil {
		p.fail("%s after reading: %v", l.folder, err)
		return digests, false
	}
	p.check(sameListing(l, after), "%s: reading with BODY.PEEK[] changed no flag", l.folder)
	return digests, true
}

// wait blocks until cond holds for what the server pushed to cn, sending
// NOOP so that the server may speak, at most for d.
func (cn *conn) wait(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		cn.mu.Lock()
		ok := cond()
		cn.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if err := cn.c.Noop().Wait(); err != nil {
			return false
		}
		select {
		case <-cn.news:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// toggle sets or removes one flag on connection a and checks that the
// second connection b sees it and, for \Seen, that the counter of the edge
// (STATUS UNSEEN, asked on a third connection) follows.
func (p *probe) toggle(a, b, s3 *conn, uid imap.UID, flag imap.Flag, op imap.StoreFlagsOp, counts bool) {
	word := "+FLAGS"
	want := true
	if op == imap.StoreFlagsDel {
		word, want = "-FLAGS", false
	}
	what := fmt.Sprintf("UID STORE %d %s (%s)", uid, word, flag)
	var before *status
	if counts {
		var err error
		if before, err = statusOf(s3, "INBOX"); err != nil {
			p.fail("%s: STATUS before: %s", what, describe(err))
			return
		}
	}
	mBefore, _ := p.metrics()
	b.reset()
	t := time.Now()
	msgs, err := a.c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: op, Flags: []imap.Flag{flag}}, nil).Collect()
	if err != nil {
		p.fail("%s: %s", what, describe(err))
		return
	}
	took := time.Since(t)
	answered := false
	for _, m := range msgs {
		if m.UID == uid && hasFlag(m.Flags, flag) == want {
			answered = true
		}
	}
	p.check(answered, "%s: the response names the new flags (%s)", what, secs(took))
	if mAfter, err := p.metrics(); err == nil && mBefore != nil {
		p.check(mAfter.answered("/store", 200)-mBefore.answered("/store", 200) == 1,
			"%s: exactly one /store at the edge, answered with 200", what)
	}
	// the second connection learns it without asking for it
	pushed := b.wait(5*time.Second, func() bool {
		f, ok := b.pushed[uid]
		return ok && hasFlag(f, flag) == want
	})
	p.check(pushed, "%s: the second connection is notified of the change", what)
	flags, found, err := flagsOf(b, uid)
	if err != nil {
		p.fail("%s: second connection, UID FETCH FLAGS: %s", what, describe(err))
	} else {
		p.check(found && hasFlag(flags, flag) == want, "%s: visible in the second connection", what)
	}
	if counts {
		after, err := statusOf(s3, "INBOX")
		if err != nil {
			p.fail("%s: STATUS after: %s", what, describe(err))
			return
		}
		wantUnseen := int64(before.unseen) - 1
		if !want {
			wantUnseen = int64(before.unseen) + 1
		}
		p.check(int64(after.unseen) == wantUnseen, "%s: UNSEEN at the edge %d → %d", what, before.unseen, after.unseen)
	}
}

// idle waits in IDLE for a mail delivered from outside and returns its
// number (0 if none came).
func (p *probe) idle(a *conn) imap.UID {
	before, err := list(a, "INBOX")
	if err != nil {
		p.fail("INBOX before IDLE: %v", err)
		return 0
	}
	// a signal from before IDLE is not about this
	select {
	case <-p.signal:
		p.info("a signal came before IDLE was running: discarded")
	default:
	}
	a.reset()
	cmd, err := a.c.Idle()
	if err != nil {
		p.fail("IDLE: %s", describe(err))
		return 0
	}
	started := time.Now()
	hint := p.triggerHint
	if hint == "" {
		hint = "a line on standard input"
	}
	fmt.Printf(">>>    IDLE is running in INBOX (EXISTS %d). Now deliver a message to this mailbox from outside,\n", before.exists)
	fmt.Printf(">>>    then give the signal: %s. Waiting at most %s.\n", hint, p.idleWait)

	firstExists := func() (existsEvent, bool) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, e := range a.exists {
			if e.n > before.exists {
				return e, true
			}
		}
		return existsEvent{}, false
	}
	var signalAt time.Time
	timeout := time.After(p.idleWait)
waiting:
	for {
		select {
		case signalAt = <-p.signal:
			break waiting
		case <-a.news:
			if e, ok := firstExists(); ok {
				fmt.Printf("       EXISTS %d has arrived, %s after IDLE began; waiting for the signal\n", e.n, secs(e.at.Sub(started)))
			}
		case <-timeout:
			break waiting
		}
	}
	if signalAt.IsZero() {
		if e, ok := firstExists(); ok {
			p.ok("EXISTS %d arrived during IDLE, %s after it began (no signal until the end of the wait)", e.n, secs(e.at.Sub(started)))
		} else {
			p.fail("no signal and no EXISTS in %s", p.idleWait)
		}
	} else {
		// the mail is in: EXISTS has to be here, or come soon
		deadline := time.After(p.idleGrace)
		for {
			if _, ok := firstExists(); ok {
				break
			}
			select {
			case <-a.news:
				continue
			case <-deadline:
			}
			break
		}
		if e, ok := firstExists(); ok {
			d := e.at.Sub(signalAt)
			if d >= 0 {
				p.ok("EXISTS %d arrived during IDLE, %s after the signal", e.n, secs(d))
			} else {
				p.ok("EXISTS %d arrived during IDLE, %s before the signal (%s after IDLE began)", e.n, secs(-d), secs(e.at.Sub(started)))
			}
		} else {
			p.fail("no EXISTS within %s after the signal (IDLE ran %s)", p.idleGrace, secs(time.Since(started)))
		}
	}
	if err := cmd.Close(); err != nil {
		p.fail("DONE: %s", describe(err))
		return 0
	}
	if err := cmd.Wait(); err != nil {
		p.fail("IDLE ends with %s", describe(err))
		return 0
	}
	after, err := list(a, "INBOX")
	if err != nil {
		p.fail("INBOX after IDLE: %v", err)
		return 0
	}
	var fresh []row
	for _, r := range after.rows {
		if r.uid >= before.uidNext {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		p.info("INBOX after IDLE: no new message (EXISTS %d)", after.exists)
		return 0
	}
	p.check(after.exists == before.exists+uint32(len(fresh)), "INBOX after IDLE: %d new message(s), EXISTS %d → %d, UIDNEXT %d → %d",
		len(fresh), before.exists, after.exists, before.uidNext, after.uidNext)
	newest := fresh[len(fresh)-1]
	msgs, err := a.c.Fetch(imap.UIDSetNum(newest.uid), &imap.FetchOptions{UID: true, RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
	if err != nil || len(msgs) != 1 || len(msgs[0].BodySection) != 1 {
		p.fail("new message UID %d not readable: %s", newest.uid, describe(err))
	} else {
		p.check(int64(len(msgs[0].BodySection[0].Bytes)) == newest.size, "new message UID %d: BODY[] has %d bytes like RFC822.SIZE",
			newest.uid, len(msgs[0].BodySection[0].Bytes))
	}
	if p.expectInbox >= 0 {
		p.info("EXPECT_INBOX applied to the state before IDLE; now it is %d", after.exists)
	}
	return newest.uid
}

// expunge marks one mail as deleted and removes it with UID EXPUNGE.
func (p *probe) expunge(a, b, s3 *conn, arrived imap.UID) {
	l, err := list(a, "INBOX")
	if err != nil {
		p.fail("INBOX before EXPUNGE: %v", err)
		return
	}
	if len(l.rows) == 0 {
		p.fail("INBOX is empty: nothing to remove")
		return
	}
	target := l.rows[len(l.rows)-1].uid
	why := "the newest message"
	if arrived != 0 {
		target, why = arrived, "the message from the IDLE step"
	}
	if p.expungeUID != 0 {
		target, why = imap.UID(p.expungeUID), "EXPUNGE_UID"
	}
	if l.find(target) == nil {
		p.fail("UID %d (%s) does not exist in INBOX", target, why)
		return
	}
	if !a.c.Caps().Has(imap.CapUIDPlus) {
		p.fail("the bridge does not advertise UIDPLUS: without UID EXPUNGE, EXPUNGE would hit every message marked deleted")
		return
	}
	p.info("removing UID %d (%s); other messages marked deleted stay", target, why)
	if _, err := b.c.Select("INBOX", nil).Wait(); err != nil {
		p.fail("second connection, SELECT INBOX: %s", describe(err))
		return
	}
	before, err := statusOf(s3, "INBOX")
	if err != nil {
		p.fail("STATUS before: %s", describe(err))
		return
	}
	mBefore, _ := p.metrics()
	if _, err := a.c.Store(imap.UIDSetNum(target), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil {
		p.fail("UID STORE %d +FLAGS (\\Deleted): %s", target, describe(err))
		return
	}
	flags, found, err := flagsOf(b, target)
	p.check(err == nil && found && hasFlag(flags, imap.FlagDeleted), "UID %d is marked deleted in the second connection", target)
	b.reset()
	gone, err := a.c.UIDExpunge(imap.UIDSetNum(target)).Collect()
	if err != nil {
		p.fail("UID EXPUNGE %d: %s", target, describe(err))
		return
	}
	p.check(len(gone) == 1, "UID EXPUNGE %d: the response reports exactly one removed message (%d)", target, len(gone))
	if mAfter, err := p.metrics(); err == nil && mBefore != nil {
		p.check(mAfter.answered("/expunge", 200)-mBefore.answered("/expunge", 200) == 1, "exactly one /expunge at the edge, answered with 200")
	}
	pushed := b.wait(5*time.Second, func() bool { return len(b.expunged) == 1 })
	p.check(pushed, "the second connection is notified of EXPUNGE")
	_, found, err = flagsOf(b, target)
	p.check(err == nil && !found, "UID %d is gone in the second connection", target)
	after, err := statusOf(s3, "INBOX")
	if err != nil {
		p.fail("STATUS after: %s", describe(err))
		return
	}
	p.check(after.messages == before.messages-1, "MESSAGES at the edge %d → %d", before.messages, after.messages)
	again, err := list(a, "INBOX")
	if err != nil {
		p.fail("INBOX after EXPUNGE: %v", err)
		return
	}
	p.check(again.exists == l.exists-1 && again.find(target) == nil && again.uidNext == l.uidNext,
		"INBOX after EXPUNGE: EXISTS %d → %d, UIDNEXT unchanged %d", l.exists, again.exists, again.uidNext)
}

// reconnect opens a fresh connection and checks from the counters that the
// folder was not read in full again and a mail read before is not fetched
// again.
func (p *probe) reconnect(read map[imap.UID]string) {
	before, err := p.metrics()
	if err != nil {
		p.fail("/metrics: %v", err)
		return
	}
	t := time.Now()
	c, err := p.login(p.address, p.password)
	if err != nil {
		p.fail("new connection: %s", describe(err))
		return
	}
	defer c.close()
	l, err := list(c, "INBOX")
	if err != nil {
		p.fail("new connection: %v", err)
		return
	}
	took := time.Since(t)
	// the newest mail that was read before and is still there: the one most
	// likely to be in the cache of the bridge
	var cached imap.UID
	for i := len(l.rows) - 1; i >= 0; i-- {
		if _, ok := read[l.rows[i].uid]; ok {
			cached = l.rows[i].uid
			break
		}
	}
	fetched := false
	if cached != 0 {
		msgs, err := c.c.Fetch(imap.UIDSetNum(cached), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
		switch {
		case err != nil:
			p.fail("new connection, re-reading UID %d: %s", cached, describe(err))
		case len(msgs) != 1 || msgs[0].UID != cached || len(msgs[0].BodySection) != 1:
			p.fail("new connection, re-reading UID %d: %d responses", cached, len(msgs))
		default:
			sum := sha256.Sum256(msgs[0].BodySection[0].Bytes)
			fetched = p.check(hex.EncodeToString(sum[:]) == read[cached], "new connection: UID %d is, byte for byte, the message from before", cached)
		}
	}
	after, err := p.metrics()
	if err != nil {
		p.fail("/metrics: %v", err)
		return
	}
	d := func(path string) int64 { return after.calls(path) - before.calls(path) }
	p.info("Login, SELECT INBOX and UID FETCH 1:* (%d messages): %s", l.exists, secs(took))
	p.info("Calls to the edge meanwhile: /login %d, /folders %d, /entries %d, /changes %d, /raw %d, /seq %d",
		d("/login"), d("/folders"), d("/entries"), d("/changes"), d("/raw"), d("/seq"))
	p.check(d("/login") == 1, "the new connection logs in at the edge by itself (/login %d)", d("/login"))
	p.check(d("/entries") == 0 && after.num("mirror", "fullLoads") == before.num("mirror", "fullLoads"),
		"no full fetch of the folder: /entries %d, full loads %d → %d", d("/entries"),
		before.num("mirror", "fullLoads"), after.num("mirror", "fullLoads"))
	switch {
	case fetched:
		p.check(d("/raw") == 0, "an already read message (UID %d) comes without further /raw (/raw %d)", cached, d("/raw"))
	case cached == 0:
		p.skip("re-reading a message without /raw (no previously read message at hand)")
	}
}

// separation checks, with a second mailbox, that nothing of one shows in
// the other: not a flag, not a cached mail, not a device password.
func (p *probe) separation(a *conn, digests map[string]map[imap.UID]string) {
	if strings.EqualFold(strings.TrimSpace(p.secondAddress), strings.TrimSpace(p.address)) {
		p.fail("SECOND_ADDRESS is the same address as MAILBOX_ADDRESS")
		return
	}
	mBefore, _ := p.metrics()
	second, err := p.login(p.secondAddress, p.secondPassword)
	if err != nil {
		p.fail("second mailbox does not log in: %s", describe(err))
		return
	}
	defer second.close()
	p.ok("second mailbox logs in")
	mAfter, _ := p.metrics()
	if mBefore != nil && mAfter != nil {
		nb, _ := mBefore["mailboxes"].(map[string]any)
		na, _ := mAfter["mailboxes"].(map[string]any)
		p.check(len(na) == len(nb)+1 || len(na) >= 2, "the bridge keeps the second mailbox under its own identifier (%d → %d mailboxes in /metrics)", len(nb), len(na))
	}
	// the device password of one does not open the other
	if cn, err := p.dial(); err == nil {
		err := cn.c.Login(p.address, p.secondPassword).Wait()
		p.check(err != nil && codeOf(err) == "AUTHENTICATIONFAILED", "the password of the second mailbox does not open the first (%s)", describe(err))
		cn.close()
	}

	first, err := list(a, "INBOX")
	if err != nil {
		p.fail("first mailbox: %v", err)
		return
	}
	other, err := list(second, "INBOX")
	if err != nil {
		p.fail("second mailbox: %v", err)
		return
	}
	p.ok("second mailbox: INBOX with EXISTS %d, UIDNEXT %d, UIDVALIDITY %d", other.exists, other.uidNext, other.uidValidity)
	p.check(len(other.rows) == int(other.exists), "second mailbox, INBOX: UID FETCH 1:* returns %d rows at EXISTS %d", len(other.rows), other.exists)
	p.checkRows(other, "second mailbox, INBOX")
	if st, err := statusOf(second, "Sent"); err == nil {
		p.info("second mailbox: Sent with MESSAGES %d", st.messages)
	}
	p.info("first mailbox: UIDVALIDITY %d; same value in both: %v", first.uidValidity, first.uidValidity == other.uidValidity)

	// Mails of the first mailbox are in the cache of the bridge. The second
	// mailbox asks for the same folder and the same numbers: it has to get
	// its own mails, fetched from the edge with its own session. What can be
	// said without knowing the mails: each has the length the edge gave for
	// it in the second mailbox, each caused its own /raw, and (if the two
	// mailboxes do not hold the same mails) the content differs from what
	// the first mailbox has under that number.
	var shared []imap.UID
	for i := len(other.rows) - 1; i >= 0 && len(shared) < 20; i-- {
		if _, ok := digests["INBOX"][other.rows[i].uid]; ok {
			shared = append(shared, other.rows[i].uid)
		}
	}
	if len(shared) == 0 {
		p.skip("same UID in both mailboxes (no common UID in INBOX)")
	} else {
		before, _ := p.metrics()
		sized, differing, unreadable := 0, 0, 0
		for _, uid := range shared {
			msgs, err := second.c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, RFC822Size: true,
				BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
			if err != nil || len(msgs) != 1 || msgs[0].UID != uid || len(msgs[0].BodySection) != 1 {
				p.fail("second mailbox, UID %d: not readable: %s", uid, describe(err))
				unreadable++
				if err != nil {
					break // the connection may be gone
				}
				continue
			}
			body := msgs[0].BodySection[0].Bytes
			if int64(len(body)) == other.find(uid).size {
				sized++
			} else {
				p.fail("second mailbox, UID %d: BODY[] has %d bytes, its RFC822.SIZE is %d", uid, len(body), other.find(uid).size)
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != digests["INBOX"][uid] {
				differing++
			}
		}
		after, _ := p.metrics()
		n := len(shared)
		if unreadable == 0 {
			p.check(sized == n, "second mailbox: %d UIDs that also exist in the first were read; for %d, BODY[] has the length of its own RFC822.SIZE", n, sized)
			if before != nil && after != nil {
				got := after.calls("/raw") - before.calls("/raw")
				p.check(got == int64(n), "the second mailbox fetches each of them from the edge itself (/raw +%d for %d messages), none comes from the memory of the first", got, n)
			}
			if differing > 0 {
				p.ok("under %d of %d common UIDs the second mailbox holds different content than the first", differing, n)
			} else {
				p.skip("different content under the same UID: all %d common UIDs carry the same bytes in both mailboxes; whether these are the same messages or those of the first cannot be told apart this way", n)
			}
		}
	}

	// A change of flags in the first mailbox is no news for the second.
	if len(first.rows) == 0 {
		p.skip("change flags in the first mailbox (INBOX empty)")
		return
	}
	target := first.rows[len(first.rows)-1]
	op, back := imap.StoreFlagsAdd, imap.StoreFlagsDel
	if hasFlag(target.flags, imap.FlagFlagged) {
		op, back = back, op
	}
	second.reset()
	if _, err := a.c.Store(imap.UIDSetNum(target.uid), &imap.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); err != nil {
		p.fail("first mailbox, UID STORE %d: %s", target.uid, describe(err))
		return
	}
	time.Sleep(3 * time.Second) // longer than the bridge takes to look for changes
	_ = second.c.Noop().Wait()
	second.mu.Lock()
	pushes := second.pushes + len(second.exists) + len(second.expunged)
	second.mu.Unlock()
	again, err := list(second, "INBOX")
	if err != nil {
		p.fail("second mailbox after the change in the first: %v", err)
	} else {
		p.check(pushes == 0 && sameListing(other, again), "a change in the first mailbox does not reach the second (%d notifications, listing unchanged)", pushes)
	}
	if _, err := a.c.Store(imap.UIDSetNum(target.uid), &imap.StoreFlags{Op: back, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); err != nil {
		p.fail("first mailbox, reverting flags: %s", describe(err))
	}
}

// counters prints what the bridge counted: calls to the edge by path and
// status with their times, and the faults.
func (p *probe) counters(m0 metrics) {
	m, err := p.metrics()
	if err != nil {
		p.fail("/metrics: %v", err)
		return
	}
	edge, _ := m["edge"].(map[string]any)
	var paths []string
	for path := range edge {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	fmt.Printf("       %-10s %8s %8s %10s %10s %12s  %s\n", "Path", "Calls", "Errors", "Ø ms", "max ms", "Bytes in", "by status")
	for _, path := range paths {
		e, _ := edge[path].(map[string]any)
		num := func(k string) float64 { f, _ := e[k].(float64); return f }
		var by []string
		if st, ok := e["status"].(map[string]any); ok {
			var codes []string
			for c := range st {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			for _, c := range codes {
				n, _ := st[c].(float64)
				by = append(by, fmt.Sprintf("%s: %d", c, int64(n)))
			}
		}
		fmt.Printf("       %-10s %8d %8d %10.1f %10.1f %12d  %s\n", path, int64(num("count")), int64(num("errors")),
			num("avgUs")/1000, num("maxUs")/1000, int64(num("bytesIn")), strings.Join(by, ", "))
	}
	p.info("Mirror: full loads %d, rebuilds %d, new numbering %d, inconsistent %d, folder missing %d",
		m.num("mirror", "fullLoads"), m.num("mirror", "resyncs"), m.num("mirror", "uidValidityChanges"),
		m.num("mirror", "inconsistent"), m.num("mirror", "noFolder"))
	p.info("Raw store: %d messages, %d bytes; served from it %d times, not present %d times", m.num("rawCache", "entries"), m.num("rawCache", "bytes"),
		m.num("rawCache", "hits"), m.num("rawCache", "misses"))
	p.info("IMAP: logins %d, rejected %d, throttled %d, errors %d; sessions ended %d",
		m.num("imap", "loginsOk"), m.num("imap", "loginsFailed"), m.num("imap", "loginsThrottled"), m.num("imap", "loginsError"),
		m.num("imap", "sessionsEnded"))

	// What points to a fault between bridge and edge: none of it may have
	// happened during this run.
	d := func(k string) int64 { return m.num("edgeFaults", k) - m0.num("edgeFaults", k) }
	p.check(d("http400") == 0, "the edge rejected no request of the bridge as invalid (400: %d)", d("http400"))
	p.check(d("http413") == 0, "no request was too large for the edge (413: %d)", d("http413"))
	p.check(d("http500") == 0, "no internal error of the edge (500: %d)", d("http500"))
	p.check(d("oversize")+d("notSent") == 0, "the bridge withheld no call (too large: %d, shape: %d)", d("oversize"), d("notSent"))
	p.check(m.num("imap", "rawSizeMismatch") == m0.num("imap", "rawSizeMismatch"),
		"the size of every fetched message matches the size of its row (deviations: %d)",
		m.num("imap", "rawSizeMismatch")-m0.num("imap", "rawSizeMismatch"))
	p.check(m.num("panics") == m0.num("panics"), "no panic in the bridge")
	none := int64(0)
	for _, path := range paths {
		e, _ := edge[path].(map[string]any)
		if st, ok := e["status"].(map[string]any); ok {
			if n, ok := st["none"].(float64); ok {
				none += int64(n)
			}
		}
	}
	p.check(none == 0, "every call to the edge got an answer (without answer: %d)", none)
}
