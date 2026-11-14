package httpx

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestNetHTTPReadRequest is smuggling step 1 (PLAN.md Phase 8): one case
// per framing ambiguity, pinning what http.ReadRequest on its own rejects
// and what it accepts and quietly normalises. docs/SMUGGLING.md is the
// table of these results. If a Go upgrade changes one, this test fails, and
// the table and CheckRaw need another look.
//
// For an accepted request, want describes what the proxy would forward:
// the framing (cl: ContentLength, te: TransferEncoding), the body, what is
// left on the connection after it, and any header or trailer value of
// note.
func TestNetHTTPReadRequest(t *testing.T) {
	const reject = "reject"
	cases := []struct {
		name, raw, want string
	}{
		// Content-Length and Transfer-Encoding together: chunked wins, the
		// Content-Length is dropped, the rest of the "CL body" stays.
		{"CL and TE", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 6\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nX", `cl=-1 te=[chunked] body="" rest="X"`},
		{"TE and CL", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\nContent-Length: 6\r\n\r\n0\r\n\r\nX", `cl=-1 te=[chunked] body="" rest="X"`},
		// Content-Length disguises.
		{"CL twice, same value", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\nContent-Length: 3\r\n\r\nabc", `cl=3 te=[] body="abc" rest=""`},
		{"CL twice, different", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\nContent-Length: 4\r\n\r\nabcd", reject},
		{"CL list", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 3, 3\r\n\r\nabc", reject},
		{"CL plus sign", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: +3\r\n\r\nabc", reject},
		{"CL hex", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 0x3\r\n\r\nabc", reject},
		{"CL negative", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: -1\r\n\r\nabc", reject},
		{"CL inner space", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 1 2\r\n\r\nabc", reject},
		{"CL leading zeros", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 003\r\n\r\nabc", `cl=3 te=[] body="abc" rest=""`},
		{"CL overflow", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 99999999999999999999\r\n\r\nabc", reject},
		{"CL empty", "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: \r\n\r\nabc", reject},
		// Transfer-Encoding disguises.
		{"TE gzip, chunked", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: gzip, chunked\r\n\r\n0\r\n\r\n", reject},
		{"TE chunked, identity", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked, identity\r\n\r\n0\r\n\r\n", reject},
		{"TE identity", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: identity\r\n\r\n", reject},
		{"TE xchunked", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: xchunked\r\n\r\n0\r\n\r\n", reject},
		{"TE empty", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: \r\n\r\n", reject},
		{"TE twice", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", reject},
		{"TE Chunked", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: Chunked\r\n\r\n0\r\n\r\n", `cl=-1 te=[chunked] body="" rest=""`},
		{"TE tab before value", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding:\tchunked\r\n\r\n0\r\n\r\n", `cl=-1 te=[chunked] body="" rest=""`},
		{"TE trailing space", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked \r\n\r\n0\r\n\r\n", `cl=-1 te=[chunked] body="" rest=""`},
		{"TE vertical tab", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: \x0bchunked\r\n\r\n0\r\n\r\n", reject},
		// HTTP/1.0 with chunked: the encoding is ignored and the chunked
		// body is left on the connection, to be read as the next request.
		{"TE in HTTP/1.0", "POST / HTTP/1.0\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", `cl=0 te=[] body="" rest="0\r\n\r\n"`},
		// Whitespace before the colon: a space is accepted and the field
		// is not recognised, so a backend that trims it sees chunked and
		// the proxy sees no body. A tab is rejected.
		{"space before colon", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding : chunked\r\n\r\n0\r\n\r\n", `cl=0 te=[] body="" rest="0\r\n\r\n"`},
		{"tab before colon", "GET / HTTP/1.1\r\nHost: a\r\nX-A\t: 1\r\n\r\n", reject},
		// Line endings.
		{"bare LF everywhere", "GET / HTTP/1.1\nHost: a\nX-A: 1\n\n", `cl=0 te=[] body="" rest="" X-A=[1]`},
		{"bare LF on one line", "GET / HTTP/1.1\r\nHost: a\nX-A: 1\r\n\r\n", `cl=0 te=[] body="" rest="" X-A=[1]`},
		{"bare CR in a value", "GET / HTTP/1.1\r\nHost: a\r\nX-A: 1\r2\r\n\r\n", reject},
		{"leading CRLF", "\r\nGET / HTTP/1.1\r\nHost: a\r\n\r\n", reject},
		// Folding: continuation lines are joined into the field before,
		// whatever they say.
		{"folded value", "GET / HTTP/1.1\r\nHost: a\r\nX-A: 1\r\n 2\r\n\r\n", `cl=0 te=[] body="" rest="" X-A=[1 2]`},
		{"TE folded into a value", "POST / HTTP/1.1\r\nHost: a\r\nX-A: 1\r\n Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", `cl=0 te=[] body="" rest="0\r\n\r\n" X-A=[1 Transfer-Encoding: chunked]`},
		{"first field folded", "GET / HTTP/1.1\r\n Host: a\r\n\r\n", reject},
		// Field names and values.
		{"NUL in a value", "GET / HTTP/1.1\r\nHost: a\r\nX-A: 1\x002\r\n\r\n", reject},
		{"control byte in a name", "GET / HTTP/1.1\r\nHost: a\r\nX\x01A: 1\r\n\r\n", reject},
		{"empty name", "GET / HTTP/1.1\r\nHost: a\r\n: 1\r\n\r\n", reject},
		{"no colon", "GET / HTTP/1.1\r\nHost: a\r\nXA\r\n\r\n", reject},
		{"obs-text value", "GET / HTTP/1.1\r\nHost: a\r\nX-A: \xff\r\n\r\n", `cl=0 te=[] body="" rest="" X-A=[` + "\xff" + `]`},
		// Host.
		{"Host twice", "GET / HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n", reject},
		{"Host with a space", "GET / HTTP/1.1\r\nHost: a, b\r\n\r\n", `cl=0 te=[] body="" rest="" host="a, b"`},
		{"absolute target and Host", "GET http://b/ HTTP/1.1\r\nHost: a\r\n\r\n", `cl=0 te=[] body="" rest="" host="b"`},
		// Request line.
		{"two spaces", "GET  / HTTP/1.1\r\nHost: a\r\n\r\n", reject},
		{"space in target", "GET /a b HTTP/1.1\r\nHost: a\r\n\r\n", reject},
		{"lower-case version", "GET / http/1.1\r\nHost: a\r\n\r\n", reject},
		{"HTTP/1.2", "GET / HTTP/1.2\r\nHost: a\r\n\r\n", `cl=0 te=[] body="" rest="" proto=HTTP/1.2`},
		{"HTTP/2.0", "GET / HTTP/2.0\r\nHost: a\r\n\r\n", `cl=0 te=[] body="" rest="" proto=HTTP/2.0`},
		// Chunked bodies: errors surface when the body is read.
		{"chunk extension", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n3;x=y\r\nabc\r\n0\r\n\r\n", `cl=-1 te=[chunked] body="abc" rest=""`},
		{"chunk size with space", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n3 \r\nabc\r\n0\r\n\r\n", `cl=-1 te=[chunked] body="abc" rest=""`},
		{"chunk line bare LF", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n3\nabc\n0\n\n", `cl=-1 te=[chunked] body error: chunked line ends with bare LF`},
		{"chunk size overflow", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\nfffffffffffffffff1\r\nabc\r\n0\r\n\r\n", `cl=-1 te=[chunked] body error: http chunk length too large`},
		{"chunk size 0x", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0x3\r\nabc\r\n0\r\n\r\n", `cl=-1 te=[chunked] body error: invalid byte in chunk length`},
		{"chunk data too long", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabcX\r\n0\r\n\r\n", `cl=-1 te=[chunked] body error: malformed chunked encoding`},
		// Trailers: any field is accepted, framing fields included.
		{"trailer", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nX-T: 1\r\n\r\n", `cl=-1 te=[chunked] body="" rest="" trailer=map[X-T:[1]]`},
		{"Content-Length trailer", "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nContent-Length: 5\r\n\r\n", `cl=-1 te=[chunked] body="" rest="" trailer=map[Content-Length:[5]]`},
		{"declared trailer forbidden", "POST / HTTP/1.1\r\nHost: a\r\nTrailer: Content-Length\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", reject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describe(tc.raw); got != tc.want {
				t.Errorf("http.ReadRequest:\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

// describe reads raw with http.ReadRequest and summarises the outcome in
// TestNetHTTPReadRequest's terms.
func describe(raw string) string {
	br := bufio.NewReader(strings.NewReader(raw))
	req, err := http.ReadRequest(br)
	if err != nil {
		return "reject"
	}
	var b strings.Builder
	b.WriteString("cl=")
	b.WriteString(itoa(req.ContentLength))
	b.WriteString(" te=[")
	b.WriteString(strings.Join(req.TransferEncoding, " "))
	b.WriteString("]")
	body, berr := io.ReadAll(req.Body)
	if berr != nil {
		b.WriteString(" body error: " + berr.Error())
		return b.String()
	}
	rest, _ := io.ReadAll(br)
	b.WriteString(" body=" + quote(string(body)) + " rest=" + quote(string(rest)))
	if v, ok := req.Header["X-A"]; ok {
		b.WriteString(" X-A=[" + strings.Join(v, " ") + "]")
	}
	if req.Host != "a" {
		b.WriteString(" host=" + quote(req.Host))
	}
	if req.Proto != "HTTP/1.1" && req.Proto != "HTTP/1.0" {
		b.WriteString(" proto=" + req.Proto)
	}
	if len(req.Trailer) > 0 {
		b.WriteString(" trailer=map[")
		first := true
		for k, v := range req.Trailer {
			if !first {
				b.WriteString(" ")
			}
			first = false
			b.WriteString(k + ":[" + strings.Join(v, " ") + "]")
		}
		b.WriteString("]")
	}
	return b.String()
}

func itoa(n int64) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func quote(s string) string {
	r := strings.NewReplacer("\r", `\r`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}
