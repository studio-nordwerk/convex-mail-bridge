package edgestub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests pin the test edge itself to the rules of the real one, so
// that a bridge that breaks them fails its own tests against the stub.

const testSecret = "a-secret-of-at-least-32-characters"

type probe struct {
	t    *testing.T
	stub *Stub
	srv  *httptest.Server
}

func newProbe(t *testing.T) *probe {
	s := New(testSecret)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &probe{t: t, stub: s, srv: srv}
}

type answer struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func (p *probe) post(path, session string, body []byte) answer {
	p.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, p.srv.URL+"/bridge/v1"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Bridge-Session", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := answer{status: resp.StatusCode, header: resp.Header, raw: raw}
	_ = json.Unmarshal(raw, &a.body)
	return a
}

func (p *probe) login(address, password string) string {
	p.t.Helper()
	a := p.post("/login", "", []byte(`{"address":"`+address+`","password":"`+password+`","clientIp":"192.0.2.1"}`))
	if a.status != 200 {
		p.t.Fatalf("login: %d %s", a.status, a.raw)
	}
	return a.body["session"].(string)
}

func (p *probe) want(what string, a answer, status int, code string) {
	p.t.Helper()
	if a.status != status {
		p.t.Fatalf("%s: status %d, want %d (%s)", what, a.status, status, a.raw)
	}
	if code == "" {
		return
	}
	if a.body["error"] != code {
		p.t.Fatalf("%s: error %v, want %q", what, a.body["error"], code)
	}
	if msg, _ := a.body["message"].(string); msg == "" {
		p.t.Fatalf("%s: error body without message: %s", what, a.raw)
	}
}

func setup(t *testing.T) (*probe, string, string) {
	p := newProbe(t)
	p.stub.AddMailbox("mara@workshop-one.test")
	pw := p.stub.AddPassword("mara@workshop-one.test")
	for i := 0; i < 3; i++ {
		p.stub.Deliver("mara@workshop-one.test", Inbox, BuildMail(MailSpec{
			From: "a@outside.test", To: []string{"mara@workshop-one.test"}, Subject: "x", Text: "y",
			Date: time.Date(2026, 10, 1, 8, i, 0, 0, time.UTC),
		}), time.Date(2026, 10, 1, 8, i, 0, 0, time.UTC))
	}
	return p, pw, p.login("mara@workshop-one.test", pw)
}

func TestUnknownFieldsAreRefused(t *testing.T) {
	p, pw, session := setup(t)
	digest := strings.Repeat("ab", 32)
	good := map[string]string{
		"/login":   `{"address":"mara@workshop-one.test","password":"` + pw + `","clientIp":"192.0.2.1"}`,
		"/seq":     `{"sessions":["` + session + `"]}`,
		"/folders": `{}`,
		"/entries": `{"folder":"INBOX","limit":10}`,
		"/changes": `{"folder":"INBOX","sinceModseq":0,"limit":10}`,
		"/raw":     `{"folder":"INBOX","uid":1,"uidValidity":` + uidv(p, "INBOX") + `}`,
		"/store":   `{"folder":"INBOX","uids":[1],"add":["\\Seen"],"remove":[]}`,
		"/expunge": `{"folder":"INBOX","uids":[3]}`,
		"/append":  `{"folder":"Sent","messageId":"<nothing@x.test>","digest":"` + digest + `"}`,
	}
	for path, body := range good {
		a := p.post(path, session, []byte(body))
		wantStatus := 200
		if path == "/append" {
			wantStatus = 409 // unknown mail: the request as such is fine
		}
		if a.status != wantStatus {
			t.Fatalf("%s: a good request got %d %s", path, a.status, a.raw)
		}
		// the same with one field too many
		extra := `{"mailboxKey":"x",` + strings.TrimPrefix(body, "{")
		if body == `{}` {
			extra = `{"mailboxKey":"x"}`
		}
		p.want(path+" with an unknown field", p.post(path, session, []byte(extra)), 400, "request")
	}
	if got := p.stub.Refused(); got != len(good) {
		t.Fatalf("Refused() = %d, want %d", got, len(good))
	}
	// not an object, not JSON, not UTF-8, something behind the object
	for name, body := range map[string]string{
		"array": `[]`, "string": `"x"`, "null": `null`, "empty": ``, "broken": `{"folder":`,
		"bad utf-8": "{\"folder\":\"IN\xffBOX\",\"limit\":1}", "trailing": `{"folder":"INBOX","limit":1} {}`,
	} {
		p.want(name, p.post("/entries", session, []byte(body)), 400, "request")
	}
}

func uidv(p *probe, folder string) string {
	b, _ := json.Marshal(p.stub.State("mara@workshop-one.test", folder).UIDValidity)
	return string(b)
}

func TestFieldRanges(t *testing.T) {
	p, _, session := setup(t)
	many := `[` + strings.TrimSuffix(strings.Repeat("1,", 501), ",") + `]`
	flags65 := `[` + strings.TrimSuffix(strings.Repeat(`"k",`, 65), ",") + `]`
	longFlag := `["` + strings.Repeat("k", 101) + `"]`
	sessions201 := `[` + strings.TrimSuffix(strings.Repeat(`"x",`, 201), ",") + `]`
	bad := map[string][2]string{
		"entries without limit":  {"/entries", `{"folder":"INBOX"}`},
		"entries limit 0":        {"/entries", `{"folder":"INBOX","limit":0}`},
		"entries limit 2001":     {"/entries", `{"folder":"INBOX","limit":2001}`},
		"entries limit as text":  {"/entries", `{"folder":"INBOX","limit":"10"}`},
		"entries limit fraction": {"/entries", `{"folder":"INBOX","limit":1.5}`},
		"entries afterUid -1":    {"/entries", `{"folder":"INBOX","limit":1,"afterUid":-1}`},
		"entries empty folder":   {"/entries", `{"folder":"","limit":1}`},
		"entries long folder":    {"/entries", `{"folder":"` + strings.Repeat("f", 65) + `","limit":1}`},
		"changes without since":  {"/changes", `{"folder":"INBOX","limit":1}`},
		"changes negative":       {"/changes", `{"folder":"INBOX","sinceModseq":-1,"limit":1}`},
		"raw uid 0":              {"/raw", `{"folder":"INBOX","uid":0,"uidValidity":1}`},
		"raw without validity":   {"/raw", `{"folder":"INBOX","uid":1}`},
		"raw uid too big":        {"/raw", `{"folder":"INBOX","uid":4294967296,"uidValidity":1}`},
		"store 501 uids":         {"/store", `{"folder":"INBOX","uids":` + many + `,"add":["k"]}`},
		"store uid 0":            {"/store", `{"folder":"INBOX","uids":[0],"add":["k"]}`},
		"store without uids":     {"/store", `{"folder":"INBOX","add":["k"]}`},
		"store 65 flags":         {"/store", `{"folder":"INBOX","uids":[1],"add":` + flags65 + `}`},
		"store long flag":        {"/store", `{"folder":"INBOX","uids":[1],"remove":` + longFlag + `}`},
		"store empty flag":       {"/store", `{"folder":"INBOX","uids":[1],"add":[""]}`},
		"store flags as text":    {"/store", `{"folder":"INBOX","uids":[1],"add":"k"}`},
		"expunge 501 uids":       {"/expunge", `{"folder":"INBOX","uids":` + many + `}`},
		"append without id":      {"/append", `{"folder":"Sent","digest":"` + strings.Repeat("ab", 32) + `"}`},
		"append empty id":        {"/append", `{"folder":"Sent","messageId":"<>","digest":"` + strings.Repeat("ab", 32) + `"}`},
		"append long id":         {"/append", `{"folder":"Sent","messageId":"<` + strings.Repeat("a", 501) + `>","digest":"` + strings.Repeat("ab", 32) + `"}`},
		"append short digest":    {"/append", `{"folder":"Sent","messageId":"<a@x.test>","digest":"abc"}`},
		"append id of blanks":    {"/append", `{"folder":"Sent","messageId":"<` + string(rune(0xfeff)) + ` >","digest":"` + strings.Repeat("ab", 32) + `"}`},
		"seq 201 sessions":       {"/seq", `{"sessions":` + sessions201 + `}`},
		"seq not a list":         {"/seq", `{"sessions":"x"}`},
		"seq number in list":     {"/seq", `{"sessions":[1]}`},
		"folders with a field":   {"/folders", `{"folder":"INBOX"}`},
	}
	for name, c := range bad {
		p.want(name, p.post(c[0], session, []byte(c[1])), 400, "request")
	}
	// at the limits, and with the fields that may be missing or null
	fine := map[string][2]string{
		"entries limit 2000":       {"/entries", `{"folder":"INBOX","limit":2000}`},
		"entries afterUid null":    {"/entries", `{"folder":"INBOX","limit":1,"afterUid":null}`},
		"entries afterUid 0":       {"/entries", `{"folder":"INBOX","limit":1,"afterUid":0}`},
		"store without add/remove": {"/store", `{"folder":"INBOX","uids":[1]}`},
		"store add null":           {"/store", `{"folder":"INBOX","uids":[1],"add":null,"remove":null}`},
		"store 64 flags of 100":    {"/store", `{"folder":"INBOX","uids":[1],"add":[` + strings.TrimSuffix(strings.Repeat(`"`+strings.Repeat("k", 100)+`",`, 64), ",") + `]}`},
		"store no uids":            {"/store", `{"folder":"INBOX","uids":[]}`},
		"expunge no uids":          {"/expunge", `{"folder":"INBOX","uids":[]}`},
		"seq empty":                {"/seq", `{"sessions":[]}`},
	}
	for name, c := range fine {
		if a := p.post(c[0], session, []byte(c[1])); a.status != 200 {
			t.Fatalf("%s: %d %s", name, a.status, a.raw)
		}
	}
}

func TestClientIPHasToBeABareAddress(t *testing.T) {
	p := newProbe(t)
	p.stub.AddMailbox("mara@workshop-one.test")
	pw := p.stub.AddPassword("mara@workshop-one.test")
	login := func(ip string) answer {
		b, _ := json.Marshal(map[string]string{"address": "mara@workshop-one.test", "password": pw, "clientIp": ip})
		return p.post("/login", "", b)
	}
	for _, ip := range []string{"192.0.2.1", "0.0.0.0", "2001:db8::1", "::1", "::", "::ffff:192.0.2.1",
		"2001:db8:0:0:0:0:0:1", "2001:DB8::A"} {
		if a := login(ip); a.status != 200 {
			t.Fatalf("%q refused: %d %s", ip, a.status, a.raw)
		}
	}
	for _, ip := range []string{"", "192.0.2.1:993", "[2001:db8::1]", "[2001:db8::1]:993", "fe80::1%eth0",
		"192.0.2", "192.0.2.256", "1.2.3.4.5", "mail.example.test", "2001:db8::1::2", "2001:db8:::1",
		"12345::1", "1:2:3:4:5:6:7", "1:2:3:4:5:6:7:8:9", "192.0.2.1/32", strings.Repeat("1", 65)} {
		p.want("clientIp "+ip, login(ip), 400, "request")
	}
	// The edge trims the way JavaScript does: U+0085 is not white space
	// there, so it stays and spoils the address.
	p.want("clientIp with U+0085", login(string(rune(0x85))+"192.0.2.1"), 400, "request")
	if a := login(" 192.0.2.1\t"); a.status != 200 {
		t.Fatalf("blanks around the address: %d", a.status)
	}
	// missing altogether
	p.want("no clientIp", p.post("/login", "", []byte(`{"address":"mara@workshop-one.test","password":"`+pw+`"}`)), 400, "request")
}

func TestLargeBodiesAre413(t *testing.T) {
	p, _, session := setup(t)
	// 64 KiB exactly is taken (and then judged for its fields), one more is not
	pad := func(total int) []byte {
		head := `{"folder":"INBOX","limit":1,"afterUid":`
		return []byte(head + strings.Repeat(" ", total-len(head)-2) + `0}`)
	}
	if a := p.post("/entries", session, pad(64<<10)); a.status != 200 {
		t.Fatalf("64 KiB: %d %s", a.status, a.raw)
	}
	p.want("64 KiB and one byte", p.post("/entries", session, pad(64<<10+1)), 413, "too_large")
	p.want("login, large", p.post("/login", "", pad(70<<10)), 413, "too_large")
	if p.stub.Refused() != 2 {
		t.Fatalf("Refused() = %d", p.stub.Refused())
	}
}

func TestFoldersAndStates(t *testing.T) {
	p, _, session := setup(t)
	for _, path := range []string{"/entries", "/changes"} {
		body := `{"folder":"Archive","limit":1,"sinceModseq":0}`
		if path == "/entries" {
			body = `{"folder":"Archive","limit":1}`
		}
		p.want(path+" unknown folder", p.post(path, session, []byte(body)), 404, "folder")
	}
	p.want("/store unknown folder", p.post("/store", session, []byte(`{"folder":"Archive","uids":[1],"add":["k"]}`)), 404, "folder")
	p.want("/expunge unknown folder", p.post("/expunge", session, []byte(`{"folder":"Archive","uids":[1]}`)), 404, "folder")
	p.want("/raw unknown folder", p.post("/raw", session, []byte(`{"folder":"Archive","uid":1,"uidValidity":1}`)), 404, "gone")
	p.want("/append other folder", p.post("/append", session, []byte(`{"folder":"INBOX","messageId":"<a@x.test>","digest":"`+strings.Repeat("ab", 32)+`"}`)), 403, "folder")

	st := p.stub.State("mara@workshop-one.test", Inbox)
	at, _ := json.Marshal(st.HighestModseq)
	beyond, _ := json.Marshal(st.HighestModseq + 1)
	if a := p.post("/changes", session, []byte(`{"folder":"INBOX","sinceModseq":`+string(at)+`,"limit":10}`)); a.status != 200 {
		t.Fatalf("changes at the current state: %d", a.status)
	}
	p.want("changes from the future", p.post("/changes", session, []byte(`{"folder":"INBOX","sinceModseq":`+string(beyond)+`,"limit":10}`)), 410, "resync")

	// a session that cannot be one, and one that is not known
	p.want("odd session", p.post("/folders", "short", []byte(`{}`)), 401, "session")
	p.want("unknown session", p.post("/folders", strings.Repeat("A", 43), []byte(`{}`)), 401, "session")
	// the form of the body is judged before the session is looked up
	p.want("unknown session, bad body", p.post("/folders", strings.Repeat("A", 43), []byte(`{"x":1}`)), 400, "request")

	// /seq: what cannot be a session is answered as invalid, in its place
	a := p.post("/seq", "", []byte(`{"sessions":["short","`+session+`","`+strings.Repeat("A", 43)+`"]}`))
	if a.status != 200 {
		t.Fatalf("/seq: %d %s", a.status, a.raw)
	}
	res := a.body["results"].([]any)
	if len(res) != 3 || res[0].(map[string]any)["ok"] != false || res[1].(map[string]any)["ok"] != true || res[2].(map[string]any)["ok"] != false {
		t.Fatalf("/seq results: %s", a.raw)
	}
	if len(session) != 43 {
		t.Fatalf("a session has %d characters, the edge issues 43", len(session))
	}
}

func TestThrottleSparesTheValidPassword(t *testing.T) {
	p := newProbe(t)
	p.stub.AddMailbox("mara@workshop-one.test")
	pw := p.stub.AddPassword("mara@workshop-one.test")
	p.stub.ThrottleAfter = 3
	login := func(password, ip string) answer {
		b, _ := json.Marshal(map[string]string{"address": "mara@workshop-one.test", "password": password, "clientIp": ip})
		return p.post("/login", "", b)
	}
	for i := 0; i < 3; i++ {
		p.want("wrong", login("mb_wrong", "192.0.2.1"), 401, "auth")
	}
	a := login("mb_wrong", "192.0.2.1")
	p.want("over the limit", a, 429, "throttled")
	if a.header.Get("Retry-After") == "" || a.body["retryAfterSeconds"] == nil {
		t.Fatalf("429 without waiting time: header %q body %s", a.header.Get("Retry-After"), a.raw)
	}
	if a := login(pw, "192.0.2.1"); a.status != 200 {
		t.Fatalf("the valid password was held back: %d %s", a.status, a.raw)
	}
	// the IPv4 address wrapped in IPv6 is the same client, another IPv6
	// address in the same /64 as well
	p.want("same client, wrapped", login("mb_wrong", "::ffff:192.0.2.1"), 429, "throttled")
	for i := 0; i < 3; i++ {
		p.want("v6", login("mb_wrong", "2001:db8:1:2::"+string(rune('1'+i))), 401, "auth")
	}
	p.want("v6, same /64", login("mb_wrong", "2001:db8:1:2:ffff::9"), 429, "throttled")
	p.want("v6, other /64", login("mb_wrong", "2001:db8:1:3::1"), 401, "auth")
}

func TestShortSecretSwitchesTheEdgeOff(t *testing.T) {
	s := New("too-short-for-the-edge")
	srv := httptest.NewServer(s)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/bridge/v1/seq", strings.NewReader(`{"sessions":[]}`))
	req.Header.Set("Authorization", "Bearer too-short-for-the-edge")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 401 || len(body) != 0 {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}
