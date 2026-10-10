//go:build mbsync

// Package mbsync runs a strict real mail client, mbsync (isync), against
// the bridge and the test edge. It needs the mbsync binary and is therefore
// behind a build tag; test/mbsync.sh runs it in Docker.
package mbsync

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/bridge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/config"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

const (
	mara  = "mara@mailbox-one.test"
	jonas = "jonas@mailbox-two.test"
)

type world struct {
	t    *testing.T
	stub *edgestub.Stub
	b    *bridge.Bridge
	dir  string
	rc   string
}

// run calls mbsync for one channel and returns its output and the calls it
// caused at the edge, by path.
func (w *world) run(channel string) (string, map[string]int) {
	w.t.Helper()
	w.stub.ResetCalls()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mbsync", "-c", w.rc, "-V", channel)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("mbsync %s: %v\n%s", channel, err, out)
	}
	calls := map[string]int{}
	for _, p := range []string{"/login", "/folders", "/entries", "/changes", "/raw", "/store", "/expunge", "/append"} {
		if n := w.stub.Calls(p); n > 0 {
			calls[p] = n
		}
	}
	return string(out), calls
}

func (w *world) check(address, folder string, files []maildirFile) {
	w.t.Helper()
	rows := w.stub.State(address, folder).Rows
	if len(files) != len(rows) {
		w.t.Fatalf("%s %s: Maildir has %d mails, the edge %d", address, folder, len(files), len(rows))
	}
	want := map[string]edgestub.Row{}
	for _, r := range rows {
		want[r.Digest] = r
	}
	for _, f := range files {
		r, ok := want[f.digest]
		if !ok {
			data, _ := os.ReadFile(f.path)
			w.t.Logf("first 600 bytes of the Maildir file:\n%q", data[:min(len(data), 600)])
			w.t.Fatalf("%s %s: %s is not a mail of the edge (bytes differ)", address, folder, filepath.Base(f.path))
		}
		seen, flagged := false, false
		for _, fl := range r.Flags {
			seen = seen || fl == `\Seen`
			flagged = flagged || fl == `\Flagged`
		}
		if seen != strings.Contains(f.flags, "S") || flagged != strings.Contains(f.flags, "F") {
			w.t.Fatalf("%s %s uid %d: Maildir flags %q, edge flags %v", address, folder, r.UID, f.flags, r.Flags)
		}
	}
}

func fileFor(t *testing.T, files []maildirFile, digest string) maildirFile {
	t.Helper()
	for _, f := range files {
		if f.digest == digest {
			return f
		}
	}
	t.Fatalf("no Maildir file with digest %s", digest[:12])
	return maildirFile{}
}

// setFlag renames a Maildir file so that it carries one more flag letter.
func setFlag(t *testing.T, f maildirFile, letter string) {
	t.Helper()
	letters := strings.Split(f.flags+letter, "")
	sort.Strings(letters)
	i := strings.Index(f.path, ":2,")
	target := f.path[:i+3] + strings.Join(letters, "")
	// mails still in new/ move to cur/ when they get flags
	target = strings.Replace(target, string(filepath.Separator)+"new"+string(filepath.Separator), string(filepath.Separator)+"cur"+string(filepath.Separator), 1)
	if err := os.Rename(f.path, target); err != nil {
		t.Fatal(err)
	}
}

func TestMbsync(t *testing.T) {
	if _, err := exec.LookPath("mbsync"); err != nil {
		t.Skip("mbsync is not installed")
	}
	version, _ := exec.Command("mbsync", "--version").CombinedOutput()
	t.Logf("client: %s", strings.TrimSpace(string(version)))

	secret := "sec_mbsync_test_0123456789abcdef"
	stub := edgestub.New(secret)
	srv := httptest.NewServer(stub)
	defer srv.Close()
	vars := map[string]string{
		"CONVEX_SITE_URL": srv.URL, "MAIL_BRIDGE_SECRET": secret,
		"IMAP_ADDR": "127.0.0.1:0", "SMTP_ADDR": "127.0.0.1:0", "ADMIN_ADDR": "127.0.0.1:0",
		"PROXY_PROTOCOL": "off", "SEQ_INTERVAL_MS": "200",
	}
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	b, err := bridge.Start(cfg, bridge.Options{LogWriter: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()
	_, port, _ := net.SplitHostPort(b.IMAPAddr())

	// two mailboxes: 30 + 6 mails and 8 mails
	base := time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)
	pw := map[string]string{}
	for _, box := range []struct {
		addr        string
		inbox, sent int
	}{{mara, 30, 6}, {jonas, 8, 0}} {
		stub.AddMailbox(box.addr)
		pw[box.addr] = stub.AddPassword(box.addr)
		for i := 0; i < box.inbox; i++ {
			spec := edgestub.MailSpec{
				From: fmt.Sprintf("sender%d@outside.test", i), To: []string{box.addr},
				Subject: fmt.Sprintf("Received %d for %s – café", i+1, box.addr), Date: base.Add(time.Duration(i) * 5 * time.Hour),
				Text: fmt.Sprintf("Mail %d.\nWith two lines.", i+1),
			}
			if i%5 == 0 {
				spec.HTML = "<p>Mail als <b>HTML</b></p>"
			}
			if i%7 == 0 {
				spec.AttachName = "attachment.bin"
				spec.AttachData = bytes.Repeat([]byte{byte(i), 0xff, 0x00, '\n'}, 4000+i)
			}
			var flags []string
			if i%2 == 0 {
				flags = append(flags, `\Seen`)
			}
			if i%9 == 0 {
				flags = append(flags, `\Flagged`)
			}
			stub.Deliver(box.addr, edgestub.Inbox, edgestub.BuildMail(spec), spec.Date, flags...)
		}
		for i := 0; i < box.sent; i++ {
			spec := edgestub.MailSpec{
				From: box.addr, To: []string{fmt.Sprintf("customer%d@outside.test", i)},
				Subject: fmt.Sprintf("Sent %d", i+1), Date: base.Add(time.Duration(i) * 11 * time.Hour), Text: "Reply.",
			}
			stub.Deliver(box.addr, edgestub.Sent, edgestub.BuildMail(spec), spec.Date, `\Seen`)
		}
	}

	dir := t.TempDir()
	var rc strings.Builder
	for _, name := range []string{"mara", "jonas"} {
		addr := map[string]string{"mara": mara, "jonas": jonas}[name]
		pwFile := filepath.Join(dir, name+".pw")
		if err := os.WriteFile(pwFile, []byte(pw[addr]), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&rc, `IMAPAccount %[1]s
Host 127.0.0.1
Port %[2]s
User %[3]s
PassCmd "cat %[4]s"
SSLType None
AuthMechs LOGIN
Timeout 60

IMAPStore %[1]s-remote
Account %[1]s

MaildirStore %[1]s-local
Path %[5]s/
Inbox %[5]s/INBOX
SubFolders Verbatim

Channel %[1]s
Far :%[1]s-remote:
Near :%[1]s-local:
Patterns *
Create Near
Expunge Both
Remove None
SyncState *

`, name, port, addr, pwFile, filepath.Join(dir, name))
	}
	w := &world{t: t, stub: stub, b: b, dir: dir, rc: filepath.Join(dir, "mbsyncrc")}
	if err := os.WriteFile(w.rc, []byte(rc.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	inboxDir := filepath.Join(dir, "mara", "INBOX")
	sentDir := filepath.Join(dir, "mara", "Sent")

	// 1. first sync
	start := time.Now()
	_, calls := w.run("mara")
	t.Logf("run 1 (first sync):   %v in %v", calls, time.Since(start).Round(time.Millisecond))
	if calls["/raw"] != 36 {
		t.Fatalf("first sync fetched %d raw mails, want 36", calls["/raw"])
	}
	w.check(mara, edgestub.Inbox, readMaildir(t, inboxDir))
	w.check(mara, edgestub.Sent, readMaildir(t, sentDir))
	if _, err := os.Stat(filepath.Join(dir, "mara", "Archive")); err == nil {
		t.Fatal("mbsync created a folder that does not exist")
	}

	// 2. nothing changed: nothing is fetched, nothing is written
	before := names(readMaildir(t, inboxDir)) + names(readMaildir(t, sentDir))
	start = time.Now()
	_, calls = w.run("mara")
	t.Logf("run 2 (no change):    %v in %v", calls, time.Since(start).Round(time.Millisecond))
	if calls["/raw"]+calls["/store"]+calls["/expunge"]+calls["/append"] != 0 {
		t.Fatalf("a sync without changes caused %v", calls)
	}
	if calls["/entries"] != 0 {
		t.Fatalf("a second sync read the folders in full again: %v", calls)
	}
	if after := names(readMaildir(t, inboxDir)) + names(readMaildir(t, sentDir)); after != before {
		t.Fatal("a sync without changes changed the Maildir")
	}

	// 3. changes on both sides
	rows := stub.State(mara, edgestub.Inbox).Rows
	var readAtEdge, goneAtEdge, flagLocally, deleteLocally edgestub.Row
	for _, r := range rows {
		plain := len(r.Flags) == 0
		switch {
		case plain && readAtEdge.UID == 0:
			readAtEdge = r
		case plain && flagLocally.UID == 0:
			flagLocally = r
		case plain && deleteLocally.UID == 0:
			deleteLocally = r
		case plain && goneAtEdge.UID == 0:
			goneAtEdge = r
		}
	}
	if goneAtEdge.UID == 0 {
		t.Fatal("fixture has too few unread mails")
	}
	stub.SetFlags(mara, edgestub.Inbox, readAtEdge.UID, []string{`\Seen`}, nil) // read in the console
	stub.Remove(mara, edgestub.Inbox, goneAtEdge.UID)                           // deleted for good in the app
	fresh := edgestub.BuildMail(edgestub.MailSpec{From: "new@outside.test", To: []string{mara}, Subject: "New after the first sync", Text: "fresh", Date: time.Now()})
	stub.Deliver(mara, edgestub.Inbox, fresh, time.Now())
	files := readMaildir(t, inboxDir)
	setFlag(t, fileFor(t, files, flagLocally.Digest), "F")   // flagged in the mail program
	setFlag(t, fileFor(t, files, deleteLocally.Digest), "T") // deleted in the mail program

	start = time.Now()
	_, calls = w.run("mara")
	t.Logf("run 3 (both sides):   %v in %v", calls, time.Since(start).Round(time.Millisecond))
	if calls["/raw"] != 1 {
		t.Fatalf("after one new mail %d raw mails were fetched", calls["/raw"])
	}
	if calls["/store"] == 0 || calls["/expunge"] == 0 {
		t.Fatalf("local flag and local deletion did not reach the edge: %v", calls)
	}
	after := map[uint32]edgestub.Row{}
	for _, r := range stub.State(mara, edgestub.Inbox).Rows {
		after[r.UID] = r
	}
	if r, ok := after[flagLocally.UID]; !ok || fmt.Sprint(r.Flags) != `[\Flagged]` {
		t.Fatalf("flag set in the Maildir: edge has %v", after[flagLocally.UID].Flags)
	}
	if _, ok := after[deleteLocally.UID]; ok {
		t.Fatal("mail deleted in the Maildir is still in the folder at the edge")
	}
	files = readMaildir(t, inboxDir)
	w.check(mara, edgestub.Inbox, files)
	if f := fileFor(t, files, readAtEdge.Digest); !strings.Contains(f.flags, "S") {
		t.Fatalf("mail read at the edge has Maildir flags %q", f.flags)
	}
	fileFor(t, files, sha(fresh))
	for _, f := range files {
		if f.digest == goneAtEdge.Digest || f.digest == deleteLocally.Digest {
			t.Fatal("a removed mail is still in the Maildir")
		}
	}
	w.check(mara, edgestub.Sent, readMaildir(t, sentDir))

	// 4. stable again
	before = names(readMaildir(t, inboxDir)) + names(readMaildir(t, sentDir))
	start = time.Now()
	_, calls = w.run("mara")
	t.Logf("run 4 (stable):       %v in %v", calls, time.Since(start).Round(time.Millisecond))
	if calls["/raw"]+calls["/store"]+calls["/expunge"]+calls["/append"]+calls["/entries"] != 0 {
		t.Fatalf("the follow-up sync is not quiet: %v", calls)
	}
	if after := names(readMaildir(t, inboxDir)) + names(readMaildir(t, sentDir)); after != before {
		t.Fatal("the follow-up sync changed the Maildir")
	}

	// the second mailbox gets its own mail and nothing else
	_, calls = w.run("jonas")
	t.Logf("other mailbox:        %v", calls)
	jfiles := readMaildir(t, filepath.Join(dir, "jonas", "INBOX"))
	w.check(jonas, edgestub.Inbox, jfiles)
	maraDigests := map[string]bool{}
	for _, f := range append(readMaildir(t, inboxDir), readMaildir(t, sentDir)...) {
		maraDigests[f.digest] = true
	}
	for _, f := range jfiles {
		if maraDigests[f.digest] {
			t.Fatal("a mail of one mailbox is in the Maildir of the other")
		}
	}
	t.Logf("end state: INBOX %d, Sent %d (mara), INBOX %d (jonas)", len(stub.State(mara, edgestub.Inbox).Rows), len(stub.State(mara, edgestub.Sent).Rows), len(jfiles))
}
