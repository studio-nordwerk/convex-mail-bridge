// Package imapd is the IMAP side of the bridge: the session behind
// go-imap's imapserver. It holds no mail. Metadata comes from the mirror,
// content from the edge, and every change goes to the edge first.
package imapd

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/budget"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/guard"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/mirror"
)

// Config are the IMAP settings.
type Config struct {
	MaxAuthFailures int
	AppendMaxBytes  uint32
	IdleKeepalive   time.Duration // 0: no keepalive line during IDLE
	SearchMaxDepth  int
	SearchMaxNodes  int
	MaxSetRanges    int
	RawTimeout      time.Duration
	CallTimeout     time.Duration
	// FetchHook, if set, runs at the start of every FETCH (tests only).
	FetchHook func()
}

// Server serves IMAP connections.
type Server struct {
	cfg   Config
	store *mirror.Store
	edge  *edge.Client
	m     *metrics.Registry
	log   *logx.Logger
	srv   *imapserver.Server

	parseSem chan struct{} // structure parsing of large mails, bounded
	work     *budget.Bytes // mail bytes held outside the raw cache
	gate     *edge.Gate    // "throttled" answers of the edge, remembered

	mu       sync.Mutex
	sessions map[*session]struct{}
	closing  bool
}

// New builds the IMAP server.
func New(cfg Config, store *mirror.Store, ec *edge.Client, work *budget.Bytes, gate *edge.Gate, m *metrics.Registry, log *logx.Logger) *Server {
	if cfg.MaxAuthFailures <= 0 {
		cfg.MaxAuthFailures = 3
	}
	if cfg.SearchMaxDepth <= 0 {
		cfg.SearchMaxDepth = 16
	}
	if cfg.SearchMaxNodes <= 0 {
		cfg.SearchMaxNodes = 256
	}
	if cfg.MaxSetRanges <= 0 {
		cfg.MaxSetRanges = 4096
	}
	if cfg.RawTimeout <= 0 {
		cfg.RawTimeout = 60 * time.Second
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 15 * time.Second
	}
	s := &Server{
		cfg: cfg, store: store, edge: ec, m: m, log: log,
		parseSem: make(chan struct{}, 4),
		work:     work,
		gate:     gate,
		sessions: map[*session]struct{}{},
	}
	s.srv = imapserver.New(&imapserver.Options{
		NewSession: s.newSession,
		// Announced on purpose: no MOVE (there is none), no ID (the library
		// has no command for it), no ESEARCH/CONDSTORE. IDLE, UNSELECT,
		// ENABLE, SASL-IR, LITERAL- and AUTH=PLAIN come from the library,
		// APPENDLIMIT from the session.
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:  {},
			imap.CapUIDPlus:    {},
			imap.CapSpecialUse: {},
			imap.CapNamespace:  {},
		},
		Logger: &libLogger{s: s},
		// TLS ends at the proxy in front of the bridge.
		InsecureAuth: true,
		// DebugWriter stays nil, always: it would write passwords and mail
		// content.
	})
	return s
}

// Serve accepts connections from l (a guard.Listener) until it is closed.
func (s *Server) Serve(l net.Listener) error { return s.srv.Serve(l) }

// Shutdown tells every logged-in or connected client BYE and closes its
// connection. The listener has to be closed before.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.closing = true
	all := make([]*session, 0, len(s.sessions))
	for se := range s.sessions {
		all = append(all, se)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, se := range all {
		wg.Add(1)
		go func(se *session) {
			defer wg.Done()
			se.bye("Server is shutting down, please reconnect", 3*time.Second)
		}(se)
	}
	wg.Wait()
	// The library's own Close is not used: it waits on a WaitGroup that
	// Serve may not have entered yet. The caller closes the listener (Serve
	// returns) and every remaining connection.
}

func (s *Server) newSession(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	g, _ := c.NetConn().(*guard.Conn)
	se := &session{srv: s, conn: c, g: g}
	s.mu.Lock()
	closing := s.closing
	if !closing {
		s.sessions[se] = struct{}{}
	}
	s.mu.Unlock()
	if closing {
		return nil, nil, &imap.Error{Type: imap.StatusResponseTypeBye, Text: "Server is shutting down"}
	}
	return se, nil, nil
}

func (s *Server) forget(se *session) {
	s.mu.Lock()
	delete(s.sessions, se)
	s.mu.Unlock()
}

// libLogger receives the messages of the library. Their arguments can quote
// client input, so only the kind of message is kept.
type libLogger struct{ s *Server }

func (l *libLogger) Printf(format string, args ...interface{}) {
	s := l.s
	switch {
	case strings.HasPrefix(format, "panic"):
		s.m.IMAP.Panics.Add(1)
		fields := []logx.Field{logx.Proto("imap")}
		if len(args) >= 2 {
			if stack, ok := args[1].([]byte); ok {
				fields = append(fields, stackFrames(stack)...)
			}
		}
		s.log.Event("panic", fields...)
	case strings.HasPrefix(format, "failed to read command"):
		// client went away or sent something unreadable: not worth a line
	case strings.HasPrefix(format, "handling"):
		// The command word comes from the client: only a known name is
		// logged, anything else (a typo, a secret in the wrong place) as
		// "other".
		code := "other"
		if len(args) > 0 {
			if name, ok := args[0].(string); ok && knownCommands[strings.ToUpper(name)] {
				code = strings.ReplaceAll(strings.ToUpper(name), " ", "_")
			}
		}
		s.log.Event("imap_command_error", logx.Code(code))
	case strings.HasPrefix(format, "failed to write greeting"), strings.HasPrefix(format, "failed to create session"):
		// connection closed before the greeting
	default:
		s.log.Event("imap_lib", logx.Code(firstWords(format)))
	}
}

// knownCommands are the command names that may appear in the log.
var knownCommands = func() map[string]bool {
	names := []string{
		"CAPABILITY", "NOOP", "LOGOUT", "STARTTLS", "AUTHENTICATE", "LOGIN", "ENABLE", "UNAUTHENTICATE",
		"SELECT", "EXAMINE", "CREATE", "DELETE", "RENAME", "SUBSCRIBE", "UNSUBSCRIBE", "LIST", "LSUB",
		"NAMESPACE", "STATUS", "APPEND", "IDLE", "CHECK", "CLOSE", "UNSELECT", "EXPUNGE", "SEARCH",
		"FETCH", "STORE", "COPY", "MOVE", "SORT", "THREAD", "ID", "COMPRESS",
		"UID FETCH", "UID STORE", "UID SEARCH", "UID COPY", "UID MOVE", "UID EXPUNGE", "UID SORT", "UID THREAD",
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}()

func firstWords(s string) string {
	if i := strings.IndexAny(s, ":%("); i >= 0 {
		s = s[:i]
	}
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "_")
}

// stackFrames returns the function names of a stack trace (no arguments,
// no panic value).
func stackFrames(stack []byte) []logx.Field {
	var out []logx.Field
	for _, line := range strings.Split(string(stack), "\n") {
		if line == "" || line[0] == '\t' || strings.HasPrefix(line, "goroutine ") {
			continue
		}
		if i := strings.LastIndexByte(line, '('); i > 0 {
			line = line[:i]
		}
		if strings.HasPrefix(line, "runtime") || strings.HasPrefix(line, "panic") {
			continue
		}
		out = append(out, logx.Frame(len(out), line))
		if len(out) == 6 {
			break
		}
	}
	return out
}

func ctxFor(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
