// Per-file line coverage plus function coverage for reviewed UI files, merged across browser and Node runs.
// Usage: node scripts/ui-coverage.mjs [--min 85]. Run `make e2e`, `npm run test:ui` and `npm run test:coverage` first.
import { merge } from '../tests/lib/v8cov.mjs';

const min = Number(process.argv[process.argv.indexOf('--min') + 1]) || 85;
const functionTargets = new Set([
  'ui/assets/js/pages/dashboard.js', 'ui/assets/js/pages/stats.js', 'ui/assets/js/ui.js'
]);
const all = merge(['.tmp/coverage/e2e.json', '.tmp/coverage/smoke.json', '.tmp/coverage/wizard.json', '.tmp/coverage/unit.json']);
const rows = Object.entries(all).map(([file, v]) => ({ file, total: v.lines.size, covered: v.covered.size, pct: v.lines.size ? v.covered.size / v.lines.size * 100 : 100 })).sort((a, b) => a.pct - b.pct);
const detail = process.argv.includes('--detail') ? process.argv[process.argv.indexOf('--detail') + 1] : null;
if (detail) {
  const v = all[detail];
  const src = (await import('node:fs')).readFileSync(detail, 'utf8').split('\n');
  for (const l of [...v.lines].sort((a, b) => a - b)) if (!v.covered.has(l)) console.log(String(l).padStart(4), src[l - 1].slice(0, 150));
  process.exit(0);
}
let failed = 0, functionFailures = 0;
for (const r of rows) {
  const flag = r.pct < min ? ' <-- below ' + min : '';
  if (r.pct < min) failed++;
  const functions = [...all[r.file].functions.values()], called = functions.filter(fn => fn.covered).length;
  const enforceFunctions = functionTargets.has(r.file), fnFailed = enforceFunctions && (functions.length === 0 || called !== functions.length);
  if (fnFailed) functionFailures++;
  const fnSummary = enforceFunctions ? `; functions ${called}/${functions.length}${fnFailed ? ' <-- every function must be covered' : ''}` : '';
  console.log(`${r.pct.toFixed(1).padStart(6)}%  ${String(r.covered).padStart(4)}/${String(r.total).padEnd(4)} ${r.file}${fnSummary}${flag}`);
  if (fnFailed) for (const fn of functions.filter(item => !item.covered)) console.log(`         uncovered ${fn.name} at line ${fn.line}`);
}
const t = rows.reduce((a, r) => [a[0] + r.covered, a[1] + r.total], [0, 0]);
console.log(`total ${(t[0] / t[1] * 100).toFixed(1)}% of ${t[1]} lines in ${rows.length} files; ${failed} below ${min}%; ${functionFailures} reviewed files with uncovered functions`);
process.exit(failed || functionFailures ? 1 : 0);
