package logx

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventIsOneCleanJSONLine(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Event("test event\n\"x\"", Code("évil\"\r\n{}@value"), MB(HashAddress("Mara@Mailbox-One.test ")), N("n", 7), Ms(1500*time.Millisecond),
		Code(strings.Repeat("a", 500)), Frame(0, "pkg/path.(*T).Method"))
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("not one line: %q", out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("not JSON: %q (%v)", out, err)
	}
	if strings.ContainsAny(out[:len(out)-1], "\r\n@ ") {
		t.Fatalf("unclean characters: %q", out)
	}
	if obj["ms"] != float64(1500) || obj["n"] != float64(7) {
		t.Fatalf("numbers: %v", obj)
	}
	if HashAddress("mara@mailbox-one.test") != HashAddress(" MARA@mailbox-one.TEST") || len(HashAddress("x")) != 12 {
		t.Fatal("HashAddress is not stable across spelling")
	}
	var nilLogger *Logger
	nilLogger.Event("nothing") // must not panic
}
