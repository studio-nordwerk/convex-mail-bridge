package mirror

import (
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/edge"
	"github.com/studio-nordwerk/convex-mail-bridge/bridge/internal/rawcache"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   []Range
		max  uint32
		want string
	}{
		{[]Range{{1, 3}, {2, 5}, {9, 9}, {6, 6}}, 20, "[{1 6} {9 9}]"},
		{[]Range{{5, 0}}, 20, "[{5 20}]"},
		{[]Range{{30, 0}}, 20, "[{20 30}]"}, // n:* always includes the last one
		{[]Range{{0, 0}}, 20, "[{20 20}]"},
		{[]Range{{0, 0}}, 0, "[]"},
		{[]Range{{7, 3}}, 20, "[{3 7}]"},
		{[]Range{{1, 4294967295}, {5, 6}}, 20, "[{1 4294967295}]"},
		{nil, 20, "[]"},
	}
	for _, tc := range cases {
		if got := fmt.Sprint(normalize(tc.in, tc.max)); got != tc.want {
			t.Errorf("%v max %d: %s, want %s", tc.in, tc.max, got, tc.want)
		}
	}
	set := normalize([]Range{{2, 4}, {10, 12}, {20, 20}}, 30)
	for n, want := range map[uint32]bool{1: false, 2: true, 4: true, 5: false, 11: true, 13: false, 20: true, 21: false} {
		if inRanges(set, n) != want {
			t.Errorf("inRanges(%d) = %v", n, !want)
		}
	}
}

func TestFlags(t *testing.T) {
	for f, want := range map[string]bool{
		`\Seen`: true, `\seen`: true, `$Forwarded`: true, "NonJunk": true, `\Recent`: false, `\*`: false,
		`\Invented`: false, "": false, "with space": false, "paren)": false, `a\b`: false, "umlaut-ä": false, `\`: false,
	} {
		if ValidFlag(f) != want {
			t.Errorf("ValidFlag(%q) = %v", f, !want)
		}
	}
	a, dropped := internFlags([]string{`\seen`, "$b", `\Seen`, "broken)", "$a"})
	if fmt.Sprint(a) != `[$a $b \Seen]` || dropped != 1 {
		t.Fatalf("internFlags = %v, dropped %d", a, dropped)
	}
	b, _ := internFlags([]string{"$a", `\Seen`, "$b"})
	if &a[0] != &b[0] {
		t.Fatal("equal flag sets do not share storage")
	}
	if !HasFlag(a, imap.FlagSeen) || !HasFlag(a, "$A") || HasFlag(a, imap.FlagDeleted) {
		t.Fatal("HasFlag")
	}
}

// The queues of all views together stay under the store's limit: views
// that are behind are closed before more is queued for them.
func TestQueuedUpdatesAreBoundedAcrossViews(t *testing.T) {
	st := New(Options{Raw: rawcache.New(1 << 20), MaxQueued: 100})
	killed := 0
	m := st.Attach("a@mailbox.test", "token", time.Time{}, func(string) { killed++ })
	f := m.Folder(edge.FolderInbox)
	f.loaded, f.uidValidity, f.uidNext = true, 7, 21
	for uid := uint32(1); uid <= 20; uid++ {
		f.msgs = append(f.msgs, Msg{UID: uid, Modseq: int64(uid)})
	}
	f.tracker = imapserver.NewMailboxTracker(20)
	var views []*View
	for i := 0; i < 4; i++ {
		// each view has its own member, as each connection has
		mm := st.Attach("a@mailbox.test", "token", time.Time{}, func(string) { killed++ })
		v, _, err := f.Open(mm, false)
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
	}
	modseq := int64(100)
	flip := func(n int, flag string) {
		var rows []edge.StoreRow
		for uid := uint32(1); uid <= uint32(n); uid++ {
			modseq++
			rows = append(rows, edge.StoreRow{UID: uid, Flags: []string{flag}, Modseq: modseq})
		}
		f.applyStore(rows)
	}
	flip(20, `\Seen`) // 20 updates x 4 views = 80: fits
	if q := st.queued.Load(); q != 80 || killed != 0 {
		t.Fatalf("queued %d, killed %d", q, killed)
	}
	flip(20, `\Flagged`) // 80 more would be 160: the views that are behind go first
	if q := st.queued.Load(); q > 100 {
		t.Fatalf("queued %d with a limit of 100", q)
	}
	if killed != 4 {
		t.Fatalf("killed %d views", killed)
	}
	for _, v := range views {
		if !v.Dead() {
			t.Fatal("a view that was behind is still alive")
		}
		v.Close() // closing a dropped view is harmless
		if _, err := v.Resolve(true, []Range{{1, 0}}); err != ErrViewDead {
			t.Fatalf("dead view answered: %v", err)
		}
	}
	if q := st.queued.Load(); q != 0 {
		t.Fatalf("queued %d after all views are gone", q)
	}
	// a fresh view starts at zero and gets updates again
	v, sel, err := f.Open(m, false)
	if err != nil || sel.NumMessages != 20 {
		t.Fatal(err)
	}
	flip(5, `\Answered`)
	if q := st.queued.Load(); q != 5 {
		t.Fatalf("queued %d for the fresh view", q)
	}
	v.Close()
	if q := st.queued.Load(); q != 0 {
		t.Fatalf("queued %d after closing", q)
	}
}

// Rows of two loads running side by side are both counted against the
// limit.
func TestReservationsAreCounted(t *testing.T) {
	st := New(Options{Raw: rawcache.New(1 << 20), MaxRows: 1000})
	if !st.reserve(600) {
		t.Fatal("first reservation refused")
	}
	if st.reserve(600) {
		t.Fatal("second reservation fits although the first is not released")
	}
	st.unreserve(600)
	if !st.reserve(600) {
		t.Fatal("reservation refused after release")
	}
	st.unreserve(600)
	// an idle mirror makes room, a mirror in use does not
	idle := st.Attach("idle@mailbox.test", "t", time.Time{}, nil)
	idle.box.rows.Store(700)
	if st.reserve(600) {
		t.Fatal("room taken from a mailbox with a connection")
	}
	idle.Detach()
	if !st.reserve(600) {
		t.Fatal("no room although the only other mirror is idle")
	}
	if n := len(st.boxes); n != 0 {
		t.Fatalf("%d mirrors left after eviction", n)
	}
}
