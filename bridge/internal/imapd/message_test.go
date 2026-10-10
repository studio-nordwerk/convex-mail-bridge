package imapd

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestSplitMessage(t *testing.T) {
	cases := []struct{ raw, header, body string }{
		{"A: 1\r\nB: 2\r\n\r\nbody\r\n", "A: 1\r\nB: 2\r\n\r\n", "body\r\n"},
		{"A: 1\nB: 2\n\nbody\n", "A: 1\nB: 2\n\n", "body\n"},
		{"A: 1\r\n\tfolded\r\n\r\n", "A: 1\r\n\tfolded\r\n\r\n", ""},
		{"A: 1\r\n \r\nB: 2\r\n\r\nx", "A: 1\r\n \r\nB: 2\r\n\r\n", "x"}, // a line of blanks continues the field
		{"no end of header", "no end of header", ""},
		{"\r\nbody only", "\r\n", "body only"},
		{"", "", ""},
	}
	for _, tc := range cases {
		h, b := splitMessage([]byte(tc.raw))
		if string(h) != tc.header || string(b) != tc.body {
			t.Errorf("%q: header %q body %q", tc.raw, h, b)
		}
		if string(h)+string(b) != tc.raw {
			t.Errorf("%q: header and body do not add up", tc.raw)
		}
	}
}

func TestFilterHeaderKeepsBytes(t *testing.T) {
	header := "Subject: Hello\r\n World\r\nFrom: a@outside.test\r\nbroken line\r\nX-Empty:\r\nTo: b@outside.test\r\n\r\n"
	got := string(filterHeader([]byte(header), []string{"subject", "TO"}, false))
	if got != "Subject: Hello\r\n World\r\nTo: b@outside.test\r\n\r\n" {
		t.Fatalf("FIELDS = %q", got)
	}
	got = string(filterHeader([]byte(header), []string{"Subject", "To"}, true))
	if got != "From: a@outside.test\r\nX-Empty:\r\n\r\n" {
		t.Fatalf("FIELDS.NOT = %q", got)
	}
	if got := string(filterHeader([]byte("Subject: x"), []string{"Subject"}, false)); got != "Subject: x\r\n\r\n" {
		t.Fatalf("header without line end = %q", got)
	}
	if got := string(filterHeader(nil, []string{"Subject"}, false)); got != "\r\n" {
		t.Fatalf("empty header = %q", got)
	}
}

func TestApplyPartial(t *testing.T) {
	b := []byte("0123456789")
	cases := []struct {
		off, size int64
		want      string
	}{
		{0, 4, "0123"}, {8, 10, "89"}, {10, 5, ""}, {99, 1, ""}, {3, 0, ""},
		{2, 1<<63 - 1, "23456789"}, {1<<63 - 1, 1<<63 - 1, ""},
	}
	for _, tc := range cases {
		got := applyPartial(b, &imap.SectionPartial{Offset: tc.off, Size: tc.size})
		if string(got) != tc.want {
			t.Errorf("<%d.%d> = %q, want %q", tc.off, tc.size, got, tc.want)
		}
	}
	if got := applyPartial(b, nil); !bytes.Equal(got, b) {
		t.Fatal("no partial changed the bytes")
	}
}

func TestMessageID(t *testing.T) {
	cases := map[string]string{
		"Message-ID: <a@b.test>\r\n\r\n":                         "<a@b.test>",
		"message-id:\r\n <fold@b.test>\r\nSubject: x\r\n\r\n":    "<fold@b.test>",
		"Subject: x\r\nMessage-Id: no-brackets@b.test\r\n\r\n": "no-brackets@b.test",
		"Subject: x\r\n\r\nMessage-ID: <im-text@b.test>\r\n":     "",
		"Message-ID: <first@b.test> <second@b.test>\r\n\r\n":     "<first@b.test>",
		"X-Message-ID: <wrong@b.test>\r\n\r\n":                  "",
		"":                                                       "",
	}
	for raw, want := range cases {
		if got := messageID([]byte(raw)); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}

func depthOf(bs imap.BodyStructure) int {
	switch b := bs.(type) {
	case *imap.BodyStructureMultiPart:
		max := 0
		for _, c := range b.Children {
			if d := depthOf(c); d > max {
				max = d
			}
		}
		return 1 + max
	case *imap.BodyStructureSinglePart:
		if b.MessageRFC822 != nil {
			return 1 + depthOf(b.MessageRFC822.BodyStructure)
		}
	}
	return 1
}

func countParts(bs imap.BodyStructure) int {
	switch b := bs.(type) {
	case *imap.BodyStructureMultiPart:
		n := 1
		for _, c := range b.Children {
			n += countParts(c)
		}
		return n
	case *imap.BodyStructureSinglePart:
		if b.MessageRFC822 != nil {
			return 1 + countParts(b.MessageRFC822.BodyStructure)
		}
	}
	return 1
}

func TestBodyStructureIsBounded(t *testing.T) {
	var deep strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&deep, "Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
	}
	deep.WriteString("Content-Type: text/plain\r\n\r\nunten\r\n")
	bs := parseMessage([]byte(deep.String())).bodyStructure()
	if d := depthOf(bs); d > maxMIMEDepth+1 {
		t.Fatalf("structure is %d levels deep", d)
	}

	var nested strings.Builder
	for i := 0; i < 20000; i++ {
		nested.WriteString("Content-Type: message/rfc822\r\n\r\n")
	}
	nested.WriteString("Subject: inside\r\n\r\nx\r\n")
	bs = parseMessage([]byte(nested.String())).bodyStructure()
	if d := depthOf(bs); d > maxMIMEDepth+1 {
		t.Fatalf("nested messages are %d levels deep", d)
	}

	var wide strings.Builder
	wide.WriteString("Content-Type: multipart/mixed; boundary=w\r\n\r\n")
	for i := 0; i < 20000; i++ {
		wide.WriteString("--w\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	wide.WriteString("--w--\r\n")
	bs = parseMessage([]byte(wide.String())).bodyStructure()
	if n := countParts(bs); n > maxMIMEParts+1 {
		t.Fatalf("structure describes %d parts", n)
	}
}

func TestSectionsOfAnOrdinaryMail(t *testing.T) {
	raw := "From: a@outside.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nfirst part\r\n" +
		"--x\r\nContent-Type: message/rfc822\r\n\r\nSubject: inside\r\n\r\ninner text\r\n" +
		"--x--\r\n"
	p := parseMessage([]byte(raw))
	if p.opaque {
		t.Fatal("ordinary mail treated as broken")
	}
	sec := func(part []int, spec imap.PartSpecifier) string {
		b, err := p.section(&imap.FetchItemBodySection{Part: part, Specifier: spec})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := sec(nil, imap.PartSpecifierNone); got != raw {
		t.Fatal("BODY[] is not the mail")
	}
	if got := sec([]int{1}, imap.PartSpecifierNone); got != "first part" {
		t.Fatalf("BODY[1] = %q", got)
	}
	if got := sec([]int{1}, imap.PartSpecifierMIME); !strings.HasPrefix(got, "Content-Type: text/plain") {
		t.Fatalf("BODY[1.MIME] = %q", got)
	}
	if got := sec([]int{2}, imap.PartSpecifierHeader); !strings.HasPrefix(got, "Subject: inside") {
		t.Fatalf("BODY[2.HEADER] = %q", got)
	}
	if got := sec([]int{2}, imap.PartSpecifierText); got != "inner text" {
		t.Fatalf("BODY[2.TEXT] = %q", got)
	}
	if got := sec([]int{7}, imap.PartSpecifierNone); got != "" {
		t.Fatalf("BODY[7] = %q", got)
	}
	bs, ok := p.bodyStructure().(*imap.BodyStructureMultiPart)
	if !ok || len(bs.Children) != 2 {
		t.Fatalf("structure = %#v", p.bodyStructure())
	}
	inner, ok := bs.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || inner.MessageRFC822 == nil || inner.MessageRFC822.Envelope.Subject != "inside" || int(inner.Size) != len("Subject: inside\r\n\r\ninner text") {
		t.Fatalf("embedded message = %#v", bs.Children[1])
	}
	first := bs.Children[0].(*imap.BodyStructureSinglePart)
	if int(first.Size) != len(sec([]int{1}, imap.PartSpecifierNone)) {
		t.Fatalf("size of part 1 in the structure is %d, BODY[1] has %d bytes", first.Size, len(sec([]int{1}, imap.PartSpecifierNone)))
	}
}

// Parts are stretches of the mail: asking for them, however often and
// however nested, keeps no copies.
func TestSectionsPointIntoTheMail(t *testing.T) {
	var raw strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&raw, "Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
	}
	raw.WriteString("Content-Type: text/plain\r\n\r\n" + strings.Repeat("Content ", 30000) + "\r\n")
	p := parseMessage([]byte(raw.String()))
	inside := func(b []byte) bool {
		if len(b) == 0 {
			return false
		}
		i := bytes.Index(p.raw, b)
		return i >= 0 && &b[0] == &p.raw[i]
	}
	first, err := p.section(&imap.FetchItemBodySection{Part: []int{1}})
	if err != nil || len(first) < 200000 || !inside(first) {
		t.Fatalf("BODY[1]: %d bytes, inside the mail: %v, %v", len(first), inside(first), err)
	}
	for i := 0; i < 1000; i++ {
		again, err := p.section(&imap.FetchItemBodySection{Part: []int{1}, Partial: &imap.SectionPartial{Offset: int64(i), Size: 10}})
		if err != nil || len(again) != 10 || &again[0] != &first[i] {
			t.Fatalf("repeat %d: extracted again or failed (%v)", i, err)
		}
	}
	path := []int{1}
	for i := 0; i < 19; i++ {
		path = append(path, 1)
		b, err := p.section(&imap.FetchItemBodySection{Part: append([]int{}, path...)})
		if err != nil || !inside(b) {
			t.Fatalf("depth %d: %v, inside the mail: %v", len(path), err, inside(b))
		}
	}
	if p.copied > 64<<10 {
		t.Fatalf("kept %d bytes of copies for a %d byte mail", p.copied, len(p.raw))
	}
	// header selections with names that must not be confused
	h := parseMessage([]byte("A: 1\r\nB: 2\r\nA|+B: 3\r\n\r\nx"))
	one, _ := h.section(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"A", "B"}})
	two, _ := h.section(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"A|+B"}})
	if string(one) != "A: 1\r\nB: 2\r\n\r\n" || string(two) != "A|+B: 3\r\n\r\n" {
		t.Fatalf("header selections mixed up: %q / %q", one, two)
	}
}

// A mail cut short (no closing boundary) still shows its parts, and the
// sizes in the structure are the sizes of what BODY[n] returns.
func TestCutShortMailKeepsItsParts(t *testing.T) {
	raw := "From: a@outside.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nfirst part\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nsecond part, the mail breaks off here"
	p := parseMessage([]byte(raw))
	bs, ok := p.bodyStructure().(*imap.BodyStructureMultiPart)
	if !ok || len(bs.Children) != 2 {
		t.Fatalf("structure = %#v", p.bodyStructure())
	}
	for i, want := range []string{"first part", "second part, the mail breaks off here"} {
		got, err := p.section(&imap.FetchItemBodySection{Part: []int{i + 1}})
		if err != nil || string(got) != want {
			t.Fatalf("BODY[%d] = %q (%v)", i+1, got, err)
		}
		if size := bs.Children[i].(*imap.BodyStructureSinglePart).Size; int(size) != len(got) {
			t.Fatalf("part %d: structure says %d bytes, BODY[%d] has %d", i+1, size, i+1, len(got))
		}
	}
	// a "multipart" without a single part is one part, and that part is there
	empty := parseMessage([]byte("Content-Type: multipart/mixed; boundary=empty\r\n\r\nnot a single part\r\n"))
	sp, ok := empty.bodyStructure().(*imap.BodyStructureSinglePart)
	got, _ := empty.section(&imap.FetchItemBodySection{Part: []int{1}})
	if !ok || string(got) != "not a single part\r\n" || int(sp.Size) != len(got) {
		t.Fatalf("empty multipart: %#v, BODY[1] = %q", empty.bodyStructure(), got)
	}
}
