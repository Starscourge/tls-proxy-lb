# Request smuggling

Request smuggling needs two HTTP parsers on one path that disagree about
where a request ends. One of them reads part of a request as the start of
the next, and that hidden second request bypasses whatever the first parser
was meant to enforce. In a CL.TE attack the front end trusts
`Content-Length` and the back end trusts `Transfer-Encoding: chunked`; in
TE.CL it is the other way round; in TE.TE both honour chunked but one is
fooled into ignoring a disguised `Transfer-Encoding`.

This proxy can be either half of such a pair. Behind a CDN or another
proxy, it is the back end. In front of the backends, it is the front end.
The defences are:

1. **Refuse what is ambiguous.** A request whose framing two reasonable
   parsers could read differently is answered 400 and its connection
   closed, before anything is forwarded (`httpx.CheckRaw`, below).
2. **Re-serialise everything.** Requests are written upstream by
   `Request.Write` from the parsed request, never copied as bytes, so a
   backend sees exactly one framing: the proxy's. `FuzzRequestPath` checks
   that whatever comes in, what goes out parses as exactly one request.
3. **Close after any error.** Every response the proxy generates closes
   the client connection, so bytes after a bad request are never read as
   another request.

## Step 1: what `http.ReadRequest` does on its own

`internal/httpx/readrequest_test.go` (`TestNetHTTPReadRequest`) pins one
case per ambiguity, so a Go upgrade that changes one fails the test. Go
1.27.4:

| Case | `http.ReadRequest` | What it would forward |
| --- | --- | --- |
| `Content-Length` and `Transfer-Encoding: chunked` | **accepts** | chunked; the CL is dropped and the rest of the "CL body" stays on the connection |
| Two `Content-Length`, same value | **accepts** | one CL |
| Two `Content-Length`, different | rejects | |
| CL as a list, `+5`, `0x5`, `-1`, `1 2`, empty, past 64 bits | rejects | |
| CL with leading zeros (`005`) | accepts | 5 (digits are digits) |
| TE `gzip, chunked`, `chunked, identity`, `identity`, `xchunked`, empty | rejects | |
| Two `Transfer-Encoding` fields | rejects | |
| TE `Chunked`, tab before the value, trailing space | accepts | chunked (transfer-codings are case-insensitive; OWS is allowed) |
| TE with a vertical tab | rejects | |
| **TE on an HTTP/1.0 request** | **accepts** | no body; the chunked body stays on the connection as the "next request" |
| **Space between a field name and its colon** (`Transfer-Encoding : chunked`) | **accepts** | the field is not recognised, so no body, where a lenient parser sees chunked |
| Tab before the colon | rejects | |
| **Bare LF line endings** (anywhere, or only on some lines) | **accepts** | |
| A CR alone inside a line | rejects | |
| A request preceded by an empty line | rejects | |
| **A folded line** (`obs-fold`), including one that spells `Transfer-Encoding: chunked` | **accepts** | joined into the previous field's value |
| The first field folded | rejects | |
| NUL or a control byte in a name or value, an empty name, no colon | rejects | |
| obs-text (bytes over 0x7F) in a value | accepts | as is (RFC 9110 allows it) |
| Two `Host` fields | rejects | |
| **A `Host` that is not a host** (`a, b`, `a b`, `x@a`) | **accepts** | as is |
| Absolute-form target with a different `Host` | accepts | the target's host (RFC 9112 section 3.2.2) |
| Two spaces, a space in the target, a lower-case version | rejects | |
| HTTP/1.2, HTTP/2.0 | accepts | (the proxy answers 505 to anything but 1.x) |
| **An opaque target** (`a:0`) | **accepts** | `Request.Write` sends `0` (found by `FuzzRequestPath`) |
| Chunk line with bare LF, chunk size past 64 bits, `0x` chunk size, more data than the size | accepts the header | the body read fails at the bad chunk |
| Chunk extension, a space after the chunk size | accepts | the body, re-chunked without the extension |
| **A `Content-Length` (or `Host`, ...) field in the trailer** | **accepts** | forwarded in the upstream trailer |
| A `Trailer` header declaring `Content-Length` | rejects | |

The bold rows are the ones the proxy must handle itself.

## Step 2: the raw-header check

`ReadRequest` records the header block exactly as it arrived (the
`HeaderLimitReader` beneath the `bufio.Reader` copies what it reads,
starting with whatever the reader already buffered from a pipelined
request). Once `http.ReadRequest` has accepted it, `CheckRaw` scans it
without allocating and refuses, with 400 and a closed connection:

| Refused | Reason label |
| --- | --- |
| Both `Content-Length` and `Transfer-Encoding` | `cl_and_te` |
| More than one `Content-Length`, even equal | `duplicate_cl` |
| More than one `Transfer-Encoding`, or one that is not `chunked` (any case, OWS) | `bad_te` |
| `Transfer-Encoding` on an HTTP/1.0 request | `te_in_http10` |
| Whitespace between a field name and its colon | `space_before_colon` |
| A line not ending in CRLF | `bare_lf` |
| A CR not followed by LF | `bare_cr` |
| A line starting with a space or tab | `folded_header` |

After it come checks on the parsed request: a target that is origin-form,
http(s) absolute-form or `OPTIONS *` (`bad_target`), and a `Host` made of
host bytes (`invalid_host`, the table `net/http`'s own server uses). At
the end of a chunked body, framing, routing and hop-by-hop fields are
removed from the trailer before it is forwarded
(`httpx.SanitizeTrailer`). Each refusal is counted in
`proxy_bad_requests_total{reason}`.

## The corpus

`test/integration/testdata/smuggling/` holds 46 payloads, one per file,
written as Go string bodies so every CR and LF is visible. Each one ends
with `GET /smuggled-<case>`: a request that only a parser that disagreed
with the proxy about the first request's end would see.
`TestSmugglingCorpus` sends each over HTTP and HTTPS and requires a 400,
`Connection: close`, a closed connection, and no `/smuggled` path in the
backend's request log. The five files in `ok/` are well-formed (a CL body
then a pipelined request, a chunked body with an extension and a trailer
then a pipelined request, `Chunked`, `Content-Length: 005`, an
absolute-form target) and must be served, every request in them, so the
test cannot pass by refusing everything.

Run against the Phase 7 proxy (`net/http`'s checks only), plain HTTP:

- 31 of 46 refused by `http.ReadRequest` or by the chunked reader.
- 15 forwarded: `cl-te`, `te-cl`, `te-uppercase-with-cl`, `te-http10`,
  `te-space-before-colon`, `cl-space-before-colon`, `te-folded-value`,
  `te-folded-into-other`, `cl-twice-same`, `bare-lf-request`, `bare-lf-te`,
  `lf-ends-block`, `host-space`, `host-userinfo`, `target-opaque`.
- In 11 of those the backend logged `GET /smuggled-...`. None of these is
  the backend desyncing, since the proxy always re-serialises. Each time,
  the proxy itself read the leftover bytes as a second, pipelined request
  and forwarded it on its own. That is the back-end half of a CL.TE pair:
  a front end that reads such a request as one would have had a second
  request slipped past it.

With Phase 8, all 46 are refused and the backend sees none of them.

## What is out of scope

- HTTP/2 downgrade smuggling (H2.CL, H2.TE): the proxy speaks HTTP/1.1
  only.
- Response splitting from a backend: backends are trusted
  (`docs/THREAT_MODEL.md`). Their responses are still re-framed, and
  `FuzzResponse` checks that whatever a backend sends, the client gets
  exactly one well-framed response.
- `Expect: 100-continue` and pipelining are handled by the request loop
  (Phase 1) and do not change framing.
