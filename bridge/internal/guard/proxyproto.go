package guard

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// The PROXY protocol (haproxy, versions 1 and 2) as sent by the Fly proxy
// with the "proxy_proto" handler. Only the source address is used.

var (
	errProxyMissing = errors.New("proxy header missing")
	errProxyInvalid = errors.New("proxy header invalid")
	errProxyLocal   = errors.New("proxy header without client address")
)

var proxyV2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

// readProxyHeader reads exactly one PROXY header from br and returns the
// client address. It never reads beyond the header.
func readProxyHeader(br *bufio.Reader) (netip.Addr, error) {
	first, err := br.Peek(1)
	if err != nil {
		return netip.Addr{}, errProxyMissing
	}
	switch first[0] {
	case 'P':
		return readProxyV1(br)
	case '\r':
		return readProxyV2(br)
	}
	return netip.Addr{}, errProxyMissing
}

// readProxyV1: "PROXY TCP4 1.2.3.4 5.6.7.8 1111 2222\r\n", at most 107 bytes.
func readProxyV1(br *bufio.Reader) (netip.Addr, error) {
	var line []byte
	for len(line) < 107 {
		b, err := br.ReadByte()
		if err != nil {
			return netip.Addr{}, errProxyInvalid
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) || !bytes.HasPrefix(line, []byte("PROXY ")) {
		return netip.Addr{}, errProxyInvalid
	}
	parts := strings.Split(string(line[:len(line)-2]), " ")
	if len(parts) >= 2 && parts[1] == "UNKNOWN" {
		return netip.Addr{}, errProxyLocal
	}
	if len(parts) != 6 || (parts[1] != "TCP4" && parts[1] != "TCP6") {
		return netip.Addr{}, errProxyInvalid
	}
	src, err := netip.ParseAddr(parts[2])
	if err != nil || src.Zone() != "" {
		// an address with a zone ("fe80::1%eth0") is not a client address
		return netip.Addr{}, errProxyInvalid
	}
	if _, err := netip.ParseAddr(parts[3]); err != nil {
		return netip.Addr{}, errProxyInvalid
	}
	if (parts[1] == "TCP4") != src.Is4() {
		return netip.Addr{}, errProxyInvalid
	}
	for _, p := range parts[4:] {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return netip.Addr{}, errProxyInvalid
		}
	}
	return src.Unmap(), nil
}

// readProxyV2: 12-byte signature, version/command, family, length, address
// block (TLVs ignored). The length is capped; Fly sends a few dozen bytes.
func readProxyV2(br *bufio.Reader) (netip.Addr, error) {
	var hdr [16]byte
	if _, err := ioReadFull(br, hdr[:]); err != nil {
		return netip.Addr{}, errProxyInvalid
	}
	if !bytes.Equal(hdr[:12], proxyV2Sig) {
		return netip.Addr{}, errProxyInvalid
	}
	if hdr[12]>>4 != 2 {
		return netip.Addr{}, errProxyInvalid
	}
	cmd := hdr[12] & 0x0f
	fam := hdr[13]
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if n > 512 {
		return netip.Addr{}, errProxyInvalid
	}
	body := make([]byte, n)
	if _, err := ioReadFull(br, body); err != nil {
		return netip.Addr{}, errProxyInvalid
	}
	switch cmd {
	case 0x0: // LOCAL: the proxy itself, no client
		return netip.Addr{}, errProxyLocal
	case 0x1: // PROXY
	default:
		return netip.Addr{}, errProxyInvalid
	}
	switch fam {
	case 0x11: // TCP over IPv4
		if n < 12 {
			return netip.Addr{}, errProxyInvalid
		}
		return netip.AddrFrom4([4]byte(body[0:4])), nil
	case 0x21: // TCP over IPv6
		if n < 36 {
			return netip.Addr{}, errProxyInvalid
		}
		return netip.AddrFrom16([16]byte(body[0:16])).Unmap(), nil
	}
	return netip.Addr{}, errProxyInvalid
}

func ioReadFull(br *bufio.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := br.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
