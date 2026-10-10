package guard

import (
	"bufio"
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

func TestLiteralMarker(t *testing.T) {
	cases := []struct {
		line    string
		size    int64
		nonSync bool
		ok      bool
	}{
		{"a LOGIN {5}\r", 5, false, true},
		{"a LOGIN {5}", 5, false, true},
		{"a LOGIN {5+}\r", 5, true, true},
		{"a APPEND Sent (\\Seen) {310} \r", 310, false, true}, // blank before CRLF, as the library accepts it
		{"a APPEND Sent ~{12}\r", 12, false, true},
		{"{0}\r", 0, false, true},
		{"a LOGIN {99999999999999999999}\r", 1<<63 - 1, false, true},
		{"a NOOP\r", 0, false, false},
		{"a LOGIN user pass}\r", 0, false, false},
		{"a LOGIN {}\r", 0, false, false},
		{"a LOGIN {+}\r", 0, false, false},
		{"a LOGIN {5} x\r", 0, false, false},
		{"a LOGIN \"{5}\"\r", 0, false, false},
		{"a SEARCH SUBJECT {5}  \r", 0, false, false}, // two blanks: not a literal for the library either
		{"}", 0, false, false},
		{"", 0, false, false},
		// padding does not hide an announcement
		{"a NOOP {" + strings.Repeat("0", 64) + "40+}\r", 40, true, true},
		{"a LOGIN {" + strings.Repeat("0", 500) + "5}\r", 5, false, true},
		{"a LOGIN {5}\r\r", 0, false, false},
		{"a LOGIN {5+}}\r", 0, false, false},
		{"a LOGIN {5{6}\r", 6, false, true},
		{"a LOGIN {5 }\r", 0, false, false},
	}
	for _, tc := range cases {
		size, nonSync, ok := literalMarker([]byte(tc.line))
		if ok != tc.ok || (ok && (size != tc.size || nonSync != tc.nonSync)) {
			t.Errorf("%q: got %d %v %v, want %d %v %v", tc.line, size, nonSync, ok, tc.size, tc.nonSync, tc.ok)
		}
	}
}

func TestProxyHeader(t *testing.T) {
	v2 := func(verCmd, fam byte, body []byte) string {
		b := append([]byte{}, proxyV2Sig...)
		b = append(b, verCmd, fam, byte(len(body)>>8), byte(len(body)))
		return string(append(b, body...))
	}
	v4 := []byte{203, 0, 113, 9, 10, 0, 0, 1, 0xC8, 0x00, 0x03, 0xE1}
	v6 := append(append(netip.MustParseAddr("2001:db8::9").AsSlice(), netip.MustParseAddr("2001:db8::1").AsSlice()...), 1, 2, 3, 4)
	cases := []struct {
		name string
		in   string
		want string // address, or "" for an error
		rest string // what must be left in the stream
	}{
		{"v1 tcp4", "PROXY TCP4 203.0.113.9 10.0.0.1 51200 993\r\n* rest", "203.0.113.9", "* rest"},
		{"v1 tcp6", "PROXY TCP6 2001:db8::9 2001:db8::1 51200 993\r\nrest", "2001:db8::9", "rest"},
		{"v1 mapped", "PROXY TCP6 ::ffff:203.0.113.9 ::1 1 2\r\n", "203.0.113.9", ""},
		{"v1 unknown", "PROXY UNKNOWN\r\n", "", ""},
		{"v1 family mismatch", "PROXY TCP4 2001:db8::9 2001:db8::1 1 2\r\n", "", ""},
		{"v1 bad port", "PROXY TCP4 203.0.113.9 10.0.0.1 99999 993\r\n", "", ""},
		{"v1 missing fields", "PROXY TCP4 203.0.113.9\r\n", "", ""},
		{"v1 only LF", "PROXY TCP4 203.0.113.9 10.0.0.1 1 2\n", "", ""},
		{"v1 too long", "PROXY TCP4 " + strings.Repeat("9", 120) + "\r\n", "", ""},
		{"v1 never ends", "PROXY TCP4 203.0.113.9", "", ""},
		{"v2 tcp4", v2(0x21, 0x11, v4) + "rest", "203.0.113.9", "rest"},
		{"v2 tcp6", v2(0x21, 0x21, v6), "2001:db8::9", ""},
		{"v2 with extra bytes (TLV)", v2(0x21, 0x11, append(append([]byte{}, v4...), 1, 0, 2, 0xAA, 0xBB)) + "x", "203.0.113.9", "x"},
		{"v2 local", v2(0x20, 0x00, nil), "", ""},
		{"v2 wrong version", v2(0x11, 0x11, v4), "", ""},
		{"v2 unix family", v2(0x21, 0x31, make([]byte, 216)), "", ""},
		{"v2 short address", v2(0x21, 0x11, v4[:8]), "", ""},
		{"v2 huge length", string(proxyV2Sig) + "\x21\x11\xff\xff", "", ""},
		{"v2 cut off", string(proxyV2Sig[:8]), "", ""},
		{"not a header", "a001 NOOP\r\n", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		br := bufio.NewReader(strings.NewReader(tc.in))
		addr, err := readProxyHeader(br)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: accepted as %v", tc.name, addr)
			}
			continue
		}
		if err != nil || addr.String() != tc.want {
			t.Errorf("%s: got %v %v, want %s", tc.name, addr, err, tc.want)
			continue
		}
		var rest bytes.Buffer
		_, _ = rest.ReadFrom(br)
		if rest.String() != tc.rest {
			t.Errorf("%s: the stream continues with %q, want %q", tc.name, rest.String(), tc.rest)
		}
	}
}

func TestIPKey(t *testing.T) {
	a := ipKey(netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	b := ipKey(netip.MustParseAddr("2001:db8:1:2:bbbb::2"))
	c := ipKey(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b || a == c {
		t.Fatalf("IPv6 keys: %v %v %v", a, b, c)
	}
	if ipKey(netip.MustParseAddr("::ffff:192.0.2.1")) != ipKey(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("mapped IPv4 has its own key")
	}
	if ipKey(netip.MustParseAddr("192.0.2.1")) == ipKey(netip.MustParseAddr("192.0.2.2")) {
		t.Fatal("two IPv4 addresses share a key")
	}
}
