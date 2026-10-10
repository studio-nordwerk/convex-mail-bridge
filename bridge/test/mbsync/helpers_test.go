//go:build mbsync || mbsyncedge

package mbsync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type maildirFile struct {
	path   string
	flags  string // the letters after ":2,"
	digest string // of the content with CRLF line ends, as the edge has it
}

// readMaildir lists the mails of a Maildir folder.
func readMaildir(t *testing.T, dir string) []maildirFile {
	t.Helper()
	var out []maildirFile
	for _, sub := range []string{"cur", "new"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		for _, e := range entries {
			p := filepath.Join(dir, sub, e.Name())
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			// Maildir keeps LF; the edge has CRLF. mbsync also adds its own
			// X-TUID header line to mails it has to recognize again.
			data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
			if head, body, ok := bytes.Cut(data, []byte("\n\n")); ok {
				var kept [][]byte
				for _, line := range bytes.Split(head, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("X-TUID: ")) {
						kept = append(kept, line)
					}
				}
				data = append(append(bytes.Join(kept, []byte("\n")), '\n', '\n'), body...)
			}
			data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
			flags := ""
			if i := strings.Index(e.Name(), ":2,"); i >= 0 {
				flags = e.Name()[i+3:]
			}
			out = append(out, maildirFile{path: p, flags: flags, digest: sha(data)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].digest < out[j].digest })
	return out
}

func names(files []maildirFile) string {
	var l []string
	for _, f := range files {
		l = append(l, filepath.Base(f.path))
	}
	sort.Strings(l)
	return strings.Join(l, "\n")
}
