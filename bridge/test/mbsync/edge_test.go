//go:build mbsyncedge

// The mbsync run against a given edge: test/against-edge.sh starts the
// bridge against that edge and, with MBSYNC=1, runs this next to it. Unlike
// the run against the test edge it cannot look into the edge; it compares
// what mbsync wrote to disk with what the bridge shows over IMAP, and reads
// the calls to the edge from /metrics.
//
// mbsync only pulls here: nothing is written to the mailbox. The Maildir
// lives in the temporary directory of the test, inside the container.
package mbsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type edgeCounters map[string]int64

func readCounters(t *testing.T, url string) edgeCounters {
	t.Helper()
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Get(url)
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	defer resp.Body.Close()
	var m struct {
		Edge map[string]struct {
			Count int64 `json:"count"`
		} `json:"edge"`
		Mirror     map[string]int64 `json:"mirror"`
		EdgeFaults map[string]int64 `json:"edgeFaults"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	out := edgeCounters{"fullLoads": m.Mirror["fullLoads"]}
	for path, e := range m.Edge {
		out[path] = e.Count
	}
	for k, n := range m.EdgeFaults {
		out["fault "+k] = n
	}
	return out
}

func (c edgeCounters) since(before edgeCounters) map[string]int64 {
	d := map[string]int64{}
	for k, n := range c {
		if n != before[k] {
			d[k] = n - before[k]
		}
	}
	return d
}

// imapDigests reads every mail of a folder through the bridge and returns
// the digests of their bytes, sorted.
func imapDigests(t *testing.T, addr, user, pass, folder string) []string {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(user, pass).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatalf("EXAMINE %s: %v", folder, err)
	}
	var out []string
	if sel.NumMessages == 0 {
		return out
	}
	uids, err := c.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range uids {
		msgs, err := c.Fetch(imap.UIDSetNum(u.UID), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect()
		if err != nil || len(msgs) != 1 || len(msgs[0].BodySection) != 1 {
			t.Fatalf("%s uid %d: %v", folder, u.UID, err)
		}
		out = append(out, sha(msgs[0].BodySection[0].Bytes))
	}
	sort.Strings(out)
	return out
}

func TestMbsyncAgainstEdge(t *testing.T) {
	if _, err := exec.LookPath("mbsync"); err != nil {
		t.Fatal("mbsync is not installed")
	}
	addr := os.Getenv("PROBE_IMAP_ADDR")
	metricsURL := os.Getenv("PROBE_METRICS_URL")
	user := os.Getenv("MAILBOX_ADDRESS")
	if addr == "" || metricsURL == "" || user == "" || os.Getenv("DEVICE_PASSWORD") == "" {
		t.Fatal("PROBE_IMAP_ADDR, PROBE_METRICS_URL, MAILBOX_ADDRESS and DEVICE_PASSWORD have to be set")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	version, _ := exec.Command("mbsync", "--version").CombinedOutput()
	t.Logf("client: %s", strings.TrimSpace(string(version)))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "box"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The password is not written anywhere: mbsync asks the environment.
	rc := fmt.Sprintf(`IMAPAccount box
Host %s
Port %s
User %s
PassCmd "printenv DEVICE_PASSWORD"
SSLType None
AuthMechs LOGIN
Timeout 120

IMAPStore box-remote
Account box

MaildirStore box-local
Path %[4]s/
Inbox %[4]s/INBOX
SubFolders Verbatim

Channel box
Far :box-remote:
Near :box-local:
Patterns *
Sync Pull
Create Near
Expunge None
Remove None
SyncState *
`, host, port, strconv.Quote(user), filepath.Join(dir, "box"))
	rcPath := filepath.Join(dir, "mbsyncrc")
	if err := os.WriteFile(rcPath, []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() (map[string]int64, time.Duration) {
		before := readCounters(t, metricsURL)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		start := time.Now()
		out, err := exec.CommandContext(ctx, "mbsync", "-c", rcPath, "box").CombinedOutput()
		took := time.Since(start)
		if err != nil {
			// The output of mbsync names folders and counts, not contents;
			// the user name could be in an error line, so it is cut out.
			t.Fatalf("mbsync: %v\n%s", err, strings.ReplaceAll(string(out), user, "<address>"))
		}
		return readCounters(t, metricsURL).since(before), took
	}
	inboxDir := filepath.Join(dir, "box", "INBOX")
	sentDir := filepath.Join(dir, "box", "Sent")

	// 1. first sync: everything arrives, byte for byte
	calls, took := run()
	t.Logf("run 1 (first sync): %v in %v", calls, took.Round(time.Millisecond))
	total := 0
	for folder, mdir := range map[string]string{"INBOX": inboxDir, "Sent": sentDir} {
		files := readMaildir(t, mdir)
		var got []string
		for _, f := range files {
			got = append(got, f.digest)
		}
		sort.Strings(got)
		want := imapDigests(t, addr, user, os.Getenv("DEVICE_PASSWORD"), folder)
		if len(got) != len(want) {
			t.Fatalf("%s: the Maildir has %d mails, the bridge shows %d", folder, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: a mail in the Maildir differs from what the bridge shows (after %d equal ones)", folder, i)
			}
		}
		t.Logf("%s: %d mails in the Maildir, byte for byte what the bridge shows", folder, len(got))
		total += len(got)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "box"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "INBOX" && e.Name() != "Sent" {
			t.Fatalf("mbsync created %q: a folder that does not exist", e.Name())
		}
	}
	for _, k := range []string{"/store", "/expunge", "/append", "/submit"} {
		if calls[k] != 0 {
			t.Fatalf("a pulling sync wrote to the mailbox: %v", calls)
		}
	}

	// 2. nothing changed: nothing is fetched, nothing is read in full,
	// nothing is written
	before := names(readMaildir(t, inboxDir)) + names(readMaildir(t, sentDir))
	calls, took = run()
	t.Logf("run 2 (no change):  %v in %v", calls, took.Round(time.Millisecond))
	if calls["/raw"]+calls["/store"]+calls["/expunge"]+calls["/append"] != 0 {
		t.Fatalf("a sync without changes caused %v", calls)
	}
	if calls["/entries"] != 0 || calls["fullLoads"] != 0 {
		t.Fatalf("a second sync read the folders in full again: %v", calls)
	}
	if after := names(readMaildir(t, inboxDir)) + names(readMaildir(t, sentDir)); after != before {
		t.Fatal("a sync without changes changed the Maildir")
	}
	for k, n := range calls {
		if strings.HasPrefix(k, "fault ") && n != 0 {
			t.Fatalf("the edge refused calls of the bridge: %v", calls)
		}
	}
	t.Logf("%d mails in all; second run without fetches", total)
}
