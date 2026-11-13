package httpx

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// A pooled reader or writer comes back empty and bound to its new
// connection, whatever the last user left in it.
func TestBufferPoolResets(t *testing.T) {
	br := NewReader(strings.NewReader("leftover bytes"))
	br.ReadByte()
	PutReader(br)
	br = NewReader(strings.NewReader("new"))
	if got, _ := io.ReadAll(br); string(got) != "new" {
		t.Fatalf("reader gave %q", got)
	}
	PutReader(br)

	var old, cur bytes.Buffer
	bw := NewWriter(&old)
	bw.WriteString("unflushed")
	PutWriter(bw)
	bw = NewWriter(&cur)
	bw.WriteString("new")
	bw.Flush()
	if cur.String() != "new" || old.Len() != 0 {
		t.Fatalf("writer wrote %q to the new target and %q to the old", cur.String(), old.String())
	}
	if bw.Size() != BufferSize {
		t.Fatalf("size %d", bw.Size())
	}
	PutWriter(bw)
}
