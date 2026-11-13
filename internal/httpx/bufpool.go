package httpx

import (
	"bufio"
	"io"
	"sync"
)

// BufferSize is the size of the pooled readers and writers that sit on
// every client and upstream connection. Bodies mostly bypass them: large
// reads go straight into the copy buffer and large writes straight to the
// socket.
const BufferSize = 4 << 10

var (
	readers = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, BufferSize) }}
	writers = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, BufferSize) }}
)

// NewReader returns a pooled reader of BufferSize reading from r. Return
// it with PutReader once nothing can use it again.
func NewReader(r io.Reader) *bufio.Reader {
	br := readers.Get().(*bufio.Reader)
	br.Reset(r)
	return br
}

// PutReader returns br to the pool. Bytes still buffered are dropped.
func PutReader(br *bufio.Reader) {
	br.Reset(nil)
	readers.Put(br)
}

// NewWriter returns a pooled writer of BufferSize writing to w. Return it
// with PutWriter once nothing can use it again.
func NewWriter(w io.Writer) *bufio.Writer {
	bw := writers.Get().(*bufio.Writer)
	bw.Reset(w)
	return bw
}

// PutWriter returns bw to the pool. Bytes still buffered are dropped.
func PutWriter(bw *bufio.Writer) {
	bw.Reset(nil)
	writers.Put(bw)
}
