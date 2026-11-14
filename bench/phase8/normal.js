// k6 script for bench/phase8/slowhttptest.sh: GET https://a.test:8443/ at
// a constant arrival rate (RATE per second for DURATION), the normal
// traffic whose latency must hold up while slow clients attack. Anything
// but a 200 counts as a failure.
import http from 'k6/http';
import { Counter } from 'k6/metrics';

const other = new Counter('status_other');

export const options = {
  hosts: { 'a.test': '10.0.0.10' },
  insecureSkipTLSVerify: true,
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'p(50)', 'p(99)', 'max'],
  scenarios: {
    paced: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE),
      timeUnit: '1s',
      duration: __ENV.DURATION,
      preAllocatedVUs: 20,
      maxVUs: 200,
    },
  },
};

export default function () {
  const r = http.get('https://a.test:8443/', { timeout: '10s' });
  if (r.status !== 200) other.add(1);
}
