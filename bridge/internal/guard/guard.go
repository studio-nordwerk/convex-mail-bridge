// Package guard sits between the TCP listener and the protocol libraries.
//
// It reads the PROXY header, enforces connection limits per client and in
// total, gives a connection a fixed time to log in, and limits what a client
// may send before and after login. For IMAP it follows the framing of the
// byte stream (lines and literals) exactly the way the library parses it, so
// that a literal the library refuses can never be parsed as commands.
package guard

import (
	"bufio"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
)

// Mode selects how the input stream is checked.
type Mode int

const (
	// ModeIMAP follows lines and literals.
	ModeIMAP Mode = iota
	// ModeLines only counts lines and bytes before login (SMTP; the SMTP
	// library limits lines and message size itself).
	ModeLines
)

// Config are the limits of one listener.
type Config struct {
	Proto string // "imap" or "smtp", for logs
	Mode  Mode

	ProxyRequired bool
	// TrustedPeers, if not empty, are the only TCP peers allowed to send a
	// PROXY header (the address range of the Fly proxy).
	TrustedPeers []netip.Prefix
	ProxyTimeout time.Duration

	MaxTotal int
	MaxPerIP int

	PreauthTimeout    time.Duration
	PreauthMaxLine    int
	PreauthMaxBytes   int
	PreauthMaxLiteral int64

	// After login (IMAP only).
	MaxLine           int
	MaxNonSyncLiteral int64

	TCPKeepAlive time.Duration

	// Reject returns the line written to a client before it is turned away
	// (connection limits, login timeout).
	Reject func(reason string) string

	Metrics *metrics.Proto
	Log     *logx.Logger
}

// ErrViolation is returned by Read when a client broke a limit. The
// connection is closed.
var ErrViolation = errors.New("guard: input limit exceeded")

// Listener wraps a TCP listener. Accept returns *Conn values only after the
// PROXY header was read and the limits were checked.
type Listener struct {
	inner net.Listener
	cfg   Config
	ch    chan *Conn
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup

	mu      sync.Mutex
	total   int // accepted, including connections still in the handshake
	perIP   map[netip.Prefix]int
	conns   map[*Conn]struct{}
	closing bool
	// With the PROXY protocol on, the TCP peers are proxies, not clients.
	// Their networks are counted so that an operator can see what to put
	// into the list of trusted peers. Bounded.
	peers map[netip.Prefix]int64
}

// Listen starts accepting on inner.
func Listen(inner net.Listener, cfg Config) *Listener {
	if cfg.ProxyTimeout <= 0 {
		cfg.ProxyTimeout = 5 * time.Second
	}
	if cfg.Reject == nil {
		cfg.Reject = func(string) string { return "" }
	}
	l := &Listener{
		inner: inner,
		cfg:   cfg,
		ch:    make(chan *Conn),
		done:  make(chan struct{}),
		perIP: map[netip.Prefix]int{},
		conns: map[*Conn]struct{}{},
		peers: map[netip.Prefix]int64{},
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l
}

// Addr returns the address of the inner listener.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Accept returns the next checked connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting. Open connections stay open (see CloseAll).
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		l.mu.Lock()
		l.closing = true
		l.mu.Unlock()
		close(l.done)
		err = l.inner.Close()
	})
	return err
}

// Conns returns the open connections.
func (l *Listener) Conns() []*Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Conn, 0, len(l.conns))
	for c := range l.conns {
		out = append(out, c)
	}
	return out
}

func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	var delay time.Duration
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// e.g. too many open files: back off instead of spinning
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			time.Sleep(delay)
			continue
		}
		delay = 0
		l.mu.Lock()
		over := l.cfg.MaxTotal > 0 && l.total >= l.cfg.MaxTotal
		if !over {
			l.total++
		}
		l.mu.Unlock()
		if over {
			l.cfg.Metrics.RejectedTotal.Add(1)
			l.turnAway(raw, "too many connections")
			continue
		}
		go l.handshake(raw)
	}
}

func (l *Listener) turnAway(raw net.Conn, reason string) {
	if line := l.cfg.Reject(reason); line != "" {
		_ = raw.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = raw.Write([]byte(line))
	}
	_ = raw.Close()
}

func (l *Listener) releaseTotal() {
	l.mu.Lock()
	l.total--
	l.mu.Unlock()
}

func ipKey(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	if a.Is4() {
		return netip.PrefixFrom(a, 32)
	}
	p, _ := a.Prefix(64)
	return p
}

func addrOf(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(ta.IP); ok {
			return ip.Unmap()
		}
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

func (l *Listener) handshake(raw net.Conn) {
	if tc, ok := raw.(*net.TCPConn); ok && l.cfg.TCPKeepAlive > 0 {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(l.cfg.TCPKeepAlive)
	}
	peer := addrOf(raw.RemoteAddr())
	br := bufio.NewReaderSize(raw, 4096)
	client := peer
	if l.cfg.ProxyRequired {
		l.notePeer(peer)
		if len(l.cfg.TrustedPeers) > 0 && !inPrefixes(peer, l.cfg.TrustedPeers) {
			l.cfg.Metrics.RejectedProxy.Add(1)
			l.cfg.Log.Event("conn_rejected", logx.Proto(l.cfg.Proto), logx.Code("proxy_peer_untrusted"))
			_ = raw.Close()
			l.releaseTotal()
			return
		}
		_ = raw.SetReadDeadline(time.Now().Add(l.cfg.ProxyTimeout))
		ip, err := readProxyHeader(br)
		_ = raw.SetReadDeadline(time.Time{})
		if err != nil {
			l.cfg.Metrics.RejectedProxy.Add(1)
			code := "proxy_missing"
			switch err {
			case errProxyInvalid:
				code = "proxy_invalid"
			case errProxyLocal:
				code = "proxy_local"
			}
			l.cfg.Log.Event("conn_rejected", logx.Proto(l.cfg.Proto), logx.Code(code))
			_ = raw.Close()
			l.releaseTotal()
			return
		}
		client = ip
	}
	if !client.IsValid() {
		client = netip.IPv4Unspecified()
	}
	// A bare address from here on: the edge takes it for /login without
	// zone, port or brackets, and nothing else.
	client = client.Unmap().WithZone("")
	key := ipKey(client)

	l.mu.Lock()
	if l.cfg.MaxPerIP > 0 && l.perIP[key] >= l.cfg.MaxPerIP {
		l.total--
		l.mu.Unlock()
		l.cfg.Metrics.RejectedPerIP.Add(1)
		l.turnAway(raw, "too many connections from your address")
		return
	}
	l.perIP[key]++
	c := &Conn{Conn: raw, br: br, l: l, client: client, key: key}
	l.conns[c] = struct{}{}
	closing := l.closing
	l.mu.Unlock()

	l.cfg.Metrics.ConnsAccepted.Add(1)
	l.cfg.Metrics.ConnsOpen.Add(1)
	if closing {
		_ = c.Close()
		return
	}
	if l.cfg.PreauthTimeout > 0 {
		c.timer = time.AfterFunc(l.cfg.PreauthTimeout, c.preauthExpired)
	}
	select {
	case l.ch <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *Listener) notePeer(peer netip.Addr) {
	if !peer.IsValid() {
		return
	}
	bits := 16
	if peer.Is6() {
		bits = 48
	}
	p, err := peer.Prefix(bits)
	if err != nil {
		return
	}
	l.mu.Lock()
	if _, ok := l.peers[p]; ok || len(l.peers) < 16 {
		l.peers[p]++
	}
	l.mu.Unlock()
}

// ProxyPeers returns the networks PROXY headers came from, with counts.
func (l *Listener) ProxyPeers() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.peers))
	for p, n := range l.peers {
		out[p.String()] = n
	}
	return out
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// pending literal kinds
const (
	litNone = iota
	litSync
	litNonSync
)

// Conn is a checked client connection.
type Conn struct {
	net.Conn
	br     *bufio.Reader
	l      *Listener
	client netip.Addr
	key    netip.Prefix
	timer  *time.Timer

	authed    atomic.Bool
	doomed    atomic.Bool
	closed    atomic.Bool
	closeOnce sync.Once
	wmu       sync.Mutex // serializes writes to the socket

	// read side, touched by the reading goroutine only
	left        []byte // read from the socket, not yet handed out
	inLiteral   int64
	cmdLen      int
	mark        marker // literal announcement at the end of the current line
	inQuote     bool   // inside a quoted string of the current line
	quoteEsc    bool   // after a backslash inside a quoted string
	preauthRead int
	violated    bool

	// decision window after a line that announced a literal; the write side
	// records what the server answered
	pmu          sync.Mutex
	pendingKind  int
	pendingSize  int64
	pendingWrote bool
	pendingPlus  bool
}

// ClientIP is the client address (from the PROXY header, or the TCP peer
// when the PROXY protocol is off).
func (c *Conn) ClientIP() netip.Addr { return c.client }

// Authenticated reports whether SetAuthenticated was called.
func (c *Conn) Authenticated() bool { return c.authed.Load() }

// SetAuthenticated ends the login phase: the login timer stops and the
// limits for logged-in clients apply from the next read on.
func (c *Conn) SetAuthenticated() {
	if c.authed.Swap(true) {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
	}
}

// CloseAfterNextWrite makes the connection close right after the next
// response has been written (too many failed logins).
func (c *Conn) CloseAfterNextWrite() { c.doomed.Store(true) }

func (c *Conn) preauthExpired() {
	if c.authed.Load() || c.closed.Load() {
		return
	}
	c.l.cfg.Metrics.PreauthTimeouts.Add(1)
	c.l.cfg.Log.Event("conn_closed", logx.Proto(c.l.cfg.Proto), logx.Code("login_timeout"))
	c.WriteDirect(c.l.cfg.Reject("login timeout"))
	_ = c.Close()
}

// WriteDirect writes a line outside the protocol library (used for the
// login timeout and on shutdown). Errors are ignored.
func (c *Conn) WriteDirect(line string) {
	if line == "" {
		return
	}
	c.wmu.Lock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.Conn.Write([]byte(line))
	_ = c.Conn.SetWriteDeadline(time.Time{})
	c.wmu.Unlock()
}

// Write writes server output and notes it for the literal decision window.
func (c *Conn) Write(p []byte) (int, error) {
	c.pmu.Lock()
	if c.pendingKind != litNone && len(p) > 0 {
		c.pendingWrote = true
		if p[0] == '+' {
			c.pendingPlus = true
		}
	}
	c.pmu.Unlock()
	c.wmu.Lock()
	n, err := c.Conn.Write(p)
	c.wmu.Unlock()
	if c.doomed.Load() {
		_ = c.Close()
	}
	return n, err
}

// Close closes the connection once and releases its slot.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.timer != nil {
			c.timer.Stop()
		}
		err = c.Conn.Close()
		l := c.l
		l.mu.Lock()
		delete(l.conns, c)
		l.perIP[c.key]--
		if l.perIP[c.key] <= 0 {
			delete(l.perIP, c.key)
		}
		l.total--
		l.mu.Unlock()
		l.cfg.Metrics.ConnsOpen.Add(-1)
	})
	return err
}

func (c *Conn) violation(code string) error {
	if c.violated {
		return ErrViolation
	}
	c.violated = true
	if c.authed.Load() {
		c.l.cfg.Metrics.InputViolation.Add(1)
	} else {
		c.l.cfg.Metrics.PreauthViolation.Add(1)
	}
	c.l.cfg.Log.Event("conn_closed", logx.Proto(c.l.cfg.Proto), logx.Code(code))
	_ = c.Close()
	return ErrViolation
}

// Read hands input to the protocol library. In IMAP mode it delivers at
// most up to the end of a line that announces a literal, so that the
// library's answer to that line (continuation, or a rejection) is known
// before the next byte is interpreted.
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.violated {
		return 0, ErrViolation
	}
	if c.l.cfg.Mode == ModeIMAP {
		if err := c.resolvePending(); err != nil {
			return 0, err
		}
	}
	var n int
	var err error
	if len(c.left) > 0 {
		n = copy(p, c.left)
		c.left = c.left[n:]
		if len(c.left) == 0 {
			c.left = nil
		}
	} else {
		n, err = c.br.Read(p)
		if n == 0 {
			return 0, err
		}
		err = nil // a read error comes back on the next call
	}

	authed := c.authed.Load()
	k := n
	if c.l.cfg.Mode == ModeIMAP {
		var verr error
		k, verr = c.scanIMAP(p[:n], authed)
		if verr != nil {
			return 0, verr
		}
	} else if !authed {
		k, err = c.scanLines(p[:n])
		if err != nil {
			return 0, err
		}
	}
	if k < n {
		rest := make([]byte, 0, n-k+len(c.left))
		rest = append(rest, p[k:n]...)
		rest = append(rest, c.left...)
		c.left = rest
	}
	if !authed {
		c.preauthRead += k
		if c.l.cfg.PreauthMaxBytes > 0 && c.preauthRead > c.l.cfg.PreauthMaxBytes {
			return 0, c.violation("preauth_bytes")
		}
	}
	return k, err
}

// scanLines counts line length before login (SMTP).
func (c *Conn) scanLines(p []byte) (int, error) {
	for i, b := range p {
		c.cmdLen++
		if c.l.cfg.PreauthMaxLine > 0 && c.cmdLen > c.l.cfg.PreauthMaxLine {
			return 0, c.violation("preauth_line")
		}
		if b == '\n' {
			c.cmdLen = 0
			// one line per read before login, so that limits switch at the
			// right byte
			return i + 1, nil
		}
	}
	return len(p), nil
}

func (c *Conn) resolvePending() error {
	c.pmu.Lock()
	kind, size, wrote, plus := c.pendingKind, c.pendingSize, c.pendingWrote, c.pendingPlus
	c.pendingKind, c.pendingWrote, c.pendingPlus = litNone, false, false
	c.pmu.Unlock()
	switch kind {
	case litSync:
		if plus {
			c.inLiteral = size
		}
	case litNonSync:
		if wrote {
			// The library answered before reading the literal: it refused
			// it, and the literal bytes would now be read as commands.
			return c.violation("literal_refused")
		}
		c.inLiteral = size
	}
	return nil
}

// scanIMAP returns how many of the bytes in p may be handed out now: at
// most one line, or literal data up to the end of the literal. One line at
// a time matters. The library asks for more input only when it is done
// with everything it has, including writing the answer; so whatever the
// server writes after a line with a literal announcement was handed out is
// the answer to that line and nothing else, also when the client sent
// several commands in one go.
func (c *Conn) scanIMAP(p []byte, authed bool) (int, error) {
	maxLine := c.l.cfg.MaxLine
	if !authed {
		maxLine = c.l.cfg.PreauthMaxLine
	}
	i := 0
	for i < len(p) {
		if c.inLiteral > 0 {
			m := int64(len(p) - i)
			if m > c.inLiteral {
				m = c.inLiteral
			}
			i += int(m)
			c.inLiteral -= m
			continue
		}
		b := p[i]
		i++
		c.cmdLen++
		if maxLine > 0 && c.cmdLen > maxLine {
			if authed {
				return 0, c.violation("line_too_long")
			}
			return 0, c.violation("preauth_line")
		}
		if b != '\n' {
			// Quoted strings: the library reads them up to the closing
			// quote, across line ends. An announcement inside one is not
			// an announcement for the library, so it is none here either.
			switch {
			case c.quoteEsc:
				c.quoteEsc = false
			case c.inQuote && b == '\\':
				c.quoteEsc = true
			case b == '"':
				c.inQuote = !c.inQuote
			}
			if c.inQuote {
				c.mark = marker{}
			} else {
				c.mark.feed(b)
			}
			continue
		}
		if c.inQuote {
			// A line must not end inside a quoted string. The library
			// would read on into the next line, and from there on the two
			// would disagree about what is a command.
			return 0, c.violation("quote_across_lines")
		}
		size, nonSync, isLit := c.mark.end()
		if !isLit {
			c.cmdLen = 0
			return i, nil
		}
		if !authed && size > c.l.cfg.PreauthMaxLiteral {
			return 0, c.violation("preauth_literal")
		}
		if authed && nonSync && size > c.l.cfg.MaxNonSyncLiteral {
			return 0, c.violation("nonsync_literal_too_big")
		}
		c.pmu.Lock()
		if nonSync {
			c.pendingKind = litNonSync
		} else {
			c.pendingKind = litSync
		}
		c.pendingSize = size
		c.pendingWrote, c.pendingPlus = false, false
		c.pmu.Unlock()
		return i, nil
	}
	return i, nil
}

// marker follows the bytes of a line and knows, at its end, whether the
// line ends with a literal announcement the way the library reads one:
// "{" digits ["+"] "}" [SP] [CR] LF. It keeps no buffer, so leading zeros
// or any other padding cannot push the announcement out of sight.
type marker struct {
	state   int // 0 none, 1 digits, 2 after "+", 3 after "}", 4 after "} ", 5 after CR
	digits  int
	size    int64
	nonSync bool
}

func (m *marker) feed(b byte) {
	if b == '{' {
		*m = marker{state: 1}
		return
	}
	switch m.state {
	case 1:
		switch {
		case b >= '0' && b <= '9':
			m.digits++
			if m.size > (1<<63-1-9)/10 {
				m.size = 1<<63 - 1 // more than anything allowed
			} else {
				m.size = m.size*10 + int64(b-'0')
			}
			return
		case b == '+' && m.digits > 0:
			m.state, m.nonSync = 2, true
			return
		case b == '}' && m.digits > 0:
			m.state = 3
			return
		}
	case 2:
		if b == '}' {
			m.state = 3
			return
		}
	case 3:
		if b == ' ' {
			m.state = 4
			return
		}
		if b == '\r' {
			m.state = 5
			return
		}
	case 4:
		if b == '\r' {
			m.state = 5
			return
		}
	}
	m.state = 0
}

// end is called at LF and resets the marker for the next line.
func (m *marker) end() (size int64, nonSync, ok bool) {
	size, nonSync, ok = m.size, m.nonSync, m.state >= 3
	*m = marker{}
	return size, nonSync, ok
}

// literalMarker runs a whole line (without its LF) through a marker.
func literalMarker(line []byte) (size int64, nonSync, ok bool) {
	var m marker
	for _, b := range line {
		m.feed(b)
	}
	return m.end()
}
