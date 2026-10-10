package imapd

import (
	"bytes"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// Mail content comes from strangers. Whatever it is: no panic, BODY[] is
// the mail, header plus text is the mail, and the structure stays bounded.
func FuzzMessage(f *testing.F) {
	f.Add([]byte("From: a@b.test\r\nSubject: x\r\n\r\nbody\r\n"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\na\r\n--x\r\nContent-Type: message/rfc822\r\n\r\nSubject: i\r\n\r\nb\r\n--x--\r\n"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=\"\"\r\n\r\n--\r\n\r\n"))
	f.Add([]byte(" leading blank\r\n\r\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<16 {
			return
		}
		p := parseMessage(raw)
		if got, _ := p.section(&imap.FetchItemBodySection{}); !bytes.Equal(got, raw) {
			t.Fatal("BODY[] is not the mail")
		}
		h, _ := p.section(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader})
		b, _ := p.section(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierText})
		if !bytes.Equal(append(append([]byte{}, h...), b...), raw) {
			t.Fatal("header and text do not add up to the mail")
		}
		_ = p.envelope()
		bs := p.bodyStructure()
		if bs == nil {
			t.Fatal("no structure")
		}
		if d := depthOf(bs); d > maxMIMEDepth+1 {
			t.Fatalf("structure is %d deep", d)
		}
		for _, part := range [][]int{{1}, {2}, {1, 1}, {1, 2, 3}} {
			_, _ = p.section(&imap.FetchItemBodySection{Part: part})
			_, _ = p.section(&imap.FetchItemBodySection{Part: part, Specifier: imap.PartSpecifierMIME})
			_, _ = p.section(&imap.FetchItemBodySection{Part: part, Specifier: imap.PartSpecifierHeader})
			_, _ = p.section(&imap.FetchItemBodySection{Part: part, Specifier: imap.PartSpecifierText, Partial: &imap.SectionPartial{Offset: 3, Size: 1 << 62}})
		}
		_, _ = p.section(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject", "From"}})
		_, _ = p.section(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFieldsNot: []string{"Subject"}})
		if p.copied > 2*len(raw)+(128<<10) {
			t.Fatalf("kept %d bytes of copies for a %d byte mail", p.copied, len(raw))
		}
		_ = messageID(raw)
	})
}
