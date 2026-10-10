package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/metrics"
)

// recorder is a transport that keeps what was sent and answers what the
// test says.
type recorder struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
	answer func(path string) (status int, header map[string]string, body string)
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	data, _ := io.ReadAll(req.Body)
	path := strings.TrimPrefix(req.URL.Path, "/bridge/v1")
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.bodies = append(r.bodies, data)
	r.mu.Unlock()
	status, header, body := 200, map[string]string(nil), "{}"
	if r.answer != nil {
		status, header, body = r.answer(path)
	}
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status, Header: h, ContentLength: int64(len(body)),
		Body: io.NopCloser(strings.NewReader(body)), Request: req,
	}, nil
}

func (r *recorder) last(t *testing.T) (string, []byte) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.paths) == 0 {
		t.Fatal("nothing was sent")
	}
	return r.paths[len(r.paths)-1], r.bodies[len(r.bodies)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.paths)
}

func newTestClient(rec *recorder) (*Client, *metrics.Registry) {
	m := metrics.New()
	return New(Options{SiteURL: "https://edge.test", Secret: strings.Repeat("s", 40), Metrics: m, Transport: rec}), m
}

func keysOf(t *testing.T, body []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The edge refuses a body with any field it does not know. This is the
// table of the contract: for every call exactly these fields, no others.
func TestRequestsCarryExactlyTheFieldsOfTheContract(t *testing.T) {
	rec := &recorder{}
	c, _ := newTestClient(rec)
	ctx := context.Background()
	caller := Caller{Session: strings.Repeat("t", 43), MB: "h"}
	digest := strings.Repeat("ab", 32)

	cases := []struct {
		name string
		call func()
		path string
		want []string
		raw  string // the whole body, where it is fixed
	}{
		{"login", func() { _, _ = c.Login(ctx, "a@x.test", "pw", netip.MustParseAddr("192.0.2.7")) }, "/login",
			[]string{"address", "clientIp", "password"}, `{"address":"a@x.test","password":"pw","clientIp":"192.0.2.7"}`},
		{"folders", func() { _, _ = c.Folders(ctx, caller) }, "/folders", []string{}, `{}`},
		{"entries from the start", func() { _, _ = c.Entries(ctx, caller, "INBOX", 0, 2000) }, "/entries",
			[]string{"folder", "limit"}, `{"folder":"INBOX","limit":2000}`},
		{"entries after a uid", func() { _, _ = c.Entries(ctx, caller, "INBOX", 7, 10) }, "/entries",
			[]string{"afterUid", "folder", "limit"}, `{"folder":"INBOX","afterUid":7,"limit":10}`},
		{"changes", func() { _, _ = c.Changes(ctx, caller, "Sent", 0, 5) }, "/changes",
			[]string{"folder", "limit", "sinceModseq"}, `{"folder":"Sent","sinceModseq":0,"limit":5}`},
		{"raw", func() { _, _ = c.Raw(ctx, caller, "INBOX", 9, 3) }, "/raw",
			[]string{"folder", "uid", "uidValidity"}, `{"folder":"INBOX","uid":3,"uidValidity":9}`},
		{"store", func() { _, _ = c.Store(ctx, caller, "INBOX", []uint32{1, 2}, []string{`\Seen`}, nil) }, "/store",
			[]string{"add", "folder", "remove", "uids"}, `{"folder":"INBOX","uids":[1,2],"add":["\\Seen"],"remove":[]}`},
		{"store nothing", func() { _, _ = c.Store(ctx, caller, "INBOX", nil, nil, nil) }, "/store",
			[]string{"add", "folder", "remove", "uids"}, `{"folder":"INBOX","uids":[],"add":[],"remove":[]}`},
		{"expunge", func() { _, _ = c.Expunge(ctx, caller, "INBOX", []uint32{4}) }, "/expunge",
			[]string{"folder", "uids"}, `{"folder":"INBOX","uids":[4]}`},
		{"append", func() { _, _ = c.Append(ctx, caller, "<id@x.test>", digest) }, "/append",
			[]string{"digest", "folder", "messageId"}, `{"folder":"Sent","messageId":"<id@x.test>","digest":"` + digest + `"}`},
		{"seq", func() { _, _ = c.Seq(ctx, []string{"a", "b"}) }, "/seq", []string{"sessions"}, `{"sessions":["a","b"]}`},
		{"seq none", func() { _, _ = c.Seq(ctx, nil) }, "/seq", []string{"sessions"}, `{"sessions":[]}`},
	}
	for _, tc := range cases {
		before := rec.count()
		tc.call()
		if rec.count() != before+1 {
			t.Fatalf("%s: %d calls instead of one", tc.name, rec.count()-before)
		}
		path, body := rec.last(t)
		if path != tc.path {
			t.Fatalf("%s: path %q", tc.name, path)
		}
		if got := keysOf(t, body); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: fields %v, want %v", tc.name, got, tc.want)
		}
		if string(body) != tc.raw {
			t.Fatalf("%s: body %s, want %s", tc.name, body, tc.raw)
		}
	}
}

// The edge answers a body over 64 KiB with 413. The largest bodies the
// client can produce, reckoned: 500 numbers of ten digits with two full
// lists of the longest flags, and 200 sessions of the greatest length.
func TestLargestBodiesStayUnderTheLimitOfTheEdge(t *testing.T) {
	rec := &recorder{}
	c, m := newTestClient(rec)
	ctx := context.Background()
	caller := Caller{Session: strings.Repeat("t", 43)}

	uids := make([]uint32, MaxStoreUIDs)
	for i := range uids {
		uids[i] = 4294967295 - uint32(i)
	}
	// "<" and "&" are atom characters, and the usual JSON writer of Go makes
	// six bytes of each.
	add := make([]string, MaxFlagsPerList)
	remove := make([]string, MaxFlagsPerList)
	for i := range add {
		add[i] = strings.Repeat("<", MaxFlagChars)
		remove[i] = strings.Repeat("&", MaxFlagChars)
	}
	if _, err := c.Store(ctx, caller, "INBOX", uids, add, remove); err != nil {
		t.Fatal(err)
	}
	_, body := rec.last(t)
	if len(body) > MaxRequestBytes {
		t.Fatalf("/store: %d bytes", len(body))
	}
	storeBytes := len(body)
	if strings.Contains(string(body), `\`+"u003c") || !strings.Contains(string(body), `"<<<`) {
		t.Fatal("/store: flags are written with HTML escapes")
	}

	if _, err := c.Expunge(ctx, caller, "INBOX", uids); err != nil {
		t.Fatal(err)
	}
	_, body = rec.last(t)
	if len(body) > MaxRequestBytes {
		t.Fatalf("/expunge: %d bytes", len(body))
	}
	expungeBytes := len(body)

	sessions := make([]string, MaxSeqSessions)
	for i := range sessions {
		sessions[i] = strings.Repeat("~", MaxSessionChars)
		if !validToken(sessions[i]) {
			t.Fatal("the longest session is not a valid one")
		}
	}
	rec.answer = func(string) (int, map[string]string, string) {
		return 200, nil, `{"results":[` + strings.TrimSuffix(strings.Repeat(`{"ok":false},`, MaxSeqSessions), ",") + `]}`
	}
	if _, err := c.Seq(ctx, sessions); err != nil {
		t.Fatal(err)
	}
	_, body = rec.last(t)
	if len(body) > MaxRequestBytes {
		t.Fatalf("/seq: %d bytes", len(body))
	}
	t.Logf("largest bodies: /store %d, /expunge %d, /seq %d of %d bytes", storeBytes, expungeBytes, len(body), MaxRequestBytes)
	if n := m.EdgeOversize.Load() + m.EdgeNotSent.Load(); n != 0 {
		t.Fatalf("%d calls were not sent", n)
	}
}

// What is outside the limits of the edge is not sent at all, and counted.
func TestCallsOutsideTheLimitsAreNotSent(t *testing.T) {
	rec := &recorder{}
	var faults []string
	m := metrics.New()
	c := New(Options{SiteURL: "https://edge.test", Secret: strings.Repeat("s", 40), Metrics: m, Transport: rec,
		OnFault: func(path string, status int, code string) { faults = append(faults, path+" "+code) }})
	ctx := context.Background()
	caller := Caller{Session: strings.Repeat("t", 43)}
	digest := strings.Repeat("ab", 32)

	many := make([]uint32, MaxStoreUIDs+1)
	for i := range many {
		many[i] = uint32(i + 1)
	}
	flags := make([]string, MaxFlagsPerList+1)
	for i := range flags {
		flags[i] = "k"
	}
	calls := map[string]func() error{
		"store too many uids":  func() error { _, err := c.Store(ctx, caller, "INBOX", many, []string{"k"}, nil); return err },
		"store uid 0":          func() error { _, err := c.Store(ctx, caller, "INBOX", []uint32{0}, []string{"k"}, nil); return err },
		"store too many flags": func() error { _, err := c.Store(ctx, caller, "INBOX", []uint32{1}, flags, nil); return err },
		"store long flag": func() error {
			_, err := c.Store(ctx, caller, "INBOX", []uint32{1}, nil, []string{strings.Repeat("k", MaxFlagChars+1)})
			return err
		},
		"store empty flag":      func() error { _, err := c.Store(ctx, caller, "INBOX", []uint32{1}, []string{""}, nil); return err },
		"expunge too many uids": func() error { _, err := c.Expunge(ctx, caller, "INBOX", many); return err },
		"raw uid 0":             func() error { _, err := c.Raw(ctx, caller, "INBOX", 5, 0); return err },
		"raw uidValidity 0":     func() error { _, err := c.Raw(ctx, caller, "INBOX", 0, 5); return err },
		"append no id":          func() error { _, err := c.Append(ctx, caller, "", digest); return err },
		"append long id": func() error {
			_, err := c.Append(ctx, caller, "<"+strings.Repeat("a", MaxMessageIDNormChars+1)+">", digest)
			return err
		},
		"append bad digest": func() error { _, err := c.Append(ctx, caller, "<a@x.test>", "abc"); return err },
		"seq too many":      func() error { _, err := c.Seq(ctx, make([]string, MaxSeqSessions+1)); return err },
	}
	for name, call := range calls {
		err := call()
		if CodeOf(err) != "request" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// a body that is too large, whatever made it so
	big := struct {
		X string `json:"x"`
	}{strings.Repeat("x", MaxRequestBytes)}
	if err := c.json(ctx, "/folders", caller, big, nil); CodeOf(err) != "oversize" {
		t.Fatalf("oversize: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("%d calls reached the edge", rec.count())
	}
	if got := m.EdgeNotSent.Load(); got != int64(len(calls)) {
		t.Fatalf("notSent = %d, want %d", got, len(calls))
	}
	if m.EdgeOversize.Load() != 1 {
		t.Fatalf("oversize = %d", m.EdgeOversize.Load())
	}
	if len(faults) != len(calls)+1 {
		t.Fatalf("%d faults reported, want %d", len(faults), len(calls)+1)
	}
}

func TestClientIPIsABareAddress(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7":        "192.0.2.7",
		"::ffff:192.0.2.7": "192.0.2.7", // IPv4 wrapped in IPv6
		"2001:db8::1":      "2001:db8::1",
		"fe80::1%eth0":     "fe80::1", // no zone
		"::1":              "::1",
	}
	for in, want := range cases {
		if got := ClientIP(netip.MustParseAddr(in)); got != want {
			t.Fatalf("ClientIP(%s) = %q, want %q", in, got, want)
		}
	}
	if got := ClientIP(netip.Addr{}); got != "0.0.0.0" {
		t.Fatalf("no address: %q", got)
	}
	// and with a port it cannot even be handed over: the type is an address
	rec := &recorder{}
	c, _ := newTestClient(rec)
	_, _ = c.Login(context.Background(), "a@x.test", "pw", netip.MustParseAddrPort("[2001:db8::2%en0]:993").Addr())
	_, body := rec.last(t)
	var got struct {
		ClientIP string `json:"clientIp"`
	}
	_ = json.Unmarshal(body, &got)
	if got.ClientIP != "2001:db8::2" {
		t.Fatalf("clientIp = %q", got.ClientIP)
	}
}

func TestThrottledTakesRetryAfterFromBodyOrHeader(t *testing.T) {
	rec := &recorder{}
	c, _ := newTestClient(rec)
	ip := netip.MustParseAddr("192.0.2.7")

	rec.answer = func(string) (int, map[string]string, string) {
		return 429, map[string]string{"Retry-After": "40"}, `{"error":"throttled","message":"later","retryAfterSeconds":25}`
	}
	_, err := c.Login(context.Background(), "a@x.test", "pw", ip)
	if !Is(err, 429, "throttled") || RetryAfterOf(err) != 25 {
		t.Fatalf("body: %v, retry %d", err, RetryAfterOf(err))
	}
	rec.answer = func(string) (int, map[string]string, string) {
		return 429, map[string]string{"Retry-After": "40"}, `{"error":"throttled","message":"later"}`
	}
	_, err = c.Login(context.Background(), "a@x.test", "pw", ip)
	if !Is(err, 429, "throttled") || RetryAfterOf(err) != 40 {
		t.Fatalf("header: %v, retry %d", err, RetryAfterOf(err))
	}
	rec.answer = func(string) (int, map[string]string, string) {
		return 429, map[string]string{"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}, `{"error":"throttled","message":"later"}`
	}
	_, err = c.Login(context.Background(), "a@x.test", "pw", ip)
	if !Is(err, 429, "throttled") || RetryAfterOf(err) != 0 {
		t.Fatalf("date in header: %v, retry %d", err, RetryAfterOf(err))
	}
}

// 400, 413 and 500 point to a fault between bridge and edge: counted per
// status, over all paths and per path, and reported.
func TestFaultsAreCountedPerStatus(t *testing.T) {
	rec := &recorder{}
	var faults []string
	m := metrics.New()
	c := New(Options{SiteURL: "https://edge.test", Secret: strings.Repeat("s", 40), Metrics: m, Transport: rec,
		OnFault: func(path string, status int, code string) {
			faults = append(faults, path+" "+http.StatusText(status)+" "+code)
		}})
	ctx := context.Background()
	caller := Caller{Session: strings.Repeat("t", 43)}

	answer := func(status int, body string) {
		rec.answer = func(string) (int, map[string]string, string) { return status, nil, body }
	}
	answer(400, `{"error":"request","message":"Request invalid."}`)
	if _, err := c.Folders(ctx, caller); !Is(err, 400, "request") {
		t.Fatal(err)
	}
	if _, err := c.Entries(ctx, caller, "INBOX", 0, 10); !Is(err, 400, "request") {
		t.Fatal(err)
	}
	answer(413, `{"error":"too_large","message":"Request too large."}`)
	if _, err := c.Store(ctx, caller, "INBOX", []uint32{1}, []string{"k"}, nil); !Is(err, 413, "too_large") {
		t.Fatal(err)
	}
	answer(500, `{"error":"internal","message":"Internal error."}`)
	if _, err := c.Seq(ctx, []string{"a"}); !Is(err, 500, "internal") {
		t.Fatal(err)
	}
	// not faults: answers the contract provides for
	answer(404, `{"error":"gone","message":"x"}`)
	_, _ = c.Raw(ctx, caller, "INBOX", 1, 1)
	answer(401, `{"error":"session","message":"x"}`)
	_, _ = c.Folders(ctx, caller)
	// /submit: a rejection that names its SMTP answer is for the mail
	// program; without one, and for every 500, it is a fault
	answer(413, `{"error":"size","smtpCode":552,"message":"too large"}`)
	_, _ = c.Submit(ctx, caller, "a@x.test", []string{"b@y.test"}, []byte("x"))
	answer(400, `{"error":"request","message":"x"}`)
	_, _ = c.Submit(ctx, caller, "a@x.test", []string{"b@y.test"}, []byte("x"))
	answer(500, `{"error":"internal","smtpCode":550,"message":"x"}`)
	_, _ = c.Submit(ctx, caller, "a@x.test", []string{"b@y.test"}, []byte("x"))

	if m.Edge400.Load() != 3 || m.Edge413.Load() != 1 || m.Edge500.Load() != 2 {
		t.Fatalf("faults 400=%d 413=%d 500=%d", m.Edge400.Load(), m.Edge413.Load(), m.Edge500.Load())
	}
	if len(faults) != 6 {
		t.Fatalf("%d faults reported: %v", len(faults), faults)
	}
	per := map[string]int64{
		"/folders 400": 1, "/folders 401": 1, "/entries 400": 1, "/store 413": 1, "/seq 500": 1,
		"/raw 404": 1, "/submit 413": 1, "/submit 400": 1, "/submit 500": 1,
	}
	for k, want := range per {
		parts := strings.Split(k, " ")
		var status int
		_ = json.Unmarshal([]byte(parts[1]), &status)
		if got := m.Edge(parts[0]).StatusCount(status); got != want {
			t.Fatalf("%s: %d, want %d", k, got, want)
		}
	}
	snap, _ := json.Marshal(m.Snapshot())
	for _, want := range []string{`"http400":3`, `"http413":1`, `"http500":2`, `"status":{"400":1,"401":1}`} {
		if !strings.Contains(string(snap), want) {
			t.Fatalf("/metrics lacks %s in %s", want, snap)
		}
	}
}

func TestLoginRefusesASessionItCouldNotCarry(t *testing.T) {
	rec := &recorder{}
	c, _ := newTestClient(rec)
	ip := netip.MustParseAddr("192.0.2.7")
	for name, token := range map[string]string{
		"too long":   strings.Repeat("a", MaxSessionChars+1),
		"too short":  "abc",
		"with quote": strings.Repeat("a", 20) + `\"` + strings.Repeat("a", 20),
		"with blank": strings.Repeat("a", 20) + " " + strings.Repeat("a", 20),
		"not ascii":  strings.Repeat("a", 20) + "ü" + strings.Repeat("a", 20),
	} {
		tok := token
		rec.answer = func(string) (int, map[string]string, string) {
			return 200, nil, `{"session":"` + tok + `","expiresAt":32503680000000,"mailbox":"m1"}`
		}
		if _, err := c.Login(context.Background(), "a@x.test", "pw", ip); CodeOf(err) != "decode" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	rec.answer = func(string) (int, map[string]string, string) {
		return 200, nil, `{"session":"` + strings.Repeat("A", 43) + `","expiresAt":32503680000000,"mailbox":"m1"}`
	}
	if _, err := c.Login(context.Background(), "a@x.test", "pw", ip); err != nil {
		t.Fatal(err)
	}
}

func TestLoginInputAndMessageID(t *testing.T) {
	long := strings.Repeat("a", MaxAddressChars-7) + "@x.test"
	ok := [][2]string{{"a@x.test", "pw"}, {long, strings.Repeat("p", MaxPasswordChars)}, {"ä@x.test", "ü"}}
	for _, c := range ok {
		if !LoginInput(c[0], c[1]) {
			t.Fatalf("refused %q", c[0][:4])
		}
	}
	bad := [][2]string{{"", "pw"}, {"   ", "pw"}, {string(rune(0xfeff)), "pw"}, {"a@x.test", ""},
		{long + "a", "pw"}, {"a@x.test", strings.Repeat("p", MaxPasswordChars+1)}}
	for i, c := range bad {
		if LoginInput(c[0], c[1]) {
			t.Fatalf("accepted case %d", i)
		}
	}
	// characters are counted the way the edge counts them: 256 letters with
	// two bytes each are 256 characters
	if !LoginInput("a@x.test", strings.Repeat("ü", MaxPasswordChars)) {
		t.Fatal("256 two-byte characters refused")
	}

	if !AppendableMessageID("<a@x.test>") || !AppendableMessageID("a@x.test") ||
		!AppendableMessageID("<"+strings.Repeat("a", MaxMessageIDNormChars)+">") {
		t.Fatal("usual Message-ID refused")
	}
	bom := string(rune(0xfeff))
	for _, id := range []string{"", "<>", " < > ", "<" + strings.Repeat("a", MaxMessageIDNormChars+1) + ">",
		strings.Repeat("<", MaxMessageIDChars) + "a",
		// The edge trims the way JavaScript does: a byte order mark is white
		// space there, so this Message-ID is empty for the edge.
		"<" + bom + ">", bom + "<" + bom + " " + bom + ">" + bom,
		// Bytes that are not UTF-8 arrive as one replacement character each.
		"<" + strings.Repeat("\xff", MaxMessageIDNormChars+1) + ">"} {
		if AppendableMessageID(id) {
			t.Fatalf("accepted %.12q", id)
		}
	}
	// and U+0085 is no white space there: something is left
	if !AppendableMessageID("<" + string(rune(0x85)) + ">") {
		t.Fatal("refused a Message-ID the edge takes")
	}
	if !AppendableMessageID("<" + strings.Repeat("\xff", MaxMessageIDNormChars) + ">") {
		t.Fatal("refused 500 replacement characters")
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingBody) Close() error             { return nil }

type brokenTransport struct{ status int }

func (b brokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: b.status, Header: http.Header{}, ContentLength: -1, Body: failingBody{}, Request: req}, nil
}

// A 500 is a 500 whatever became of its body: unreadable or far too long,
// it is counted and reported all the same, and the error keeps the status.
func TestFaultsAreCountedWhenTheBodyIsUnusable(t *testing.T) {
	var faults []string
	onFault := func(path string, status int, code string) {
		faults = append(faults, fmt.Sprintf("%s %d %s", path, status, code))
	}
	m := metrics.New()
	c := New(Options{SiteURL: "https://edge.test", Secret: strings.Repeat("s", 40), Metrics: m, Transport: brokenTransport{500}, OnFault: onFault})
	_, err := c.Folders(context.Background(), Caller{Session: strings.Repeat("t", 43)})
	var ee *Error
	if !errors.As(err, &ee) || ee.Status != 500 || ee.Code != "read" {
		t.Fatalf("unreadable body: %v", err)
	}
	rec := &recorder{answer: func(string) (int, map[string]string, string) { return 400, nil, strings.Repeat("x", 70<<10) }}
	c2 := New(Options{SiteURL: "https://edge.test", Secret: strings.Repeat("s", 40), Metrics: m, Transport: rec, OnFault: onFault})
	if _, err := c2.Folders(context.Background(), Caller{Session: strings.Repeat("t", 43)}); !Is(err, 400, "too_large") {
		t.Fatalf("overlong body: %v", err)
	}
	if m.Edge500.Load() != 1 || m.Edge400.Load() != 1 {
		t.Fatalf("faults 500=%d 400=%d", m.Edge500.Load(), m.Edge400.Load())
	}
	if strings.Join(faults, "; ") != "/folders 500 read; /folders 400 too_large" {
		t.Fatalf("reported: %v", faults)
	}
}
