# Threat model

What the proxy defends against, how, and where each defence is tested.
The proxy is an HTTP/1.1 reverse proxy that terminates TLS for the
Internet and forwards plain HTTP to backends on a private network.

## Assets

- **Availability**: serving well-behaved clients at their usual latency,
  whatever other clients do.
- **Backend integrity**: a backend sees exactly the requests clients sent,
  one at a time, with framing it cannot misread, and forwarding headers it
  can trust.
- **TLS keys and session tickets**: the certificates' private keys, read
  from disk at startup and on reload, and the ticket keys crypto/tls keeps
  in memory.
- **The process**: a fault caused by one client must not take down the
  others.

## Trust boundaries

| Party | Trusted? | Why |
| --- | --- | --- |
| Clients | **No** | Anyone on the Internet. Every byte they send is hostile until parsed and checked. |
| Trusted proxies (`trusted_proxies`) | For `X-Forwarded-For` only | A CDN or load balancer in front; its `X-Forwarded-For` names the client. It is still a client for everything else. |
| Backends | Yes | Run by the same operator on a private network. Their responses are still re-framed rather than copied, which `FuzzResponse` tests, but a malicious backend is out of scope. |
| The config file and certificate files | Yes | Written by the operator. A typo must give an error and never a panic (`FuzzConfig`), but a hostile config is out of scope. |
| The admin listener | Loopback only | It refuses to bind anything but a loopback address; on the host, whoever can reach loopback can read metrics and profiles. |
| Signals | The process owner | `SIGHUP` reloads and `SIGTERM` stops; only the owner and root can send them. |

## Attacks and mitigations

### Request smuggling

*Attack*: a request whose framing (Content-Length, Transfer-Encoding, line
endings, folding) one parser reads differently from another, so a second,
hidden request gets past whichever parser enforced something.

*Mitigations*: ambiguous framing is refused with 400 and a closed
connection before anything is forwarded (`httpx.CheckRaw`). Requests are
re-serialised by `Request.Write`, never copied as bytes, so backends see
one framing. Framing fields are stripped from trailers. Every error closes
the connection.

*Tests*: `TestNetHTTPReadRequest` pins what `net/http` does on its own;
`TestCheckRaw`; `TestSmugglingCorpus` sends 46 payloads over HTTP and
HTTPS, each refused and none reaching the backend as a second request,
where the Phase 7 proxy forwarded 15 (`docs/SMUGGLING.md`);
`FuzzRequestPath` checks that whatever comes in, what goes out parses as
exactly one request.

### Slow clients (slowloris, slow body, slow read)

*Attack*: hold many connections open while sending, or reading, as little
as possible, until the server runs out of connections, goroutines or
memory.

*Mitigations*:
- Slow headers: one deadline (`header_read_ms`, 5 s) covers the whole
  header block from its first byte, so sending a byte every few seconds
  does not extend it. The first request on a new connection gets the same
  deadline, and the TLS handshake its own (`handshake_ms`).
- Slow body: a minimum rate (`min_body_rate`, 500 B/s after
  `body_grace_ms`, 10 s), enforced as a read deadline that every byte
  moves 1/rate seconds later. A trickle is cut off however often it sends.
- Slow read: the stall timeout (`client_io_ms`) closes a client that
  stops reading. One that reads slowly holds its connection, its
  goroutine, its fixed buffers and an upstream connection, which
  `max_conns` and `max_conns_per_ip` bound. Each costs about 165 KB of
  heap while its write is blocked (measured: 835 MiB for 5,021), so size
  `max_conns` to memory: the default 10,000 allows about 1.6 GiB. There
  is no minimum rate for responses; one would cap how long a slow reader
  holds its share, at the risk of cutting off slow mobile clients.
- Idle keep-alive connections are closed after `idle_ms`.

*Tests*: `bench/phase8/slowhttptest.sh` runs all three at 5,000
connections while measuring normal requests' latency and the proxy's
goroutines and memory; `TestBodyMinRate`, `TestProxyBodyTooSlow`; Phase 1's
header-deadline tests.

### Resource exhaustion by volume of requests or size

*Attack*: very large headers, very many headers, very long request lines,
huge bodies, or endless requests on one connection.

*Mitigations*: `max_header_bytes` (431, enforced beneath the
`bufio.Reader`, so a long line is never buffered whole),
`max_header_count` (431), `max_request_line` (414), `max_body_bytes`
(413; off by default, because bodies stream through fixed buffers and cost
the proxy nothing whatever their size; set it for backends that read
bodies whole), `max_requests_per_conn` (1,000, then `Connection: close`),
`max_conns` (10,000) and `max_conns_per_ip` at accept, before the TLS
handshake. Per-client rate limits (Phase 6) answer 429. Bodies and
responses are streamed, never buffered whole, and a request with a body is
never retried after the body has started, so it is never held for a
retry.

*Tests*: `TestProxyRejects`, `TestProxyRefusesAmbiguousFraming`,
`TestProxyBodyTooLarge`, `TestProxyMaxRequestsPerConn`,
`TestUncappedUploadDoesNotGrowMemory` (1 GiB through, heap +3 MiB),
Phase 1's 1 GB slow-reader test, Phase 6's rate-limit tests.

### Spoofed client identity

*Attack*: a client sends its own `X-Forwarded-For` or `Forwarded` to pose
as another address: to a backend that logs or authorises by it, or to the
proxy's own rate limiter and per-IP cap.

*Mitigations*: from a peer that is not in `trusted_proxies`, the proxy
replaces `X-Forwarded-For` with the peer's address and drops `Forwarded`;
only a trusted proxy's chain is appended to. The rate limiter takes the
rightmost address that is not a trusted proxy. A client naming
`X-Forwarded-For` in `Connection` only strips its own copy; the proxy sets
its own afterwards. `X-Forwarded-Proto` is always overwritten.

*Tests*: `TestProxyForwardedFor`, `TestSetForwarded`, Phase 6's
`TestRateLimitIgnoresUntrustedForwardedFor`.

### Hop-by-hop header abuse

*Attack*: headers meant for one connection (`Connection`, `Upgrade`,
`Proxy-Authorization`, `TE`, anything a `Connection` field names) passed to
the backend, to upgrade the backend connection or carry credentials meant
for the proxy.

*Mitigations*: all are removed in both directions; `CONNECT` is refused
(405); a backend's 101 is refused, since `Upgrade` never reaches it.

*Tests*: `TestPrepareRequestWire`, `TestProxyRequestHeaders`,
`TestProxyResponseHopByHop`, `FuzzResponse`.

### Routing and certificate confusion

*Attack*: a request whose `Host` names a different site from the TLS SNI,
to reach a backend under another site's certificate; an invalid `Host` or
an opaque request target that a backend would parse differently.

*Mitigations*: SNI and `Host` must agree (421 otherwise); `Host` must be
made of host bytes; the target must be origin-form, http(s) absolute-form
or `OPTIONS *`; duplicate `Host` is refused.

*Tests*: Phase 3's `TestTLSMisdirected`, `TestReadRequest`, the corpus's
`host-*` and `target-opaque` cases.

### Panics

*Attack*: an input that reaches a bug and panics. In Go an unrecovered
panic in any goroutine ends the process, and with it every other client's
connection.

*Mitigations*: a `recover` at the top of each connection goroutine and of
the request-body writer goroutine logs the panic with its stack, counts it
in `proxy_panics_total`, and ends only that connection. Deferred cleanup
(buffers, the upstream connection, the writer) runs during the unwind.

*Tests*: `TestServerRecoversPanic`, `TestProxyRecoversPanic`; the three
fuzz targets look for panics directly.

### File descriptor exhaustion

*Attack*: open connections until `accept` fails with EMFILE, after which a
naive accept loop spins at full CPU.

*Mitigations*: `max_conns` keeps the proxy under its descriptor limit;
if `Accept` fails anyway, the loop backs off from 5 ms to 1 s, logs at
most once a second, and counts each failure.

*Tests*: `TestAcceptBackoff`.

### TLS downgrade and weak ciphers

*Mitigations*: TLS 1.2 and 1.3 only, forward-secret AEAD suites, no
renegotiation, no compression (Phase 3). Session tickets come from one
`tls.Config` and survive reloads.

*Tests*: `testssl.sh` (`docs/testssl-phase8.txt`).

### Privilege

*Attack*: a compromise of the proxy process (through a bug Go's memory
safety does not rule out, or a dependency) gains whatever the process
holds.

*Mitigations*: the proxy needs no privileges. The image runs it as
uid 10001; the lab drops every capability, sets `no-new-privileges` and
mounts the root filesystem read-only, all checked by `check-lab.sh`. On a
host, `CAP_NET_BIND_SERVICE` alone is enough to bind 443. Started as root,
it warns. Third-party code is three modules, scanned by `govulncheck` in
CI.

## What Go's memory safety does and does not cover

It rules out buffer overflows, use-after-free and out-of-bounds writes in
the proxy's own code: the bugs that make a C parser exploitable. It does
not rule out:

- **logic bugs**: smuggling is entirely a logic bug, and so was the opaque
  target `FuzzRequestPath` found;
- **data races**, which the race detector finds only on the paths tests
  run (every test and benchmark script runs with `-race`);
- **panics**, which end the process unless recovered (above);
- **resource exhaustion**: goroutines, memory and file descriptors are as
  finite in Go as anywhere (above).

## Out of scope

- **Volumetric DDoS**: floods that saturate the network link or the
  host's packet processing are for the network or an upstream scrubbing
  service. The proxy bounds what it holds per connection, but cannot make
  more bandwidth.
- **Malicious backends and config**: both are trusted (above).
- **Attacks on the host**: kernel, container runtime, other processes.
- **HTTP/2 and HTTP/3**: not implemented, so neither are their attacks
  (rapid reset, HPACK bombs, H2 downgrade smuggling).
- **Application-level attacks** that pass through as valid HTTP (SQL
  injection, XSS, credential stuffing): the backends' to handle; the
  proxy is not a WAF.
- **Side channels on TLS**: crypto/tls's constant-time implementations are
  relied on.
