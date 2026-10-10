// Package config reads the settings of the bridge from the environment.
// There is no other source: no file, no flags.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
)

// MinSecretChars is the least length of MAIL_BRIDGE_SECRET the edge accepts.
const MinSecretChars = 32

// Config is everything that can be set.
type Config struct {
	SiteURL string // CONVEX_SITE_URL
	Secret  string // MAIL_BRIDGE_SECRET

	IMAPAddr  string // IMAP_ADDR
	SMTPAddr  string // SMTP_ADDR
	AdminAddr string // ADMIN_ADDR (healthz, metrics)

	ProxyRequired bool           // PROXY_PROTOCOL=required|off
	TrustedPeers  []netip.Prefix // PROXY_TRUSTED_CIDRS

	IMAPMaxConns      int // IMAP_MAX_CONNS
	IMAPMaxConnsPerIP int // IMAP_MAX_CONNS_PER_IP
	SMTPMaxConns      int // SMTP_MAX_CONNS
	SMTPMaxConnsPerIP int // SMTP_MAX_CONNS_PER_IP

	PreauthTimeout    time.Duration // PREAUTH_TIMEOUT_SECONDS
	PreauthMaxLine    int           // PREAUTH_MAX_LINE_BYTES
	PreauthMaxBytes   int           // PREAUTH_MAX_BYTES
	PreauthMaxLiteral int64         // PREAUTH_MAX_LITERAL_BYTES
	MaxAuthFailures   int           // MAX_AUTH_FAILURES

	IMAPMaxLine    int    // IMAP_MAX_LINE_BYTES
	AppendMaxBytes uint32 // APPEND_MAX_BYTES
	SearchMaxDepth int    // SEARCH_MAX_DEPTH
	MaxSetRanges   int    // IMAP_MAX_SET_RANGES

	SMTPReadTimeout   time.Duration // SMTP_READ_TIMEOUT_SECONDS (idle, and one whole DATA)
	SMTPDomain        string        // SMTP_DOMAIN
	SMTPMaxBytes      int64         // SMTP_MAX_MESSAGE_BYTES
	SMTPMaxRecipients int           // SMTP_MAX_RECIPIENTS

	RawCacheBytes  int64 // RAW_CACHE_BYTES
	RawMaxBytes    int64 // RAW_MAX_BYTES
	RawConcurrency int   // RAW_FETCH_CONCURRENCY
	MirrorMaxRows  int64 // MIRROR_MAX_ENTRIES
	WorkMemBytes   int64 // WORK_MEM_BYTES

	EdgeTimeout     time.Duration // EDGE_TIMEOUT_MS (15 s in the contract)
	EdgeLongTimeout time.Duration // EDGE_LONG_TIMEOUT_MS (raw, submit: 60 s)

	SeqInterval   time.Duration // SEQ_INTERVAL_MS
	Freshness     time.Duration // SESSION_FRESH_SECONDS
	IdleKeepalive time.Duration // IDLE_KEEPALIVE_SECONDS (0: off)
	TCPKeepAlive  time.Duration // TCP_KEEPALIVE_SECONDS (0: off)
	ShutdownGrace time.Duration // SHUTDOWN_GRACE_SECONDS
}

type reader struct {
	get  func(string) string
	errs []string
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.get(key)); v != "" {
		return v
	}
	return def
}

func (r *reader) num(key string, def, min, max int64) int64 {
	v := strings.TrimSpace(r.get(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < min || n > max {
		r.errs = append(r.errs, fmt.Sprintf("%s must be a number between %d and %d", key, min, max))
		return def
	}
	return n
}

// Load reads the configuration. get is os.Getenv outside of tests. The
// returned error lists every problem; it never contains a value.
func Load(get func(string) string) (*Config, error) {
	r := &reader{get: get}
	c := &Config{}

	// The edge trims the secret and is switched off altogether (401 for
	// everything) if less than 32 characters are left. Starting with such a
	// secret could never work, so it is refused here.
	c.Secret = strings.TrimSpace(r.get("MAIL_BRIDGE_SECRET"))
	if c.Secret == "" {
		r.errs = append(r.errs, "MAIL_BRIDGE_SECRET is missing")
	} else if len(c.Secret) < MinSecretChars {
		r.errs = append(r.errs, "MAIL_BRIDGE_SECRET is too short (at least 32 characters)")
	} else if strings.ContainsAny(c.Secret, " \t\r\n") {
		r.errs = append(r.errs, "MAIL_BRIDGE_SECRET must not contain blanks")
	}
	if raw := r.get("CONVEX_SITE_URL"); strings.TrimSpace(raw) == "" {
		r.errs = append(r.errs, "CONVEX_SITE_URL is missing")
	} else if u, err := edge.CheckSiteURL(raw); err != nil {
		r.errs = append(r.errs, err.Error())
	} else {
		c.SiteURL = u
	}

	c.IMAPAddr = r.str("IMAP_ADDR", ":1143")
	c.SMTPAddr = r.str("SMTP_ADDR", ":1465")
	c.AdminAddr = r.str("ADMIN_ADDR", "127.0.0.1:8080")

	switch v := strings.ToLower(r.str("PROXY_PROTOCOL", "required")); v {
	case "required":
		c.ProxyRequired = true
	case "off":
	default:
		r.errs = append(r.errs, "PROXY_PROTOCOL must be required or off")
	}
	for _, part := range strings.Split(r.get("PROXY_TRUSTED_CIDRS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			r.errs = append(r.errs, "PROXY_TRUSTED_CIDRS must be a comma-separated list of CIDR ranges")
			break
		}
		c.TrustedPeers = append(c.TrustedPeers, p.Masked())
	}

	c.IMAPMaxConns = int(r.num("IMAP_MAX_CONNS", 1000, 1, 100000))
	c.IMAPMaxConnsPerIP = int(r.num("IMAP_MAX_CONNS_PER_IP", 30, 1, 100000))
	c.SMTPMaxConns = int(r.num("SMTP_MAX_CONNS", 200, 1, 100000))
	c.SMTPMaxConnsPerIP = int(r.num("SMTP_MAX_CONNS_PER_IP", 10, 1, 100000))

	c.PreauthTimeout = time.Duration(r.num("PREAUTH_TIMEOUT_SECONDS", 30, 1, 600)) * time.Second
	c.PreauthMaxLine = int(r.num("PREAUTH_MAX_LINE_BYTES", 2048, 256, 8192))
	c.PreauthMaxBytes = int(r.num("PREAUTH_MAX_BYTES", 16384, 1024, 1<<20))
	c.PreauthMaxLiteral = r.num("PREAUTH_MAX_LITERAL_BYTES", 128, 0, 1024)
	c.MaxAuthFailures = int(r.num("MAX_AUTH_FAILURES", 3, 1, 20))

	// The library reads at most 50 KiB per command; staying below makes the
	// guard decide, with a clean close.
	c.IMAPMaxLine = int(r.num("IMAP_MAX_LINE_BYTES", 32768, 1024, 49152))
	c.AppendMaxBytes = uint32(r.num("APPEND_MAX_BYTES", 6<<20, 1024, 64<<20))
	c.SearchMaxDepth = int(r.num("SEARCH_MAX_DEPTH", 16, 1, 64))
	c.MaxSetRanges = int(r.num("IMAP_MAX_SET_RANGES", 4096, 16, 65536))

	c.SMTPReadTimeout = time.Duration(r.num("SMTP_READ_TIMEOUT_SECONDS", 300, 10, 3600)) * time.Second
	c.SMTPDomain = r.str("SMTP_DOMAIN", "mail-bridge")
	c.SMTPMaxBytes = r.num("SMTP_MAX_MESSAGE_BYTES", 5<<20, 1024, 64<<20)
	c.SMTPMaxRecipients = int(r.num("SMTP_MAX_RECIPIENTS", 50, 1, 1000))

	c.RawCacheBytes = r.num("RAW_CACHE_BYTES", 64<<20, 1<<20, 4<<30)
	c.RawMaxBytes = r.num("RAW_MAX_BYTES", 32<<20, 1<<20, 256<<20)
	c.RawConcurrency = int(r.num("RAW_FETCH_CONCURRENCY", 6, 1, 64))
	c.MirrorMaxRows = r.num("MIRROR_MAX_ENTRIES", 400000, 1000, 50000000)
	c.WorkMemBytes = r.num("WORK_MEM_BYTES", 128<<20, 32<<20, 16<<30)

	c.EdgeTimeout = time.Duration(r.num("EDGE_TIMEOUT_MS", 15000, 50, 120000)) * time.Millisecond
	c.EdgeLongTimeout = time.Duration(r.num("EDGE_LONG_TIMEOUT_MS", 60000, 50, 300000)) * time.Millisecond
	c.SeqInterval = time.Duration(r.num("SEQ_INTERVAL_MS", 2000, 100, 60000)) * time.Millisecond
	c.Freshness = time.Duration(r.num("SESSION_FRESH_SECONDS", 10, 1, 600)) * time.Second
	c.IdleKeepalive = time.Duration(r.num("IDLE_KEEPALIVE_SECONDS", 240, 0, 1700)) * time.Second
	c.TCPKeepAlive = time.Duration(r.num("TCP_KEEPALIVE_SECONDS", 60, 0, 3600)) * time.Second
	c.ShutdownGrace = time.Duration(r.num("SHUTDOWN_GRACE_SECONDS", 10, 1, 120)) * time.Second

	// One mailbox may hold a quarter of the work memory; the largest single
	// thing it can hold has to fit into that.
	largest := c.RawMaxBytes
	if int64(c.AppendMaxBytes) > largest {
		largest = int64(c.AppendMaxBytes)
	}
	if c.SMTPMaxBytes > largest {
		largest = c.SMTPMaxBytes
	}
	if c.WorkMemBytes < 4*largest {
		r.errs = append(r.errs, "WORK_MEM_BYTES must be at least four times the largest of RAW_MAX_BYTES, APPEND_MAX_BYTES and SMTP_MAX_MESSAGE_BYTES")
	}
	if c.Freshness < 2*c.SeqInterval {
		// Otherwise connections would lose their confirmation between two
		// polls and every command would ask the edge again.
		c.Freshness = 2*c.SeqInterval + time.Second
	}
	if len(r.errs) > 0 {
		return nil, errors.New(strings.Join(r.errs, "; "))
	}
	return c, nil
}
