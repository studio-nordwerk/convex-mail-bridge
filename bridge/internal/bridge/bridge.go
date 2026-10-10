// Package bridge wires the parts together: listeners with their guards,
// the IMAP and SMTP servers, the mirror with its poller, and the internal
// HTTP port.
package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/budget"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/config"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/guard"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/imapd"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/logx"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/mirror"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/rawcache"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/smtpd"
)

// Bridge is a running bridge.
type Bridge struct {
	cfg *config.Config
	Log *logx.Logger
	M   *metrics.Registry

	edge  *edge.Client
	raw   *rawcache.Cache
	store *mirror.Store
	imap  *imapd.Server
	smtp  *smtpd.Server

	imapL  *guard.Listener
	smtpL  *guard.Listener
	adminL net.Listener
	admin  *http.Server

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopping atomic.Bool
	stopOnce sync.Once
}

// Options are the things tests replace.
type Options struct {
	LogWriter     io.Writer
	EdgeTransport http.RoundTripper
	// NoPoller leaves the periodic poll to the caller (tests call SyncNow).
	NoPoller bool
	// FetchHook runs at the start of every IMAP FETCH. Tests use it to make
	// one connection panic; it is nil otherwise.
	FetchHook func()
}

// Start opens the listeners and starts serving.
func Start(cfg *config.Config, o Options) (*Bridge, error) {
	b := &Bridge{cfg: cfg, Log: logx.New(o.LogWriter), M: metrics.New()}
	b.edge = edge.New(edge.Options{
		SiteURL: cfg.SiteURL, Secret: cfg.Secret, Metrics: b.M,
		MaxRawBytes: cfg.RawMaxBytes, Transport: o.EdgeTransport,
		CallTimeout: cfg.EdgeTimeout, LongTimeout: cfg.EdgeLongTimeout,
		// 400, 413 and 500 of the edge (and calls that were not sent for
		// their size or form) are no fault of a mail program. They are
		// counted under edgeFaults and noted here with path and status.
		OnFault: func(path string, status int, code string) {
			b.Log.Event("edge_fault", logx.Code(code), logx.N("status", int64(status)), logx.Path(path))
		},
	})
	b.raw = rawcache.New(cfg.RawCacheBytes)
	b.M.Gauge("rawCache", b.raw.Stats)
	gate := edge.NewGate()
	work := budget.New(cfg.WorkMemBytes)
	b.M.Gauge("workMem", work.Stats)
	b.store = mirror.New(mirror.Options{
		Edge: b.edge, Raw: b.raw, Metrics: b.M, Log: b.Log,
		MaxRows: cfg.MirrorMaxRows, Freshness: cfg.Freshness, RawConcurrency: cfg.RawConcurrency,
	})

	imapRaw, err := net.Listen("tcp", cfg.IMAPAddr)
	if err != nil {
		return nil, err
	}
	smtpRaw, err := net.Listen("tcp", cfg.SMTPAddr)
	if err != nil {
		_ = imapRaw.Close()
		return nil, err
	}
	adminL, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		_ = imapRaw.Close()
		_ = smtpRaw.Close()
		return nil, err
	}
	b.adminL = adminL

	common := guard.Config{
		ProxyRequired:     cfg.ProxyRequired,
		TrustedPeers:      cfg.TrustedPeers,
		PreauthTimeout:    cfg.PreauthTimeout,
		PreauthMaxLine:    cfg.PreauthMaxLine,
		PreauthMaxBytes:   cfg.PreauthMaxBytes,
		PreauthMaxLiteral: cfg.PreauthMaxLiteral,
		TCPKeepAlive:      cfg.TCPKeepAlive,
		Log:               b.Log,
	}
	ig := common
	ig.Proto, ig.Mode = "imap", guard.ModeIMAP
	ig.MaxTotal, ig.MaxPerIP = cfg.IMAPMaxConns, cfg.IMAPMaxConnsPerIP
	ig.MaxLine, ig.MaxNonSyncLiteral = cfg.IMAPMaxLine, 4096
	ig.Metrics = &b.M.IMAP
	ig.Reject = func(reason string) string { return "* BYE " + reason + "\r\n" }
	b.imapL = guard.Listen(imapRaw, ig)

	sg := common
	sg.Proto, sg.Mode = "smtp", guard.ModeLines
	sg.MaxTotal, sg.MaxPerIP = cfg.SMTPMaxConns, cfg.SMTPMaxConnsPerIP
	sg.Metrics = &b.M.SMTP
	sg.Reject = func(reason string) string { return "421 4.7.0 " + reason + "\r\n" }
	b.smtpL = guard.Listen(smtpRaw, sg)
	b.M.Gauge("proxyPeers", func() map[string]int64 {
		out := b.imapL.ProxyPeers()
		for k, v := range b.smtpL.ProxyPeers() {
			out[k] += v
		}
		return out
	})

	b.imap = imapd.New(imapd.Config{
		MaxAuthFailures: cfg.MaxAuthFailures,
		AppendMaxBytes:  cfg.AppendMaxBytes,
		IdleKeepalive:   cfg.IdleKeepalive,
		SearchMaxDepth:  cfg.SearchMaxDepth,
		MaxSetRanges:    cfg.MaxSetRanges,
		RawTimeout:      cfg.EdgeLongTimeout + 5*time.Second,
		CallTimeout:     cfg.EdgeTimeout + 2*time.Second,
		FetchHook:       o.FetchHook,
	}, b.store, b.edge, work, gate, b.M, b.Log)
	b.smtp = smtpd.New(smtpd.Config{
		Domain:          cfg.SMTPDomain,
		MaxMessageBytes: cfg.SMTPMaxBytes,
		MaxRecipients:   cfg.SMTPMaxRecipients,
		MaxAuthFailures: cfg.MaxAuthFailures,
		CallTimeout:     cfg.EdgeTimeout + 2*time.Second,
		SubmitTimeout:   cfg.EdgeLongTimeout + 5*time.Second,
		ReadTimeout:     cfg.SMTPReadTimeout,
	}, b.store, b.edge, work, gate, b.M, b.Log)

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel

	b.wg.Add(2)
	go func() { defer b.wg.Done(); _ = b.imap.Serve(b.imapL) }()
	go func() { defer b.wg.Done(); _ = b.smtp.Serve(b.smtpL) }()
	if !o.NoPoller {
		b.wg.Add(1)
		go func() { defer b.wg.Done(); b.store.Run(ctx, cfg.SeqInterval) }()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if b.stopping.Load() {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", " ")
		_ = enc.Encode(b.M.Snapshot())
	})
	b.admin = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	b.wg.Add(1)
	go func() { defer b.wg.Done(); _ = b.admin.Serve(adminL) }()

	// Without PROXY_TRUSTED_CIDRS a PROXY header is believed from any peer
	// that reaches the mail ports. Fine for the first deploy (the networks are
	// read from /metrics, proxyPeers), not for operation: say so on every start.
	if cfg.ProxyRequired && len(cfg.TrustedPeers) == 0 {
		b.Log.Event("warning", logx.Code("proxy_peers_untrusted"))
	}
	b.Log.Event("started")
	return b, nil
}

// IMAPAddr is the address the IMAP listener is bound to.
func (b *Bridge) IMAPAddr() string { return b.imapL.Addr().String() }

// SMTPAddr is the address the SMTP listener is bound to.
func (b *Bridge) SMTPAddr() string { return b.smtpL.Addr().String() }

// AdminAddr is the address of the internal HTTP port.
func (b *Bridge) AdminAddr() string { return b.adminL.Addr().String() }

// SyncNow runs one poller pass and returns when it is done (tests).
func (b *Bridge) SyncNow(ctx context.Context) { b.store.PollOnce(ctx) }

// Shutdown stops accepting, tells open connections to go (IMAP: BYE, SMTP:
// 421) and closes everything. It returns after at most the configured
// grace period.
func (b *Bridge) Shutdown() {
	b.stopOnce.Do(func() {
		b.stopping.Store(true)
		b.Log.Event("stopping")
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = b.imapL.Close()
			_ = b.smtpL.Close()
			b.imap.Shutdown()
			for _, c := range b.smtpL.Conns() {
				c.WriteDirect("421 4.3.2 Service is shutting down\r\n")
			}
			b.smtp.Close()
			for _, c := range b.imapL.Conns() {
				_ = c.Close()
			}
			for _, c := range b.smtpL.Conns() {
				_ = c.Close()
			}
			b.cancel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = b.admin.Shutdown(ctx)
			cancel()
			b.wg.Wait()
		}()
		select {
		case <-done:
		case <-time.After(b.cfg.ShutdownGrace):
			for _, c := range b.imapL.Conns() {
				_ = c.Close()
			}
			for _, c := range b.smtpL.Conns() {
				_ = c.Close()
			}
			b.cancel()
			_ = b.admin.Close()
		}
		b.Log.Event("stopped")
	})
}
