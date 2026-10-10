package edgestub

import (
	"encoding/base64"
	"fmt"
	"mime"
	"strings"
	"sync/atomic"
	"time"
)

// MailSpec describes a test mail. All addresses are invented.
type MailSpec struct {
	From       string
	To         []string
	Cc         []string
	Subject    string
	Date       time.Time
	MessageID  string   // without angle brackets; generated if empty
	InReplyTo  string   // without angle brackets
	References []string // the whole chain, oldest first; defaults to InReplyTo
	Text       string
	HTML       string // makes the mail multipart/alternative
	AttachName string // makes the mail multipart/mixed
	AttachType string // media type of the attachment, default application/octet-stream
	AttachData []byte
}

var mailCounter atomic.Int64

// BuildMail renders a spec to RFC 5322 bytes with CRLF line ends.
func BuildMail(m MailSpec) []byte {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\r\n")
	}
	if m.Date.IsZero() {
		m.Date = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	}
	if m.MessageID == "" {
		m.MessageID = fmt.Sprintf("stub-%d-%d@mail.example.test", m.Date.Unix(), mailCounter.Add(1))
	}
	line("From: %s", m.From)
	if len(m.To) > 0 {
		line("To: %s", strings.Join(m.To, ", "))
	}
	if len(m.Cc) > 0 {
		line("Cc: %s", strings.Join(m.Cc, ", "))
	}
	line("Subject: %s", mime.QEncoding.Encode("utf-8", m.Subject))
	line("Date: %s", m.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	line("Message-ID: <%s>", m.MessageID)
	if m.InReplyTo != "" {
		line("In-Reply-To: <%s>", m.InReplyTo)
	}
	refs := m.References
	if len(refs) == 0 && m.InReplyTo != "" {
		refs = []string{m.InReplyTo}
	}
	if len(refs) > 0 {
		// one identifier per line keeps long chains within the line limit
		line("References: <%s>", strings.Join(refs, ">\r\n <"))
	}
	line("MIME-Version: 1.0")

	textPart := func() {
		line("Content-Type: text/plain; charset=utf-8")
		line("Content-Transfer-Encoding: 8bit")
		line("")
		for _, l := range strings.Split(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n") {
			line("%s", l)
		}
	}
	altPart := func(boundary string) {
		line("Content-Type: multipart/alternative; boundary=%q", boundary)
		line("")
		line("--%s", boundary)
		textPart()
		line("--%s", boundary)
		line("Content-Type: text/html; charset=utf-8")
		line("Content-Transfer-Encoding: 8bit")
		line("")
		for _, l := range strings.Split(strings.ReplaceAll(m.HTML, "\r\n", "\n"), "\n") {
			line("%s", l)
		}
		line("--%s--", boundary)
	}
	switch {
	case m.AttachName != "":
		line("Content-Type: multipart/mixed; boundary=\"mix-%d\"", len(m.AttachData))
		line("")
		line("--mix-%d", len(m.AttachData))
		if m.HTML != "" {
			altPart("alt-inner")
		} else {
			textPart()
		}
		line("--mix-%d", len(m.AttachData))
		attachType := m.AttachType
		if attachType == "" {
			attachType = "application/octet-stream"
		}
		line("Content-Type: %s; name=%q", attachType, m.AttachName)
		line("Content-Disposition: attachment; filename=%q", m.AttachName)
		line("Content-Transfer-Encoding: base64")
		line("")
		enc := base64.StdEncoding.EncodeToString(m.AttachData)
		for len(enc) > 76 {
			line("%s", enc[:76])
			enc = enc[76:]
		}
		line("%s", enc)
		line("--mix-%d--", len(m.AttachData))
	case m.HTML != "":
		altPart("alt-outer")
	default:
		textPart()
	}
	return []byte(b.String())
}
