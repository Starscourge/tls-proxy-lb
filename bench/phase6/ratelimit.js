// k6 script for bench/phase6/k6.sh: GET https://limited.test:8443/ at a
// constant arrival rate (RATE per second for DURATION), counting 200s,
// 429s and anything else. The open loop matters: a closed loop would slow
// down as soon as requests are refused and never push past the limit.
import http from 'k6/http';
import { Counter } from 'k6/metrics';

const ok = new Counter('status_200');
const limited = new Counter('status_429');
const other = new Counter('status_other');

export const options = {
  hosts: { 'limited.test': '10.0.0.10' },
  // The route is served the lab's default certificate, and k6 cannot be
  // given the lab CA.
  insecureSkipTLSVerify: true,
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'p(50)', 'p(99)', 'max'],
  scenarios: {
    paced: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE),
      timeUnit: '1s',
      duration: __ENV.DURATION,
      preAllocatedVUs: 50,
      maxVUs: 400,
    },
  },
};

export default function () {
  const r = http.get('https://limited.test:8443/');
  if (r.status === 200) ok.add(1);
  else if (r.status === 429) limited.add(1);
  else other.add(1);
}
