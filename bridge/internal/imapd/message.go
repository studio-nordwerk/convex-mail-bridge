package imapd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
)

// Everything in this file reads mail content, which comes from strangers.
// Rules: sections of the whole message are cut from the raw bytes without
// parsing (byte-exact, tolerant of broken headers); structure is extracted
// with a depth and part limit and without copying part bodies; anything that
// calls into the MIME parser runs under recover.

const (
	maxMIMEDepth    = 24   // nesting of multipart and message/rfc822
	maxMIMEParts    = 2000 // parts described in one BODYSTRUCTURE
	maxSectionDepth = 24   // BODY[1.2.3...] path length
	// BODY[...] items in one FETCH. Mail programs ask for a handful.
	maxSectionsPerFetch = 32
	maxHeaderParse      = 256 << 10
)

// splitMessage returns the header block (including the empty line that ends
// it) and the body. Without an empty line everything is header.
func splitMessage(raw []byte) (header, body []byte) {
	i := 0
	for i < len(raw) {
		nl := bytes.IndexByte(raw[i:], '\n')
		if nl < 0 {
			return raw, nil
		}
		line := raw[i : i+nl]
		next := i + nl + 1
		if len(line) == 0 || (len(line) == 1 && line[0] == '\r') {
			return raw[:next], raw[next:]
		}
		i = next
	}
	return raw, nil
}

// headerFields cuts a header block into fields (each with its continuation
// lines, bytes untouched). The terminating empty line is not included.
func headerFields(header []byte) [][]byte {
	var fields [][]byte
	start := -1
	i := 0
	for i < len(header) {
		nl := bytes.IndexByte(header[i:], '\n')
		end := len(header)
		if nl >= 0 {
			end = i + nl + 1
		}
		line := header[i:end]
		blank := len(bytes.TrimRight(line, "\r\n")) == 0
		cont := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		if blank {
			if start >= 0 {
				fields = append(fields, header[start:i])
				start = -1
			}
			break
		}
		if !cont {
			if start >= 0 {
				fields = append(fields, header[start:i])
			}
			start = i
		} else if start < 0 {
			start = i // continuation without a field: keep the bytes
		}
		i = end
	}
	if start >= 0 && i >= len(header) {
		fields = append(fields, header[start:])
	}
	return fields
}

func fieldName(field []byte) string {
	i := bytes.IndexByte(field, ':')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(strings.TrimRight(string(field[:i]), " \t"))
}

// filterHeader implements HEADER.FIELDS and HEADER.FIELDS.NOT on the raw
// header: the matching fields in their original bytes plus an empty line.
func filterHeader(header []byte, names []string, not bool) []byte {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	var out bytes.Buffer
	for _, f := range headerFields(header) {
		name := fieldName(f)
		if name == "" {
			continue
		}
		if want[name] != not {
			out.Write(f)
			if !bytes.HasSuffix(f, []byte("\n")) {
				out.WriteString("\r\n")
			}
		}
	}
	out.WriteString("\r\n")
	return out.Bytes()
}

func applyPartial(b []byte, p *imap.SectionPartial) []byte {
	if p == nil {
		return b
	}
	if p.Offset < 0 || p.Offset >= int64(len(b)) {
		return nil
	}
	end := int64(len(b))
	if p.Size >= 0 && p.Size < end-p.Offset {
		end = p.Offset + p.Size
	}
	return b[p.Offset:end]
}

// parsed is what is known about one raw mail for FETCH.
type parsed struct {
	raw    []byte
	header []byte
	body   []byte
	// opaque is set when the top-level header cannot be parsed: the mail is
	// then presented as one text/plain part so that it stays readable.
	opaque bool
	th     textproto.Header

	// Sections that were cut out of the mail (parts, header selections),
	// by what was asked for: asking for the same part many times does the
	// work once. A part is a stretch of the mail, so the result normally
	// points into raw and costs no memory; copied counts what does not
	// (re-written headers), and that stays small.
	parts  map[string][]byte
	copied int
	// heavy, if set, brackets the extraction of a part: while it runs, a
	// second copy of up to the whole mail exists.
	heavy func() func()
}

// errSectionBudget: one command asked for more copies of parts than the
// mail is worth.
var errSectionBudget = &imap.Error{
	Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit,
	Text: "Too many body sections in one command",
}

func sectionKey(item *imap.FetchItemBodySection) string {
	var b strings.Builder
	for _, n := range item.Part {
		b.WriteString(strconv.Itoa(n))
		b.WriteByte('.')
	}
	b.WriteByte('|')
	b.WriteString(string(item.Specifier))
	// field names with their length, so that no name can pass for two
	for _, f := range item.HeaderFields {
		fmt.Fprintf(&b, "|+%d:%s", len(f), strings.ToLower(f))
	}
	for _, f := range item.HeaderFieldsNot {
		fmt.Fprintf(&b, "|-%d:%s", len(f), strings.ToLower(f))
	}
	return b.String()
}

func parseMessage(raw []byte) *parsed {
	p := &parsed{raw: raw}
	p.header, p.body = splitMessage(raw)
	h := p.header
	if len(h) > maxHeaderParse {
		h = h[:maxHeaderParse]
	}
	func() {
		defer func() {
			if recover() != nil {
				p.opaque = true
			}
		}()
		th, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(h)))
		p.th = th
		if err != nil {
			p.opaque = true
		}
	}()
	return p
}

// section returns the bytes of a BODY[...] item, or errSectionBudget.
func (p *parsed) section(item *imap.FetchItemBodySection) ([]byte, error) {
	b, err := p.sectionBytes(item)
	if err != nil {
		return nil, err
	}
	return applyPartial(b, item.Partial), nil
}

// sectionBytes returns the whole section, before any partial.
func (p *parsed) sectionBytes(item *imap.FetchItemBodySection) ([]byte, error) {
	if len(item.Part) == 0 && len(item.HeaderFields) == 0 && len(item.HeaderFieldsNot) == 0 {
		// cut from the raw bytes, nothing is copied
		switch item.Specifier {
		case imap.PartSpecifierNone:
			return p.raw, nil
		case imap.PartSpecifierHeader:
			return p.header, nil
		case imap.PartSpecifierText:
			return p.body, nil
		}
		return nil, nil // MIME without a part number does not exist
	}
	key := sectionKey(item)
	if b, ok := p.parts[key]; ok {
		return b, nil
	}
	if p.copied > len(p.raw)+(64<<10) {
		return nil, errSectionBudget
	}
	var done func()
	if p.heavy != nil {
		done = p.heavy()
	}
	b := p.extract(item)
	// The bytes of a part stand in the mail as they are: use those instead
	// of the copy, which can then go.
	if i := bytes.Index(p.raw, b); i >= 0 && len(b) > 0 {
		b = p.raw[i : i+len(b) : i+len(b)]
	} else {
		p.copied += len(b)
	}
	if done != nil {
		done()
	}
	if p.parts == nil {
		p.parts = map[string][]byte{}
	}
	p.parts[key] = b
	return b, nil
}

// extract copies a header selection or a part out of the mail.
func (p *parsed) extract(item *imap.FetchItemBodySection) []byte {
	if len(item.Part) == 0 {
		if item.Specifier != imap.PartSpecifierHeader {
			return nil
		}
		if len(item.HeaderFields) > 0 {
			return filterHeader(p.header, item.HeaderFields, false)
		}
		return filterHeader(p.header, item.HeaderFieldsNot, true)
	}
	if p.opaque {
		// one text/plain part: 1 is the body, 1.MIME its (unusable) header
		if len(item.Part) == 1 && item.Part[0] == 1 {
			switch item.Specifier {
			case imap.PartSpecifierNone, imap.PartSpecifierText:
				return p.body
			}
		}
		return nil
	}
	var out []byte
	func() {
		defer func() {
			if recover() != nil {
				out = nil
			}
		}()
		out = extractPart(p.raw, item)
	}()
	if out == nil && len(item.Part) == 1 && item.Part[0] == 1 &&
		(item.Specifier == imap.PartSpecifierNone || item.Specifier == imap.PartSpecifierText) {
		// A "multipart" without usable parts is described as one part (see
		// structWalker); then part 1 has to be its body.
		if _, single := p.bodyStructure().(*imap.BodyStructureSinglePart); single {
			return p.body
		}
	}
	return out
}

// extractPart returns the bytes of BODY[<part>...]. It follows
// imapserver.ExtractBodySection (go-imap, MIT licence) with one difference:
// a part that ends without its closing boundary (a mail cut short) is
// returned as far as it goes instead of not at all. The structure walker
// counts such a part the same way, so sizes and bytes agree.
func extractPart(raw []byte, item *imap.FetchItemBodySection) []byte {
	br := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return nil
	}
	parentType, header, body := findPart(header, br, item.Part)
	if body == nil {
		return nil
	}
	switch item.Specifier {
	case imap.PartSpecifierHeader, imap.PartSpecifierText:
		header, body = openPart(header, body, parentType)
	}
	if len(item.HeaderFields) > 0 {
		keep := make(map[string]struct{}, len(item.HeaderFields))
		for _, k := range item.HeaderFields {
			keep[strings.ToLower(k)] = struct{}{}
		}
		for field := header.Fields(); field.Next(); {
			if _, ok := keep[strings.ToLower(field.Key())]; !ok {
				field.Del()
			}
		}
	}
	for _, k := range item.HeaderFieldsNot {
		header.Del(k)
	}
	var buf bytes.Buffer
	switch item.Specifier {
	case imap.PartSpecifierHeader, imap.PartSpecifierMIME:
		if err := textproto.WriteHeader(&buf, header); err != nil {
			return nil
		}
	}
	switch item.Specifier {
	case imap.PartSpecifierNone, imap.PartSpecifierText:
		_, _ = io.Copy(&buf, body) // what could be read is the part
	}
	return buf.Bytes()
}

func findPart(header textproto.Header, body io.Reader, path []int) (string, textproto.Header, io.Reader) {
	root := gomessage.Header{Header: header}
	mediaType, _, _ := root.ContentType()
	if !strings.HasPrefix(mediaType, "multipart/") && len(path) > 0 && path[0] == 1 {
		// part 1 of a mail that is not multipart is the mail itself
		path = path[1:]
	}
	parentType := ""
	for _, num := range path {
		header, body = openPart(header, body, parentType)
		h := gomessage.Header{Header: header}
		mediaType, params, _ := h.ContentType()
		if !strings.HasPrefix(mediaType, "multipart/") {
			if num != 1 {
				return "", textproto.Header{}, nil
			}
			continue
		}
		mr := textproto.NewMultipartReader(body, params["boundary"])
		found := false
		for j := 1; j <= num; j++ {
			part, err := mr.NextPart()
			if err != nil || part == nil {
				return "", textproto.Header{}, nil
			}
			if j == num {
				parentType, header, body, found = mediaType, part.Header, part, true
			}
		}
		if !found {
			return "", textproto.Header{}, nil
		}
	}
	return parentType, header, body
}

// openPart steps into an embedded message.
func openPart(header textproto.Header, body io.Reader, parentType string) (textproto.Header, io.Reader) {
	h := gomessage.Header{Header: header}
	mediaType, _, _ := h.ContentType()
	if !h.Has("Content-Type") && parentType == "multipart/digest" {
		mediaType = "message/rfc822"
	}
	if mediaType == "message/rfc822" || mediaType == "message/global" {
		br := bufio.NewReader(body)
		inner, _ := textproto.ReadHeader(br)
		return inner, br
	}
	return header, body
}

func (p *parsed) envelope() *imap.Envelope {
	var env *imap.Envelope
	func() {
		defer func() {
			if recover() != nil {
				env = nil
			}
		}()
		env = imapserver.ExtractEnvelope(p.th)
	}()
	if env == nil {
		env = &imap.Envelope{}
	}
	return env
}

func (p *parsed) bodyStructure() imap.BodyStructure {
	if !p.opaque {
		var bs imap.BodyStructure
		func() {
			defer func() {
				if recover() != nil {
					bs = nil
				}
			}()
			w := &structWalker{}
			bs = w.walk(p.th, bytes.NewReader(p.body), 0)
		}()
		if bs != nil {
			return bs
		}
	}
	return &imap.BodyStructureSinglePart{
		Type: "text", Subtype: "plain", Encoding: "8bit",
		Size:     uint32(len(p.body)),
		Text:     &imap.BodyStructureText{NumLines: int64(bytes.Count(p.body, []byte("\n")))},
		Extended: &imap.BodyStructureSinglePartExt{},
	}
}

// structWalker builds a BODYSTRUCTURE like imapserver.ExtractBodyStructure,
// but bounded: nesting and part count are limited, and part bodies are
// counted while streaming instead of being copied. Part numbers stay the
// same as the ones ExtractBodySection resolves.
type structWalker struct {
	parts int
}

type counter struct {
	r     io.Reader
	bytes int64
	lines int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.bytes += int64(n)
	c.lines += int64(bytes.Count(p[:n], []byte("\n")))
	return n, err
}

func drain(r io.Reader) {
	var buf [8192]byte
	for {
		if _, err := r.Read(buf[:]); err != nil {
			return
		}
	}
}

func (w *structWalker) walk(rawHeader textproto.Header, r io.Reader, depth int) imap.BodyStructure {
	w.parts++
	header := gomessage.Header{Header: rawHeader}
	mediaType, typeParams, _ := header.ContentType()
	primaryType, subType, _ := strings.Cut(mediaType, "/")

	cr := &counter{r: r}
	if primaryType == "multipart" && depth < maxMIMEDepth && typeParams["boundary"] != "" {
		bs := &imap.BodyStructureMultiPart{Subtype: subType}
		mr := textproto.NewMultipartReader(cr, typeParams["boundary"])
		for w.parts < maxMIMEParts {
			part, _ := mr.NextPart()
			if part == nil {
				break
			}
			bs.Children = append(bs.Children, w.walk(part.Header, part, depth+1))
		}
		if len(bs.Children) > 0 {
			bs.Extended = &imap.BodyStructureMultiPartExt{
				Params:      typeParams,
				Disposition: contentDisposition(header),
				Language:    contentLanguage(header),
				Location:    header.Get("Content-Location"),
			}
			return bs
		}
		// A multipart without a single part cannot be described (and the
		// library panics on it): describe it as a leaf. What was read so
		// far is already counted; the rest follows below.
		primaryType, subType = "application", "octet-stream"
		typeParams = nil
	} else if primaryType == "multipart" {
		primaryType, subType = "application", "octet-stream"
		typeParams = nil
	}
	if primaryType == "" {
		primaryType, subType = "text", "plain"
	}

	bs := &imap.BodyStructureSinglePart{
		Type:        primaryType,
		Subtype:     subType,
		Params:      typeParams,
		ID:          header.Get("Content-Id"),
		Description: header.Get("Content-Description"),
		Encoding:    header.Get("Content-Transfer-Encoding"),
	}
	isMessage := primaryType == "message" && (subType == "rfc822" || subType == "global")
	if isMessage && depth < maxMIMEDepth && w.parts < maxMIMEParts {
		br := bufio.NewReader(cr)
		childHeader, _ := textproto.ReadHeader(br)
		child := w.walk(childHeader, br, depth+1)
		drain(br)
		bs.MessageRFC822 = &imap.BodyStructureMessageRFC822{
			Envelope:      imapserver.ExtractEnvelope(childHeader),
			BodyStructure: child,
			NumLines:      cr.lines,
		}
	} else {
		if isMessage {
			// too deep to describe as a message: offer it as a file
			bs.Type, bs.Subtype = "application", "octet-stream"
		}
		drain(cr)
	}
	bs.Size = uint32(cr.bytes)
	if bs.Type == "text" {
		bs.Text = &imap.BodyStructureText{NumLines: cr.lines}
	}
	bs.Extended = &imap.BodyStructureSinglePartExt{
		Disposition: contentDisposition(header),
		Language:    contentLanguage(header),
		Location:    header.Get("Content-Location"),
	}
	return bs
}

func contentDisposition(header gomessage.Header) *imap.BodyStructureDisposition {
	disp, params, _ := header.ContentDisposition()
	if disp == "" {
		return nil
	}
	return &imap.BodyStructureDisposition{Value: disp, Params: params}
}

func contentLanguage(header gomessage.Header) []string {
	v := header.Get("Content-Language")
	if v == "" {
		return nil
	}
	l := strings.Split(v, ",")
	for i := range l {
		l[i] = strings.TrimSpace(l[i])
	}
	return l
}

// messageID returns the Message-ID of a raw mail the way it stands in the
// header: the first "<...>" token, or the trimmed value without one.
func messageID(raw []byte) string {
	header, _ := splitMessage(raw)
	if len(header) > maxHeaderParse {
		header = header[:maxHeaderParse]
	}
	for _, f := range headerFields(header) {
		if fieldName(f) != "message-id" {
			continue
		}
		v := string(f[bytes.IndexByte(f, ':')+1:])
		v = strings.Join(strings.Fields(v), " ")
		if i := strings.IndexByte(v, '<'); i >= 0 {
			if j := strings.IndexByte(v[i:], '>'); j > 0 {
				return v[i : i+j+1]
			}
		}
		return v
	}
	return ""
}
