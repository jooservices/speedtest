// Dashboard load test: a real center on a seeded worktree database, 50 nodes with
// a month of results, N concurrent signed-in users polling the pages' API calls.
// Run with `make perf`. Gate: p95 < 300 ms and center CPU < 50% of one core.
import { readFileSync } from 'node:fs';
import { startStack, sql, query, stopAll } from '../e2e/lib.mjs';

const USERS = Number(process.env.PERF_USERS || 10), SECONDS = Number(process.env.PERF_SECONDS || 30);
const stack = await startStack({ seed: false });
const { origin, db } = stack;
sql(db, `INSERT INTO ip_addresses(ip) VALUES ('203.0.113.7') ON CONFLICT DO NOTHING;
 INSERT INTO nodes(id,name,expected_down_mbps,expected_up_mbps,last_seen_at) SELECT gen_random_uuid(),'perf'||g,300,100,now() FROM generate_series(1,50) g;
 INSERT INTO results(id,node_id,ts,status,down_mbps,up_mbps,ping_ms,server_id,server,ip,threads)
 SELECT gen_random_uuid(), n.id, now()-(g*30||' minutes')::interval,'ok',250+random()*50,90+random()*10,8+random()*4,1,'{"id":1,"name":"perf"}','203.0.113.7',4
 FROM nodes n, generate_series(0,1400) g;`);
console.log('seeded results:', query(db, 'SELECT count(*) FROM results'));

async function session() {
  let cookie = '';
  const jars = new Map();
  const jar = res => { for (const c of res.headers.getSetCookie?.() ?? []) { const [k, v] = c.split(';')[0].split('='); jars.set(k, v); } cookie = [...jars].map(([k, v]) => `${k}=${v}`).join('; '); };
  const me = await fetch(origin + '/api/auth/me', { headers: { cookie } }); jar(me);
  const csrf = (await me.json()).data.csrfToken;
  const login = await fetch(origin + '/api/auth/login', { method: 'POST', headers: { cookie, 'content-type': 'application/json', 'x-csrf-token': csrf, origin },
    body: JSON.stringify({ email: 'jooservices@gmail.com', password: 'password' }) });
  if (!login.ok) throw new Error('login failed ' + login.status);
  jar(login);
  return cookie;
}
const paths = ['/api/dashboard', '/api/nodes?page=1&size=100', '/api/results?page=1&size=100', '/api/stats/summary', '/api/alerts/events?page=1&size=50', '/api/schedules', '/api/settings'];
const lat = [], errors = {}, byPath = {};
const cpu = pid => { const f = readFileSync(`/proc/${pid}/stat`, 'utf8').split(') ')[1].split(' '); return (Number(f[11]) + Number(f[12])) / 100; };
const cpu0 = cpu(stack.pid), wall0 = Date.now();
// Sign-in is rate limited per address (by design), so every simulated user shares one session.
const shared = await session();
const cookies = Array.from({ length: USERS }, () => shared);
const end = Date.now() + SECONDS * 1000;
await Promise.all(cookies.map(async cookie => {
  while (Date.now() < end) {
    for (const p of paths) {
      const t = performance.now();
      const res = await fetch(origin + p, { headers: { cookie } });
      await res.arrayBuffer();
      const d = performance.now() - t; lat.push(d); (byPath[p] ||= []).push(d);
      if (res.status >= 400) errors[`${p} ${res.status}`] = (errors[`${p} ${res.status}`] || 0) + 1;
    }
    await new Promise(r => setTimeout(r, 1000));
  }
}));
const cpuPct = (cpu(stack.pid) - cpu0) / ((Date.now() - wall0) / 1000) * 100;
lat.sort((a, b) => a - b);
const p95 = lat[Math.floor(lat.length * 0.95)];
const per = Object.fromEntries(Object.entries(byPath).map(([k, v]) => [k, +v.sort((a, b) => a - b)[Math.floor(v.length * 0.95)].toFixed(1)]));
console.log(JSON.stringify({ perPathP95: per }));
console.log(JSON.stringify({ users: USERS, requests: lat.length, p50: +lat[Math.floor(lat.length / 2)].toFixed(1), p95: +p95.toFixed(1), max: +lat.at(-1).toFixed(1), centerCpuPercent: +cpuPct.toFixed(1), errors }));
stopAll();
process.exit(p95 < 300 && cpuPct < 50 && !Object.keys(errors).length ? 0 : 1);
