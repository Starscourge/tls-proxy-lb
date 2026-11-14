// k6 load for the Phase 9 failure demos (bench/phase9/demo-kill.sh and
// demo-slow.sh): GET at a constant arrival rate, one line per request.
//
// Env: URL (default https://10.0.0.10:8443/), RATE per second, DURATION
// (k6 duration, e.g. 60s).
//
// Each request prints, with --log-format=raw, one CSV line:
//
//   start_ms,backend,status,latency_ms,error
//
// start_ms is the wall-clock time the request started; latency_ms is what
// the client waited for the whole exchange: queueing for a connection,
// connecting, the TLS handshake, sending and the response (k6's
// http_req_duration alone leaves out the connection set-up). backend is the
// X-Backend header, empty when there was no response, and error is k6's
// error code, 0 when there was none.
//
// k6 cannot be given the lab CA, so certificate checks are off; the URL is
// an IP literal, so no SNI is sent and the proxy serves its default pool.
import http from "k6/http";

const rate = parseInt(__ENV.RATE || "1000", 10);

export const options = {
  discardResponseBodies: true,
  insecureSkipTLSVerify: true,
  // The default 60 s request timeout is longer than anything the demos
  // provoke, so every request ends with the proxy's answer.
  scenarios: {
    demo: {
      executor: "constant-arrival-rate",
      rate: rate,
      timeUnit: "1s",
      duration: __ENV.DURATION || "60s",
      // Enough VUs that a backend hanging requests for the proxy's
      // upstream_response_ms never makes k6 skip an arrival: k6 counts any
      // it could not start in dropped_iterations, and the scripts fail the
      // run if that is not zero.
      preAllocatedVUs: Math.max(50, rate / 5),
      maxVUs: rate * 12,
    },
  },
  summaryTrendStats: ["min", "med", "p(99)", "max"],
};

const url = __ENV.URL || "https://10.0.0.10:8443/";

export default function () {
  const start = Date.now();
  const res = http.get(url);
  const t = res.timings;
  const latency = t.blocked + t.connecting + t.tls_handshaking + t.duration;
  const backend = (res.headers && res.headers["X-Backend"]) || "";
  console.log(`${start},${backend},${res.status},${latency.toFixed(3)},${res.error_code || 0}`);
}
