// Per-file line coverage of the UI JavaScript, from the browser runs (E2E on the real center and the demo/API
// smoke run) plus the Node unit tests of api.js/store.js, merged by union. Usage: node scripts/ui-coverage.mjs [--min 85]. Run `make e2e`,
// `npm run test:ui` and `npm run test:coverage` first; the raw data is .tmp/coverage/*.json.
import { merge } from '../tests/lib/v8cov.mjs';

const min = Number(process.argv[process.argv.indexOf('--min') + 1]) || 85;
const all = merge(['.tmp/coverage/e2e.json', '.tmp/coverage/smoke.json', '.tmp/coverage/wizard.json', '.tmp/coverage/unit.json']);
const rows = Object.entries(all).map(([file, v]) => ({ file, total: v.lines.size, covered: v.covered.size, pct: v.lines.size ? v.covered.size / v.lines.size * 100 : 100 })).sort((a, b) => a.pct - b.pct);
const detail = process.argv.includes('--detail') ? process.argv[process.argv.indexOf('--detail') + 1] : null;
if (detail) {
  const v = all[detail];
  const src = (await import('node:fs')).readFileSync(detail, 'utf8').split('\n');
  for (const l of [...v.lines].sort((a, b) => a - b)) if (!v.covered.has(l)) console.log(String(l).padStart(4), src[l - 1].slice(0, 150));
  process.exit(0);
}
let failed = 0;
for (const r of rows) {
  const flag = r.pct < min ? ' <-- below ' + min : '';
  if (r.pct < min) failed++;
  console.log(`${r.pct.toFixed(1).padStart(6)}%  ${String(r.covered).padStart(4)}/${String(r.total).padEnd(4)} ${r.file}${flag}`);
}
const t = rows.reduce((a, r) => [a[0] + r.covered, a[1] + r.total], [0, 0]);
console.log(`total ${(t[0] / t[1] * 100).toFixed(1)}% of ${t[1]} lines in ${rows.length} files; ${failed} below ${min}%`);
process.exit(failed ? 1 : 0);
