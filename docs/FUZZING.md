# Fuzzing

Three native Go fuzz targets, each run for over a CPU-hour by
`bench/phase8/fuzz.sh` (`make fuzz`). Results: `bench/phase8/results/fuzz.txt`.

| Target | Package | What it runs | Property beyond "no panic" |
| --- | --- | --- | --- |
| `FuzzRequestPath` | `internal/httpx` | client bytes through `ReadRequest` (net/http's parser plus `CheckRaw`), the `Body` wrapper, `PrepareRequest`, `Request.Write` | what goes upstream parses as exactly one request, nothing left over, same method, target, host and body; it passes `CheckRaw` itself; `X-Forwarded-For` is the proxy's alone; no forbidden trailer |
| `FuzzResponse` | `internal/httpx` | backend bytes through `ReadResponse` (1xx relayed, 101 refused), `Body`, `PrepareResponse`, `Response.Write`; the client is GET or HEAD, HTTP/1.1 or 1.0 | the client reads the interim responses and then exactly one response, with the backend's status and body bytes, closed or kept alive as `PrepareResponse` said, with no hop-by-hop field |
| `FuzzConfig` | `internal/config` | `Parse`, then `NewSnapshot` when no certificate path is set | an error names the file; a config that parses has listeners and valid timeouts and builds a snapshot whose routes all resolve |

Each target calls the functions the proxy calls. To make that true for
responses, the 1xx and 101 loop moved from `conn` into
`httpx.ReadResponse`.

## Runs

The final run, all three at once on 22 logical CPUs (Core Ultra 7 155H,
WSL2), 7 workers each for 12 minutes, at commit `dbed423`:

| Target | CPU-seconds | Executions | Per second | Corpus | Failures |
| --- | ---: | ---: | ---: | ---: | ---: |
| `FuzzRequestPath` | 4,517 | 34,988,465 | 48,528 | 589 | 0 |
| `FuzzResponse` | 4,513 | 22,570,428 | 31,348 | 344 | 0 |
| `FuzzConfig` | 4,468 | 14,200,791 | 19,723 | 1,009 | 0 |

CPU-seconds are user plus system time of the whole `go test -fuzz`
process tree, measured by `/usr/bin/time`. `FuzzConfig`'s rate dips to
zero for seconds at a time while the engine minimises a new input; every
corpus input runs in under 10 ms.

## Findings

All three were real bugs in the proxy, not in `net/http`; each is fixed,
has a unit test, and its input is kept under `testdata/fuzz/` so every
`go test` replays it.

1. **An opaque request target forwarded as something else**
   (`FuzzRequestPath`, within seconds of the first run; input
   `testdata/fuzz/FuzzRequestPath/890746b7c0ca2373`). `00 A:0 HTTP/1.0`
   parses with the target as an opaque URL (`a:0`), and `Request.Write`
   sends it as `0`: the backend would get a request line the proxy never
   saw. Fix: only origin-form, http(s) absolute-form with a host, and
   `OPTIONS *` are forwarded; anything else is 400 `bad_target`.

2. **A bodiless response with no length closed a keep-alive connection**
   (`FuzzResponse`'s seeds, before any fuzzing). For a response to HEAD,
   or a 204 or 304 without a length, `Response.Write` added
   `Connection: close`, treating it as close-delimited, while the proxy
   kept the connection open. A lost keep-alive, not a desync. The same
   check showed a 204 to a POST getting `Content-Length: 0`, which RFC
   9110 forbids. Fix in `PrepareResponse` (see DESIGN.md, "Three
   response-framing fixes").

3. **A chunked 204 left five bytes for the client to misread**
   (`FuzzResponse`, after 4.5 minutes of the first full run; input
   `testdata/fuzz/FuzzResponse/ddae26f75ef4b074`:
   `HTTP/1.1 204 \nTrAnsfer-EnCoding:Chunked\n\n`). `net/http` reads a 204
   with no body but keeps `Transfer-Encoding: chunked`, and
   `Response.Write` then writes an empty last chunk, `0\r\n\r\n`, after
   the header. The client, which knows a 204 has no body, reads those
   bytes as the start of the next response. Fix: a 1xx, 204 or 304 is
   relayed without `Transfer-Encoding` (a HEAD response keeps it; no body
   is written for HEAD). Backends are trusted, but this is the proxy
   emitting a framing its client cannot read.

`FuzzConfig` found nothing: the TOML decoder and the validator gave an
error naming the file for every input, and every config they accepted
built.

## Corpora

`testdata/fuzz/<Target>/` in each package holds the seeds' descendants
the engine kept because they reached new coverage, plus the failing
inputs: the minimised set, about 760 KB in all. `go test` runs every one
as a regression test in about a second (`-race` included). The seeds
themselves are in the test files: the smuggling cases from
`TestNetHTTPReadRequest`, ordinary and broken responses, `fullConfig`,
and `lab/proxy.toml`.

To fuzz further: `go test -run '^$' -fuzz=FuzzRequestPath
-fuzztime=10m ./internal/httpx`, then `bench/phase8/fuzz.sh` copies new
corpus entries from the Go cache.

## Why the fuzzer found what the unit tests did not

The unit tests and the 46-case smuggling corpus were written from known
attacks. The fuzzer starts from those same inputs, but keeps any mutation
that reaches a branch no earlier input reached, and builds on it. Two of
its three findings were in inputs nobody writes on purpose (a method of
`00`, a target of `A:0`, a 204 that is chunked). They were found because
the property checked the whole output, not just the absence of a panic:
a parser bug that produces valid-looking but different bytes is
invisible to a crash detector and obvious to "parse what you sent and
compare".
