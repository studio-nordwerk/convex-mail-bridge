// Package smtpd is the submission side of the bridge: it takes a mail from
// a logged-in mail program and hands bytes and envelope to the edge. It
// relays nothing by itself and keeps nothing.
package smtpd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/budget"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/guard"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/mirror"
)

// Config are the SMTP settings.
type Config struct {
	Domain          string
	MaxMessageBytes int64
	MaxRecipients   int
	MaxAuthFailures int
	CallTimeout     time.Duration
	SubmitTimeout   time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
}

// Server serves SMTP submission.
type Server struct {
	cfg   Config
	store *mirror.Store
	edge  *edge.Client
	work  *budget.Bytes
	gate  *edge.Gate
	m     *metrics.Registry
	log   *logx.Logger
	srv   *smtp.Server
}

// New builds the SMTP server.
func New(cfg Config, store *mirror.Store, ec *edge.Client, work *budget.Bytes, gate *edge.Gate, m *metrics.Registry, log *logx.Logger) *Server {
	if cfg.MaxAuthFailures <= 0 {
		cfg.MaxAuthFailures = 3
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 15 * time.Second
	}
	if cfg.SubmitTimeout <= 0 {
		cfg.SubmitTimeout = 60 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 5 * time.Minute
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.Domain == "" {
		cfg.Domain = "mail-bridge"
	}
	s := &Server{cfg: cfg, store: store, edge: ec, work: work, gate: gate, m: m, log: log}
	srv := smtp.NewServer(smtp.BackendFunc(s.newSession))
	srv.Domain = cfg.Domain
	srv.MaxMessageBytes = cfg.MaxMessageBytes
	srv.MaxRecipients = cfg.MaxRecipients
	srv.ReadTimeout = cfg.ReadTimeout
	srv.WriteTimeout = cfg.WriteTimeout
	// TLS ends at the proxy in front of the bridge.
	srv.AllowInsecureAuth = true
	srv.ErrorLog = &libLogger{s: s}
	// srv.Debug stays nil, always.
	s.srv = srv
	return s
}

// Serve accepts connections from l (a guard.Listener) until it is closed.
func (s *Server) Serve(l net.Listener) error { return s.srv.Serve(l) }

// Close closes all connections.
func (s *Server) Close() { _ = s.srv.Close() }

type libLogger struct{ s *Server }

func (l *libLogger) Printf(format string, v ...interface{}) {
	if strings.HasPrefix(format, "panic") {
		l.s.m.SMTP.Panics.Add(1)
		l.s.log.Event("panic", logx.Proto("smtp"))
		return
	}
	// "error handling <addr>: ...": a connection ended badly. The text
	// names the client address, so only the fact is kept, and not per line.
}

func (l *libLogger) Println(v ...interface{}) {}

func smtpErr(code int, enh smtp.EnhancedCode, msg string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: msg}
}

var (
	errAuthRequired = smtpErr(530, smtp.EnhancedCode{5, 7, 0}, "Authentication required")
	errAuthFailed   = smtpErr(535, smtp.EnhancedCode{5, 7, 8}, "Authentication failed")
	errAuthTemp     = smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Temporary authentication failure, try again later")
	errSender       = smtpErr(550, smtp.EnhancedCode{5, 7, 1}, "Sender address is not the address you are logged in with")
	errRecipient    = smtpErr(553, smtp.EnhancedCode{5, 1, 3}, "Recipient address is not valid")
	errTooBig       = smtpErr(552, smtp.EnhancedCode{5, 3, 4}, "Message is too large")
	errLongLine     = smtpErr(554, smtp.EnhancedCode{5, 6, 0}, "Message has a line that is too long")
	errTemp         = smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary failure, try again later")
	errSessionOver  = smtpErr(535, smtp.EnhancedCode{5, 7, 8}, "Session ended, please log in again")
)

type session struct {
	srv *Server
	g   *guard.Conn

	failures int
	address  string // as logged in
	token    string
	hash     string
	// The session is registered like an IMAP connection: the poll confirms
	// it, and when the edge no longer accepts it or it runs out, the
	// connection ends.
	member *mirror.Member
	expiry *time.Timer

	from string
	to   []string
}

// kill ends the connection because its session is over.
func (se *session) kill(code string) {
	if code == "expired" {
		se.srv.m.SMTP.SessionsExpired.Add(1)
	} else {
		se.srv.m.SMTP.SessionsEnded.Add(1)
	}
	se.srv.log.Event("smtp_conn_ended", logx.Code(code), logx.MB(se.hash))
	if g := se.g; g != nil {
		go func() {
			g.WriteDirect("421 4.7.0 Session ended, please log in again\r\n")
			_ = g.Close()
		}()
	}
}

func (s *Server) newSession(c *smtp.Conn) (smtp.Session, error) {
	g, _ := c.Conn().(*guard.Conn)
	return &session{srv: s, g: g}, nil
}

func (se *session) failed(err error) error {
	se.failures++
	if se.failures >= se.srv.cfg.MaxAuthFailures {
		se.srv.m.SMTP.AuthLockouts.Add(1)
		se.srv.log.Event("smtp_conn_ended", logx.Code("auth_failures"))
		if se.g != nil {
			se.g.CloseAfterNextWrite()
		}
	}
	return err
}

func (se *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (se *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, se.failed(smtp.ErrAuthUnknownMechanism)
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		return se.login(identity, username, password)
	}), nil
}

func validCredential(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func (se *session) login(identity, username, password string) error {
	m := se.srv.m
	username = strings.TrimSpace(username)
	if identity != "" && !strings.EqualFold(strings.TrimSpace(identity), username) {
		m.SMTP.LoginsFailed.Add(1)
		return se.failed(errAuthFailed)
	}
	if !validCredential(username, edge.MaxAddressChars) || !validCredential(password, edge.MaxPasswordChars) ||
		!edge.LoginInput(username, password) {
		m.SMTP.LoginsFailed.Add(1)
		return se.failed(errAuthFailed)
	}
	var client netip.Addr
	if se.g != nil {
		client = se.g.ClientIP()
	}
	clientIP := edge.ClientIP(client)
	if se.srv.gate.Blocked(username, clientIP, password) {
		m.SMTP.LoginsThrottled.Add(1)
		return se.failed(errAuthTemp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), se.srv.cfg.CallTimeout)
	defer cancel()
	sess, err := se.srv.edge.Login(ctx, username, password, client)
	if err != nil {
		switch {
		case edge.Is(err, 401, "auth"):
			m.SMTP.LoginsFailed.Add(1)
			se.srv.log.Event("smtp_login_failed", logx.Code("auth"), logx.Addr(logx.HashAddress(username)))
			return se.failed(errAuthFailed)
		case edge.Is(err, 429, "throttled"):
			se.srv.gate.Block(username, clientIP, password, edge.RetryAfterOf(err))
			m.SMTP.LoginsThrottled.Add(1)
			return se.failed(errAuthTemp)
		case edge.Is(err, 503, "preparing"):
			m.SMTP.LoginsPreparing.Add(1)
			return se.failed(errAuthTemp)
		}
		m.SMTP.LoginsError.Add(1)
		se.srv.log.Event("smtp_login_failed", logx.Code("edge_"+edge.CodeOf(err)))
		return se.failed(errAuthTemp)
	}
	// The password is not kept.
	se.address = username
	se.token = sess.Token
	se.hash = logx.HashMailbox(sess.Mailbox)
	se.member = se.srv.store.Attach(sess.Mailbox, sess.Token, sess.ExpiresAt, se.kill)
	if !sess.ExpiresAt.IsZero() {
		member := se.member
		se.expiry = time.AfterFunc(time.Until(sess.ExpiresAt), func() { member.Kill("expired") })
	}
	if se.g != nil {
		se.g.SetAuthenticated()
	}
	m.SMTP.LoginsOK.Add(1)
	se.srv.log.Event("smtp_login", logx.MB(se.hash), logx.Addr(logx.HashAddress(username)))
	return nil
}

func (se *session) Reset() {
	se.from = ""
	se.to = nil
}

func (se *session) Logout() error {
	se.token = ""
	if se.expiry != nil {
		se.expiry.Stop()
	}
	if se.member != nil {
		se.member.Detach()
	}
	return nil
}

func (se *session) ready() bool {
	return se.token != "" && se.member != nil && !se.member.Killed()
}

// validAddress is deliberately narrow: printable ASCII with one "@" part,
// no spaces, bounded. It ends up in an HTTP header.
func validAddress(a string) bool {
	if len(a) < 3 || len(a) > 320 || !strings.Contains(a, "@") {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c <= 0x20 || c >= 0x7f || c == '<' || c == '>' || c == '"' || c == ',' || c == '\\' {
			return false
		}
	}
	return true
}

func (se *session) Mail(from string, opts *smtp.MailOptions) error {
	if !se.ready() {
		return errAuthRequired
	}
	if !strings.EqualFold(strings.TrimSpace(from), se.address) {
		se.srv.m.SenderRejected.Add(1)
		se.srv.log.Event("smtp_sender_rejected", logx.MB(se.hash))
		return errSender
	}
	se.from = se.address
	se.to = nil
	return nil
}

func (se *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !se.ready() || se.from == "" {
		return errAuthRequired
	}
	if !validAddress(to) {
		return errRecipient
	}
	if max := se.srv.cfg.MaxRecipients; max > 0 && len(se.to) >= max {
		return smtpErr(452, smtp.EnhancedCode{4, 5, 3}, "Too many recipients")
	}
	se.to = append(se.to, to)
	return nil
}

func enhancedFor(code int) smtp.EnhancedCode {
	switch code {
	case 550:
		return smtp.EnhancedCode{5, 7, 1}
	case 552:
		return smtp.EnhancedCode{5, 3, 4}
	case 554:
		return smtp.EnhancedCode{5, 6, 0}
	case 451:
		return smtp.EnhancedCode{4, 3, 0}
	}
	if code >= 500 {
		return smtp.EnhancedCode{5, 0, 0}
	}
	return smtp.EnhancedCode{4, 0, 0}
}

func (se *session) Data(r io.Reader) error {
	m := se.srv.m
	if !se.ready() || se.from == "" || len(se.to) == 0 {
		return errAuthRequired
	}
	max := se.srv.cfg.MaxMessageBytes
	// The message is held in memory until the edge has answered. That
	// memory is budgeted per mailbox, and a transfer has a fixed time: with
	// BDAT a client could otherwise keep one open (and its memory reserved)
	// for as long as it likes.
	bctx, bcancel := context.WithTimeout(context.Background(), se.srv.cfg.CallTimeout)
	release, berr := se.srv.work.Acquire(bctx, se.hash, max)
	bcancel()
	if berr != nil {
		m.SubmitDeferred.Add(1)
		return errTemp
	}
	defer release()
	var deadline *time.Timer
	if g := se.g; g != nil {
		deadline = time.AfterFunc(se.srv.cfg.ReadTimeout, func() {
			se.srv.log.Event("smtp_conn_ended", logx.Code("data_timeout"), logx.MB(se.hash))
			_ = g.Close()
		})
		defer deadline.Stop()
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, max+1))
	if deadline != nil {
		// Only the transfer is timed here. Handing the mail to the edge
		// has its own limit, and its answer must still reach the client.
		deadline.Stop()
	}
	if err != nil {
		if serr, ok := err.(*smtp.SMTPError); ok {
			return serr // the library's own size limit
		}
		if errors.Is(err, smtp.ErrTooLongLine) {
			return errLongLine
		}
		return errTemp
	}
	if n > max {
		return errTooBig
	}
	ctx, cancel := context.WithTimeout(context.Background(), se.srv.cfg.SubmitTimeout)
	defer cancel()
	res, err := se.srv.edge.Submit(ctx, edge.Caller{Session: se.token, MB: se.hash}, se.from, se.to, buf.Bytes())
	if err == nil {
		if res.Status == "sent" {
			m.SubmitSent.Add(1)
		} else {
			// Unclear whether the mail went out. A 250 keeps the mail
			// program from sending it a second time; the app shows the mail
			// as unclear.
			m.SubmitUnknown.Add(1)
		}
		se.srv.log.Event("smtp_submit", logx.Code(res.Status), logx.MB(se.hash), logx.N("bytes", n))
		return nil
	}
	if edge.IsSession(err) {
		m.SMTP.SessionsEnded.Add(1)
		se.srv.log.Event("smtp_conn_ended", logx.Code("session"), logx.MB(se.hash))
		se.token = ""
		if se.g != nil {
			se.g.CloseAfterNextWrite()
		}
		return errSessionOver
	}
	var ee *edge.Error
	if e, ok := err.(*edge.Error); ok {
		ee = e
	}
	// The edge names the answer for the mail program. Not so for a 500: that
	// is an error inside the edge, always temporary for the mail program.
	if ee != nil && ee.Status >= 400 && ee.Status != 500 && ee.SMTPCode >= 400 && ee.SMTPCode <= 599 {
		msg := ee.Message
		if msg == "" {
			msg = "Message was not accepted"
		}
		if ee.SMTPCode >= 500 {
			m.SubmitRejected.Add(1)
		} else {
			m.SubmitDeferred.Add(1)
		}
		se.srv.log.Event("smtp_submit_failed", logx.Code(ee.Code), logx.MB(se.hash), logx.N("smtp", int64(ee.SMTPCode)))
		return smtpErr(ee.SMTPCode, enhancedFor(ee.SMTPCode), msg)
	}
	// No usable answer: temporary, and the bridge does not try again by
	// itself.
	m.SubmitDeferred.Add(1)
	se.srv.log.Event("smtp_submit_failed", logx.Code("edge_"+edge.CodeOf(err)), logx.MB(se.hash))
	return errTemp
}
