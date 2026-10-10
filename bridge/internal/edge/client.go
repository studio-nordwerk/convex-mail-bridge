// Package edge is the client for the HTTP edge in Convex described in
// docs/edge-contract.md (version 1.1 with the
// addendum after the reviews of the edge).
//
// The edge refuses every body that carries a field it does not know (400
// "request") and every body over 64 KiB (413). So each call builds its body
// from a struct with exactly the fields of the contract, and nothing leaves
// this package that is larger than the edge takes (see limits.go).
//
// Every call carries the shared secret; every call except login and seq also
// carries the session of the connection it is made for. The client never
// logs and never returns texts that contain secrets: errors carry the path,
// the HTTP status and the short error code of the edge only.
package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
)

// Folder names of stage 1. There are exactly these two.
const (
	FolderInbox = "INBOX"
	FolderSent  = "Sent"
)

// maxJSONBody bounds what the client reads as a JSON answer.
const maxJSONBody = 8 << 20

// Error is an answer of the edge that is not a success, or a failure to get
// an answer at all (Status 0).
type Error struct {
	Path       string
	Status     int    // HTTP status, 0 for transport errors and timeouts
	Code       string // "auth", "session", "throttled", "preparing", "folder", "gone", "resync", "unknown", ...
	Message    string // short text of the edge, sanitized; only passed on for /submit
	RetryAfter int    // seconds, for "throttled"
	SMTPCode   int    // for /submit
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("edge %s: no answer (%s)", e.Path, e.Code)
	}
	return fmt.Sprintf("edge %s: %d %s", e.Path, e.Status, e.Code)
}

// CodeOf returns the error code of err if it is an *Error, else "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsSession reports whether the edge rejected the session of the call
// (invalid, expired, device password revoked, mailbox switched off). The
// bridge then ends exactly the connection the session belongs to.
func IsSession(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized && e.Code == "session"
}

// Is reports whether err is an edge answer with the given status and code.
func Is(err error, status int, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status && e.Code == code
}

// Caller identifies on whose behalf a call is made: the session of one
// connection and the hash of its mailbox (for the per-mailbox counters).
type Caller struct {
	Session string
	MB      string
}

// Client talks to the edge.
type Client struct {
	base        string
	secret      string
	hc          *http.Client
	m           *metrics.Registry
	callTimeout time.Duration // 15 s
	longTimeout time.Duration // raw and submit: 60 s
	maxRaw      int64
	onFault     func(path string, status int, code string)
}

// Options configures a Client.
type Options struct {
	SiteURL     string
	Secret      string
	Metrics     *metrics.Registry
	CallTimeout time.Duration
	LongTimeout time.Duration
	MaxRawBytes int64
	Transport   http.RoundTripper // tests
	// OnFault is called for answers that point to a fault between bridge
	// and edge (400, 413, 500) and for calls that were not sent because of
	// their size or form. It gets the path, the status (0: not sent) and
	// the error code, nothing else.
	OnFault func(path string, status int, code string)
}

// CheckSiteURL validates CONVEX_SITE_URL. Plain http is accepted for
// loopback hosts only: the secret and device passwords travel on this link.
// (A build with -tags localedge, for the local probe against an edge on the
// same machine, accepts it for any host; see PlainHTTPAnyHost.)
func CheckSiteURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", errors.New("CONVEX_SITE_URL is not a URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("CONVEX_SITE_URL must not carry credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !PlainHTTPAnyHost && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("CONVEX_SITE_URL must be https (http is allowed for loopback only)")
		}
	default:
		return "", errors.New("CONVEX_SITE_URL must be https")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// New returns a client. SiteURL must have passed CheckSiteURL.
func New(o Options) *Client {
	if o.CallTimeout <= 0 {
		o.CallTimeout = 15 * time.Second
	}
	if o.LongTimeout <= 0 {
		o.LongTimeout = 60 * time.Second
	}
	if o.MaxRawBytes <= 0 {
		o.MaxRawBytes = 32 << 20
	}
	if o.Metrics == nil {
		o.Metrics = metrics.New()
	}
	tr := o.Transport
	if tr == nil {
		tr = &http.Transport{
			Proxy:                 nil, // never through an environment proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return &Client{
		base:   strings.TrimRight(o.SiteURL, "/") + "/bridge/v1",
		secret: o.Secret,
		hc: &http.Client{
			Transport: tr,
			// A redirect would carry the secret to another URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		m:           o.Metrics,
		callTimeout: o.CallTimeout,
		longTimeout: o.LongTimeout,
		maxRaw:      o.MaxRawBytes,
		onFault:     o.OnFault,
	}
}

// notSent notes a call the client refuses to make: its form or size is not
// what the edge takes, so the edge would only answer 400 or 413.
func (c *Client) notSent(path, code string) *Error {
	if code == "oversize" {
		c.m.EdgeOversize.Add(1)
	} else {
		c.m.EdgeNotSent.Add(1)
	}
	if c.onFault != nil {
		c.onFault(path, 0, code)
	}
	return &Error{Path: path, Code: code}
}

// IsFault reports whether a status of the edge points to a fault between
// bridge and edge rather than to the mail program or the mailbox.
func IsFault(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge ||
		status == http.StatusInternalServerError
}

type errorBody struct {
	Error             string `json:"error"`
	Message           string `json:"message"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	SMTPCode          int    `json:"smtpCode"`
}

type request struct {
	path        string
	caller      Caller
	body        []byte
	contentType string
	header      map[string]string
	timeout     time.Duration
	maxBody     int64
	wantRaw     bool // answer is message/rfc822, not JSON
}

// do performs one call. On success it returns the body. Everything else is
// an *Error.
func (c *Client) do(ctx context.Context, rq request) (body []byte, err error) {
	start := time.Now()
	var in int64
	status := 0       // 0: no answer (transport, timeout, or not sent at all)
	designed := false // a rejection of /submit that names its SMTP answer
	defer func() {
		c.m.ObserveEdge(rq.path, status, time.Since(start), int64(len(rq.body)), in, err != nil)
		// 400, 413 and 500 point to a fault between bridge and edge,
		// whatever became of their body.
		if err != nil && IsFault(status) && !designed {
			c.m.EdgeFault(status)
			if c.onFault != nil {
				c.onFault(rq.path, status, CodeOf(err))
			}
		}
		if rq.caller.MB != "" {
			c.m.Mailbox(rq.caller.MB, func(m *metrics.Mailbox) {
				m.EdgeCalls++
				m.EdgeBytesIn += in
			})
		}
	}()

	if rq.contentType == "application/json" && len(rq.body) > MaxRequestBytes {
		// The edge would answer 413. This is a fault of the bridge, never
		// of the mail program: do not send it, count it.
		return nil, c.notSent(rq.path, "oversize")
	}

	ctx, cancel := context.WithTimeout(ctx, rq.timeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, c.base+rq.path, bytes.NewReader(rq.body))
	if rerr != nil {
		return nil, &Error{Path: rq.path, Code: "request"}
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("Content-Type", rq.contentType)
	if rq.caller.Session != "" {
		req.Header.Set("X-Bridge-Session", rq.caller.Session)
	}
	for k, v := range rq.header {
		req.Header.Set(k, v)
	}

	resp, derr := c.hc.Do(req)
	if derr != nil {
		code := "transport"
		if ctx.Err() != nil || isTimeout(derr) {
			code = "timeout"
		}
		// The underlying error text can contain the URL but no secret; it is
		// still not passed on: callers only need the class.
		return nil, &Error{Path: rq.path, Code: code}
	}
	defer resp.Body.Close()
	status = resp.StatusCode

	limit := rq.maxBody
	if resp.StatusCode != http.StatusOK {
		limit = 64 << 10
	}
	data, rerr2 := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	in = int64(len(data))
	if rerr2 != nil {
		code := "read"
		if ctx.Err() != nil || isTimeout(rerr2) {
			code = "timeout"
		}
		// The status did arrive; only its body did not.
		return nil, &Error{Path: rq.path, Status: resp.StatusCode, Code: code}
	}
	if int64(len(data)) > limit {
		return nil, &Error{Path: rq.path, Status: resp.StatusCode, Code: "too_large"}
	}

	if resp.StatusCode == http.StatusOK {
		if rq.wantRaw {
			// A truncated answer must not pass as a mail: the edge sets
			// Content-Length, and a body cut short by a size limit ends
			// without an error on the wire.
			if resp.ContentLength >= 0 && resp.ContentLength != int64(len(data)) {
				return nil, &Error{Path: rq.path, Status: resp.StatusCode, Code: "truncated"}
			}
		}
		return data, nil
	}

	e := &Error{Path: rq.path, Status: resp.StatusCode}
	var eb errorBody
	if len(data) > 0 && json.Unmarshal(data, &eb) == nil {
		e.Code = cleanCode(eb.Error)
		e.Message = CleanText(eb.Message, 200)
		e.RetryAfter = eb.RetryAfterSeconds
		e.SMTPCode = eb.SMTPCode
	}
	if e.RetryAfter <= 0 && resp.StatusCode == http.StatusTooManyRequests {
		// The header says the same as the body; either is enough.
		if n, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && n > 0 {
			e.RetryAfter = n
		}
	}
	if e.Code == "" {
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			// 401 with an empty body: the shared secret was rejected.
			e.Code = "secret"
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			e.Code = "redirect"
		default:
			e.Code = "http"
		}
	}
	// A rejection of /submit that names its SMTP answer is the edge
	// speaking to the mail program (too large, wrong sender), not a fault.
	designed = rq.path == "/submit" && e.SMTPCode != 0 && resp.StatusCode != http.StatusInternalServerError
	return nil, e
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded)
}

func cleanCode(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "other"
		}
	}
	return s
}

// CleanText makes a text of the edge safe to show in a protocol answer:
// printable ASCII only, one line, bounded length.
func CleanText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')
		} else {
			b.WriteByte('?')
		}
	}
	return strings.TrimSpace(b.String())
}

// encode writes v as JSON the short way: "<" and "&" as they are, not as
// six bytes each, so that the size of a body can be reckoned from its parts.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (c *Client) json(ctx context.Context, path string, caller Caller, in, out any) error {
	body, err := encode(in)
	if err != nil {
		return &Error{Path: path, Code: "request"}
	}
	data, err := c.do(ctx, request{
		path: path, caller: caller, body: body, contentType: "application/json",
		timeout: c.callTimeout, maxBody: maxJSONBody,
	})
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Path: path, Status: http.StatusOK, Code: "decode"}
	}
	return nil
}

// ---- login -----------------------------------------------------------

// Session is the answer to a successful login.
type Session struct {
	Token     string
	ExpiresAt time.Time
	// Mailbox is the stable, opaque identifier of the mailbox. Mirror, raw
	// cache and counters of the bridge hang on it, not on the address.
	Mailbox string
}

// validID: an identifier that can serve as a key (not empty, bounded, no
// control characters).
func validID(s string, max int) bool {
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

// validToken: a session travels in a header and, for /seq, in a list of up
// to 200 in a body of at most 64 KiB. So it is visible ASCII without quote
// and backslash (one byte each in JSON) and of bounded length.
func validToken(s string) bool {
	if len(s) < 16 || len(s) > MaxSessionChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// ClientIP writes a client address the only way the edge takes it: a bare
// IPv4 or IPv6 address, no port, no brackets, no zone. An address that is
// not valid becomes 0.0.0.0, which the edge counts as one client.
func ClientIP(a netip.Addr) string {
	if !a.IsValid() {
		return "0.0.0.0"
	}
	return a.Unmap().WithZone("").String()
}

type loginRequest struct {
	Address  string `json:"address"`
	Password string `json:"password"`
	ClientIP string `json:"clientIp"`
}

// Login exchanges address and device password for a session. clientIP is the
// address from the PROXY header; the edge throttles failed attempts per
// address and client.
func (c *Client) Login(ctx context.Context, address, password string, clientIP netip.Addr) (*Session, error) {
	var out struct {
		Session   string `json:"session"`
		ExpiresAt int64  `json:"expiresAt"` // milliseconds since 1970
		Mailbox   string `json:"mailbox"`
	}
	err := c.json(ctx, "/login", Caller{}, loginRequest{
		Address: address, Password: password, ClientIP: ClientIP(clientIP),
	}, &out)
	if err != nil {
		return nil, err
	}
	// Without a usable mailbox identifier there is nothing to key a mirror
	// on: no login rather than a guess.
	if !validToken(out.Session) || !validID(out.Mailbox, 256) {
		return nil, &Error{Path: "/login", Status: http.StatusOK, Code: "decode"}
	}
	s := &Session{Token: out.Session, Mailbox: out.Mailbox}
	if out.ExpiresAt > 0 {
		s.ExpiresAt = time.UnixMilli(out.ExpiresAt)
	}
	return s, nil
}

// ---- folders -----------------------------------------------------------

// Folder is one folder of a mailbox as the edge counts it.
type Folder struct {
	Name          string `json:"name"`
	SpecialUse    string `json:"specialUse"`
	UIDValidity   uint32 `json:"uidValidity"`
	UIDNext       uint32 `json:"uidNext"`
	HighestModseq int64  `json:"highestModseq"`
	Exists        uint32 `json:"exists"`
	Unseen        uint32 `json:"unseen"`
}

// Folders returns the counters of both folders.
func (c *Client) Folders(ctx context.Context, caller Caller) ([]Folder, error) {
	var out struct {
		Folders []Folder `json:"folders"`
	}
	// exactly {}: the edge takes no field here
	if err := c.json(ctx, "/folders", caller, struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Folders, nil
}

// ---- entries and changes -----------------------------------------------

// Row is one numbered mail of a folder.
type Row struct {
	UID          uint32   `json:"uid"`
	Flags        []string `json:"flags"`
	Modseq       int64    `json:"modseq"`
	Size         int64    `json:"size"`
	InternalDate int64    `json:"internalDate"`
	// Live is only set by /changes: false marks a row that left the folder.
	Live *bool `json:"live,omitempty"`
}

// IsLive reports whether the row is still in the folder.
func (r *Row) IsLive() bool { return r.Live == nil || *r.Live }

// Page is one page of /entries or /changes.
type Page struct {
	Rows          []Row  `json:"rows"`
	More          bool   `json:"more"`
	UIDValidity   uint32 `json:"uidValidity"`
	UIDNext       uint32 `json:"uidNext"`
	HighestModseq int64  `json:"highestModseq"`
}

type entriesRequest struct {
	Folder   string  `json:"folder"`
	AfterUID *uint32 `json:"afterUid,omitempty"`
	Limit    int     `json:"limit"`
}

type changesRequest struct {
	Folder      string `json:"folder"`
	SinceModseq int64  `json:"sinceModseq"`
	Limit       int    `json:"limit"`
}

// Entries returns living rows with uid > afterUID, ascending.
func (c *Client) Entries(ctx context.Context, caller Caller, folder string, afterUID uint32, limit int) (*Page, error) {
	in := entriesRequest{Folder: folder, Limit: limit}
	if afterUID > 0 {
		in.AfterUID = &afterUID
	}
	var out Page
	if err := c.json(ctx, "/entries", caller, in, &out); err != nil {
		return nil, err
	}
	var last uint32
	for i := range out.Rows {
		r := &out.Rows[i]
		if r.UID == 0 || r.UID <= last || r.UID <= afterUID || r.Size < 0 {
			return nil, &Error{Path: "/entries", Status: http.StatusOK, Code: "decode"}
		}
		last = r.UID
		r.Live = nil
	}
	if out.UIDValidity == 0 || out.UIDNext == 0 || out.UIDNext <= last {
		return nil, &Error{Path: "/entries", Status: http.StatusOK, Code: "decode"}
	}
	return &out, nil
}

// Changes returns rows changed after sinceModseq, ascending by modseq, each
// row at most once. A 410 "resync" means the mirror cannot be continued: it
// is older than the edge keeps removed rows, or it is a state the folder
// never had (above its highest one). Either way it has to be dropped.
func (c *Client) Changes(ctx context.Context, caller Caller, folder string, sinceModseq int64, limit int) (*Page, error) {
	var out Page
	err := c.json(ctx, "/changes", caller, changesRequest{
		Folder: folder, SinceModseq: sinceModseq, Limit: limit,
	}, &out)
	if err != nil {
		return nil, err
	}
	last := sinceModseq
	for i := range out.Rows {
		r := &out.Rows[i]
		if r.UID == 0 || r.Modseq <= last || r.Size < 0 {
			return nil, &Error{Path: "/changes", Status: http.StatusOK, Code: "decode"}
		}
		last = r.Modseq
		if r.IsLive() && r.UID >= out.UIDNext {
			// a living mail at or beyond the next number cannot be
			return nil, &Error{Path: "/changes", Status: http.StatusOK, Code: "decode"}
		}
	}
	if out.UIDValidity == 0 || out.UIDNext == 0 {
		return nil, &Error{Path: "/changes", Status: http.StatusOK, Code: "decode"}
	}
	return &out, nil
}

// IsResync reports a 410 of /changes.
func IsResync(err error) bool { return Is(err, http.StatusGone, "resync") }

// IsUIDValidity reports a 409 of /raw: the folder was renumbered.
func IsUIDValidity(err error) bool { return Is(err, http.StatusConflict, "uidvalidity") }

// IsNoFolder reports a 404 "folder": the edge does not have this folder for
// the mailbox of the session.
func IsNoFolder(err error) bool { return Is(err, http.StatusNotFound, "folder") }

// ---- raw ---------------------------------------------------------------

type rawRequest struct {
	Folder      string `json:"folder"`
	UID         uint32 `json:"uid"`
	UIDValidity uint32 `json:"uidValidity"`
}

// Raw returns the bytes of one mail. uidValidity is the numbering the uid
// belongs to; if the folder has another one by now the edge answers 409
// "uidvalidity" (see IsUIDValidity) instead of some other mail.
func (c *Client) Raw(ctx context.Context, caller Caller, folder string, uidValidity, uid uint32) ([]byte, error) {
	if uid == 0 || uidValidity == 0 {
		return nil, c.notSent("/raw", "request") // the edge takes numbers from 1
	}
	body, err := encode(rawRequest{Folder: folder, UID: uid, UIDValidity: uidValidity})
	if err != nil {
		return nil, c.notSent("/raw", "request")
	}
	return c.do(ctx, request{
		path: "/raw", caller: caller, body: body, contentType: "application/json",
		timeout: c.longTimeout, maxBody: c.maxRaw, wantRaw: true,
	})
}

// ---- store and expunge ---------------------------------------------------

// StoreRow is a row whose flags were actually changed by a store.
type StoreRow struct {
	UID    uint32   `json:"uid"`
	Flags  []string `json:"flags"`
	Modseq int64    `json:"modseq"`
}

type storeRequest struct {
	Folder string   `json:"folder"`
	UIDs   []uint32 `json:"uids"`
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

type expungeRequest struct {
	Folder string   `json:"folder"`
	UIDs   []uint32 `json:"uids"`
}

// checkUIDs: what the edge takes as a list of numbers.
func checkUIDs(uids []uint32) bool {
	if len(uids) > MaxStoreUIDs {
		return false
	}
	for _, u := range uids {
		if u == 0 {
			return false
		}
	}
	return true
}

// checkFlags: what the edge takes as a list of flags.
func checkFlags(flags []string) bool {
	if len(flags) > MaxFlagsPerList {
		return false
	}
	for _, f := range flags {
		if f == "" || len(f) > MaxFlagChars {
			return false
		}
	}
	return true
}

// Store adds and removes flags. At most MaxStoreUIDs uids and
// MaxFlagsPerList flags per list in one call; callers split larger jobs. A
// call outside these limits is not sent.
func (c *Client) Store(ctx context.Context, caller Caller, folder string, uids []uint32, add, remove []string) ([]StoreRow, error) {
	if add == nil {
		add = []string{}
	}
	if remove == nil {
		remove = []string{}
	}
	if uids == nil {
		uids = []uint32{}
	}
	if !checkUIDs(uids) || !checkFlags(add) || !checkFlags(remove) {
		return nil, c.notSent("/store", "request")
	}
	var out struct {
		Rows []StoreRow `json:"rows"`
	}
	err := c.json(ctx, "/store", caller, storeRequest{Folder: folder, UIDs: uids, Add: add, Remove: remove}, &out)
	if err != nil {
		return nil, err
	}
	return out.Rows, nil
}

// Expunge hides mails from the mail program. It returns the uids that were
// actually hidden. At most MaxStoreUIDs uids per call.
func (c *Client) Expunge(ctx context.Context, caller Caller, folder string, uids []uint32) ([]uint32, error) {
	if uids == nil {
		uids = []uint32{}
	}
	if !checkUIDs(uids) {
		return nil, c.notSent("/expunge", "request")
	}
	var out struct {
		UIDs []uint32 `json:"uids"`
	}
	err := c.json(ctx, "/expunge", caller, expungeRequest{Folder: folder, UIDs: uids}, &out)
	if err != nil {
		return nil, err
	}
	return out.UIDs, nil
}

// ---- append --------------------------------------------------------------

// Appended is the existing sent mail an APPEND refers to.
type Appended struct {
	UID         uint32 `json:"uid"`
	UIDValidity uint32 `json:"uidValidity"`
}

type appendRequest struct {
	Folder    string `json:"folder"`
	MessageID string `json:"messageId"`
	Digest    string `json:"digest"`
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Append asks whether a mail the mail program wants to file in Sent is a
// mail that was sent through us. 409 "unknown" if not. messageID goes as it
// stands in the header (the edge normalizes it); the digest is only noted
// by the edge, not compared.
func (c *Client) Append(ctx context.Context, caller Caller, messageID, digest string) (*Appended, error) {
	if !AppendableMessageID(messageID) || !isHexDigest(digest) {
		return nil, c.notSent("/append", "request")
	}
	var out Appended
	err := c.json(ctx, "/append", caller, appendRequest{
		Folder: FolderSent, MessageID: messageID, Digest: digest,
	}, &out)
	if err != nil {
		return nil, err
	}
	if out.UID == 0 || out.UIDValidity == 0 {
		return nil, &Error{Path: "/append", Status: http.StatusOK, Code: "decode"}
	}
	return &out, nil
}

// ---- seq -----------------------------------------------------------------

// SeqResult is the answer for one session of a /seq call.
type SeqResult struct {
	OK  bool  `json:"ok"`
	Seq int64 `json:"seq"`
}

type seqRequest struct {
	Sessions []string `json:"sessions"`
}

// Seq asks for the change counters of the mailboxes behind the given
// sessions (at most MaxSeqSessions). Sessions the edge no longer accepts
// come back with OK false.
func (c *Client) Seq(ctx context.Context, sessions []string) ([]SeqResult, error) {
	var out struct {
		Results []SeqResult `json:"results"`
	}
	if len(sessions) > MaxSeqSessions {
		return nil, c.notSent("/seq", "request")
	}
	if sessions == nil {
		sessions = []string{}
	}
	if err := c.json(ctx, "/seq", Caller{}, seqRequest{Sessions: sessions}, &out); err != nil {
		return nil, err
	}
	if len(out.Results) != len(sessions) {
		return nil, &Error{Path: "/seq", Status: http.StatusOK, Code: "decode"}
	}
	return out.Results, nil
}

// ---- submit --------------------------------------------------------------

// Submitted is the answer to a submission. Both states are a success for
// the mail program.
type Submitted struct {
	Status string `json:"status"` // "sent" or "unknown"
	UID    uint32 `json:"uid"`
}

// Submit hands a mail from SMTP to the edge. from and to are the envelope.
func (c *Client) Submit(ctx context.Context, caller Caller, from string, to []string, raw []byte) (*Submitted, error) {
	toJSON, err := json.Marshal(to)
	if err != nil {
		return nil, &Error{Path: "/submit", Code: "request"}
	}
	data, err := c.do(ctx, request{
		path: "/submit", caller: caller, body: raw, contentType: "message/rfc822",
		header:  map[string]string{"X-Envelope-From": from, "X-Envelope-To": string(toJSON)},
		timeout: c.longTimeout, maxBody: 64 << 10,
	})
	if err != nil {
		return nil, err
	}
	var out Submitted
	if json.Unmarshal(data, &out) != nil || (out.Status != "sent" && out.Status != "unknown") {
		return nil, &Error{Path: "/submit", Status: http.StatusOK, Code: "decode"}
	}
	return &out, nil
}
