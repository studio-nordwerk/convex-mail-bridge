package guard

import (
	"bufio"
	"bytes"
	"testing"
)

// The PROXY header is the first thing an unknown peer can send.
func FuzzProxyHeader(f *testing.F) {
	f.Add([]byte("PROXY TCP4 203.0.113.9 10.0.0.1 51200 993\r\n"))
	f.Add([]byte("PROXY TCP6 2001:db8::9 2001:db8::1 1 2\r\nrest"))
	f.Add(append(append([]byte{}, proxyV2Sig...), 0x21, 0x11, 0, 12, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12))
	f.Add([]byte("PROXY UNKNOWN\r\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, in []byte) {
		br := bufio.NewReader(bytes.NewReader(in))
		addr, err := readProxyHeader(br)
		if err == nil && !addr.IsValid() {
			t.Fatalf("accepted %q without an address", in)
		}
	})
}

func FuzzLiteralMarker(f *testing.F) {
	f.Add([]byte("a LOGIN {5}\r"))
	f.Add([]byte("a APPEND Sent {310+} \r"))
	f.Add([]byte("}"))
	f.Fuzz(func(t *testing.T, line []byte) {
		size, _, ok := literalMarker(line)
		if ok && size < 0 {
			t.Fatalf("%q: negative size", line)
		}
	})
}
