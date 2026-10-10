package bridge_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edgestub"
)

// The random test: several IMAP connections and the edge change one folder
// at the same time (flags, removals, new mail) while the bridge keeps
// polling. Afterwards every long-lived connection must, after NOOP, hold
// the same picture as a fresh connection, and both the same as the edge.
//
// "Picture" is taken literally: each long-lived connection keeps a model of
// the folder that is built from nothing but the untagged responses it was
// sent (EXISTS, EXPUNGE, FETCH FLAGS), in the order they arrived. A wrong
// sequence number in a single EXPUNGE makes the model drift, even if a
// later FETCH 1:* would look right.
//
// The generator is fixed (mulberry32), so every run takes the same
// decisions per actor; only the interleaving is up to the scheduler.
// test/fuzz-mutation-check.sh runs the same test against a build with a
// deliberately wrong removal order and expects it to fail.

type rng struct{ s uint32 }

func (r *rng) next() uint32 {
	r.s += 0x6D2B79F5
	t := r.s
	t = (t ^ (t >> 15)) * (t | 1)
	t ^= t + (t^(t>>7))*(t|61)
	return t ^ (t >> 14)
}

func (r *rng) intn(n int) int { return int(r.next() % uint32(n)) }

type modelEntry struct {
	uid   uint32 // 0: the server has not told us yet
	flags []string
	known bool // flags are known
}

// fuzzConn is a minimal IMAP client that sees every untagged line in order.
type fuzzConn struct {
	name  string
	r     *rawConn
	tag   int
	model []modelEntry
	errs  []string
	dead  bool
}

var (
	reNum   = regexp.MustCompile(`^\* (\d+) (EXISTS|EXPUNGE|RECENT|FETCH)(.*)$`)
	reUID   = regexp.MustCompile(`UID (\d+)`)
	reFlags = regexp.MustCompile(`FLAGS \(([^)]*)\)`)
)

func (f *fuzzConn) fail(format string, args ...any) {
	if len(f.errs) < 5 {
		f.errs = append(f.errs, f.name+": "+fmt.Sprintf(format, args...))
	}
}

func (f *fuzzConn) apply(line string) {
	m := reNum.FindStringSubmatch(line)
	if m == nil {
		return
	}
	n, _ := strconv.Atoi(m[1])
	switch m[2] {
	case "EXISTS":
		if n < len(f.model) {
			f.fail("EXISTS %d although the client knows %d mails", n, len(f.model))
			return
		}
		for len(f.model) < n {
			f.model = append(f.model, modelEntry{})
		}
	case "EXPUNGE":
		if n < 1 || n > len(f.model) {
			f.fail("EXPUNGE %d with %d mails", n, len(f.model))
			return
		}
		f.model = append(f.model[:n-1], f.model[n:]...)
	case "FETCH":
		if n < 1 || n > len(f.model) {
			f.fail("FETCH for sequence number %d with %d mails", n, len(f.model))
			return
		}
		e := &f.model[n-1]
		if u := reUID.FindStringSubmatch(m[3]); u != nil {
			uid64, _ := strconv.ParseUint(u[1], 10, 32)
			uid := uint32(uid64)
			if e.uid != 0 && e.uid != uid {
				f.fail("sequence number %d was uid %d and is now uid %d", n, e.uid, uid)
			}
			e.uid = uid
		}
		if fl := reFlags.FindStringSubmatch(m[3]); fl != nil {
			e.flags = strings.Fields(fl[1])
			sort.Strings(e.flags)
			e.known = true
		}
	}
}

// cmd sends a command and reads up to its tagged answer, applying every
// untagged line to the model. It returns the status word (OK, NO, BAD).
func (f *fuzzConn) cmd(format string, args ...any) string {
	if f.dead {
		return "DEAD"
	}
	f.tag++
	tag := fmt.Sprintf("t%d", f.tag)
	_ = f.r.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(f.r.c, tag+" "+format+"\r\n", args...); err != nil {
		f.dead = true
		f.fail("connection lost while writing: %v", err)
		return "DEAD"
	}
	for {
		line, err := f.r.line()
		if err != nil {
			f.dead = true
			f.fail("connection lost while waiting for %s (%s)", tag, strings.Fields(fmt.Sprintf(format, args...))[0])
			return "DEAD"
		}
		if strings.HasPrefix(line, tag+" ") {
			return strings.Fields(line)[1]
		}
		if strings.HasPrefix(line, "* BYE") {
			f.fail("server said %q", line)
		}
		f.apply(line)
	}
}

func (f *fuzzConn) knownUIDs() []uint32 {
	var out []uint32
	for _, e := range f.model {
		if e.uid != 0 {
			out = append(out, e.uid)
		}
	}
	return out
}

func openFuzzConn(t *testing.T, e *env, name, address, folder string) *fuzzConn {
	t.Helper()
	f := &fuzzConn{name: name, r: dialRaw(t, e.b.IMAPAddr())}
	f.r.until("* OK")
	if st := f.cmd("LOGIN %s %s", address, e.pw[address]); st != "OK" {
		t.Fatalf("%s: login %s", name, st)
	}
	if st := f.cmd("SELECT %s", folder); st != "OK" {
		t.Fatalf("%s: select %s", name, st)
	}
	if len(f.model) > 0 {
		if st := f.cmd("FETCH 1:* (UID FLAGS)"); st != "OK" {
			t.Fatalf("%s: initial fetch %s", name, st)
		}
	}
	return f
}

var fuzzFlags = []string{`\Seen`, `\Flagged`, `\Answered`, `$Forwarded`, `$MailFlagBit0`}

func runRandom(t *testing.T, seed uint32, conns, steps int) (problems []string) {
	t.Helper()
	e := newEnv(t)
	e.mailbox(mara, 40, 6)
	e.mailbox(jonas, 5, 0)
	untouched := e.stub.State(jonas, edgestub.Inbox)

	var actors []*fuzzConn
	for i := 0; i < conns; i++ {
		actors = append(actors, openFuzzConn(t, e, fmt.Sprintf("conn%d", i), mara, "INBOX"))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// the bridge looks for changes all the time
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := &rng{s: seed ^ 0xA5A5A5A5}
		for {
			select {
			case <-stop:
				return
			default:
			}
			e.sync()
			time.Sleep(time.Duration(1+r.intn(6)) * time.Millisecond)
		}
	}()

	var actorWG sync.WaitGroup
	// the edge: new mail, read/unread in the console, deleted for good
	actorWG.Add(1)
	go func() {
		defer actorWG.Done()
		r := &rng{s: seed ^ 0x0E0E0E0E}
		for i := 0; i < steps; i++ {
			rows := e.stub.State(mara, edgestub.Inbox).Rows
			switch k := r.intn(10); {
			case k < 4:
				raw := edgestub.BuildMail(edgestub.MailSpec{From: "random@outside.test", To: []string{mara}, Subject: fmt.Sprintf("Random %d", i), Text: "x"})
				e.stub.Deliver(mara, edgestub.Inbox, raw, time.Now())
			case k < 7 && len(rows) > 0:
				row := rows[r.intn(len(rows))]
				flag := []string{fuzzFlags[r.intn(len(fuzzFlags))]}
				if r.intn(2) == 0 {
					e.stub.SetFlags(mara, edgestub.Inbox, row.UID, flag, nil)
				} else {
					e.stub.SetFlags(mara, edgestub.Inbox, row.UID, nil, flag)
				}
			case k < 9 && len(rows) > 25:
				// several removals at once: they reach the bridge in one
				// batch
				n := 1 + r.intn(3)
				for j := 0; j < n; j++ {
					e.stub.Remove(mara, edgestub.Inbox, rows[r.intn(len(rows))].UID)
				}
			}
			time.Sleep(time.Duration(r.intn(4)) * time.Millisecond)
		}
	}()
	// the mail programs
	for i, f := range actors {
		actorWG.Add(1)
		go func(i int, f *fuzzConn) {
			defer actorWG.Done()
			r := &rng{s: seed + uint32(i)*7919}
			for s := 0; s < steps && !f.dead; s++ {
				uids := f.knownUIDs()
				flag := fuzzFlags[r.intn(len(fuzzFlags))]
				op := "+"
				if r.intn(3) == 0 {
					op = "-"
				}
				switch k := r.intn(12); {
				case k < 4 && len(uids) > 0:
					a := uids[r.intn(len(uids))]
					b := uids[r.intn(len(uids))]
					if a > b {
						a, b = b, a
					}
					if b-a > 6 {
						b = a + 6
					}
					f.cmd("UID STORE %d:%d %sFLAGS (%s)", a, b, op, flag)
				case k < 6 && len(f.model) > 0:
					n := 1 + r.intn(len(f.model))
					f.cmd("STORE %d %sFLAGS (%s)", n, op, flag)
				case k == 6 && len(uids) > 20:
					uid := uids[r.intn(len(uids))]
					f.cmd(`UID STORE %d +FLAGS (\Deleted)`, uid)
					if r.intn(2) == 0 {
						f.cmd("UID EXPUNGE %d", uid)
					} else {
						f.cmd("EXPUNGE")
					}
				case k == 7 && len(f.model) > 0:
					// usually: look at what arrived since (the mails the
					// client only knows from EXISTS)
					from := 1 + r.intn(len(f.model))
					for i, me := range f.model {
						if me.uid == 0 {
							from = i + 1
							break
						}
					}
					f.cmd("FETCH %d:* (UID FLAGS)", from)
				case k == 8:
					f.cmd("UID SEARCH UNSEEN")
				default:
					f.cmd("NOOP")
				}
				time.Sleep(time.Duration(r.intn(3)) * time.Millisecond)
			}
		}(i, f)
	}
	actorWG.Wait()
	close(stop)
	wg.Wait()

	// Quiet now. One more look at the edge, then every connection asks.
	e.sync()
	st := e.stub.State(mara, edgestub.Inbox)
	for _, f := range actors {
		f.cmd("NOOP")
		problems = append(problems, f.errs...)
		if f.dead {
			continue
		}
		// 1. the model built from the stream of updates
		if len(f.model) != len(st.Rows) {
			problems = append(problems, fmt.Sprintf("%s: the client believes in %d mails, the edge has %d", f.name, len(f.model), len(st.Rows)))
			continue
		}
		knownUID, knownFlags := 0, 0
		for i, me := range f.model {
			row := st.Rows[i]
			if me.uid != 0 {
				knownUID++
				if me.uid != row.UID {
					problems = append(problems, fmt.Sprintf("%s: position %d is uid %d for the client and uid %d at the edge", f.name, i+1, me.uid, row.UID))
					break
				}
			}
			if me.known {
				knownFlags++
				want := append([]string(nil), row.Flags...)
				sort.Strings(want)
				if !sameStrings(me.flags, want) {
					problems = append(problems, fmt.Sprintf("%s: uid %d has flags %v for the client and %v at the edge", f.name, row.UID, me.flags, want))
					break
				}
			}
		}
		if knownUID < len(st.Rows)/2 || knownFlags < len(st.Rows)/2 {
			problems = append(problems, fmt.Sprintf("%s: the model knows only %d uids and %d flag sets of %d mails, the check is weak", f.name, knownUID, knownFlags, len(st.Rows)))
		}
		// 2. what the long-lived connection answers when asked
		before := f.model
		f.model = make([]modelEntry, len(before))
		if len(before) > 0 {
			f.cmd("FETCH 1:* (UID FLAGS)")
		}
		for i, me := range f.model {
			row := st.Rows[i]
			want := append([]string(nil), row.Flags...)
			sort.Strings(want)
			if me.uid != row.UID || !sameStrings(me.flags, want) {
				problems = append(problems, fmt.Sprintf("%s: FETCH says position %d is uid %d %v, the edge has uid %d %v", f.name, i+1, me.uid, me.flags, row.UID, want))
				break
			}
		}
		problems = append(problems, f.errs[min(len(f.errs), len(problems)):]...)
	}
	// 3. a fresh connection
	fresh := openFuzzConn(t, e, "fresh", mara, "INBOX")
	if len(fresh.model) != len(st.Rows) {
		problems = append(problems, fmt.Sprintf("fresh connection sees %d mails, the edge has %d", len(fresh.model), len(st.Rows)))
	} else {
		for i, me := range fresh.model {
			row := st.Rows[i]
			want := append([]string(nil), row.Flags...)
			sort.Strings(want)
			if me.uid != row.UID || !sameStrings(me.flags, want) {
				problems = append(problems, fmt.Sprintf("fresh connection: position %d is uid %d %v, the edge has uid %d %v", i+1, me.uid, me.flags, row.UID, want))
				break
			}
		}
	}
	// the other mailbox was never touched
	after := e.stub.State(jonas, edgestub.Inbox)
	if after.HighestModseq != untouched.HighestModseq {
		problems = append(problems, "the other mailbox changed")
	}
	if n := e.metric("panics"); n != 0 {
		problems = append(problems, fmt.Sprintf("%d panics", n))
	}
	if n := e.metric("mirror", "inconsistent"); n != 0 {
		problems = append(problems, fmt.Sprintf("mirror inconsistent %d times", n))
	}
	t.Logf("seed %d: %d mails at the end, modseq %d, %d /changes calls, %d /store, %d /expunge",
		seed, len(st.Rows), st.HighestModseq, e.stub.Calls("/changes"), e.stub.Calls("/store"), e.stub.Calls("/expunge"))
	return problems
}

func TestRandomConcurrentChanges(t *testing.T) {
	seeds := []uint32{20261007, 1, 424242, 0xC0FFEE}
	if v := os.Getenv("MAILBRIDGE_FUZZ_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		seeds = []uint32{uint32(n)}
	}
	steps := 250
	if testing.Short() {
		steps = 80
	}
	for _, seed := range seeds {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			if problems := runRandom(t, seed, 4, steps); len(problems) > 0 {
				t.Fatalf("seed %d: long-lived connections, a fresh connection and the edge disagree:\n  %s", seed, strings.Join(problems, "\n  "))
			}
		})
	}
}
