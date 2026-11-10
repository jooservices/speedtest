// Use V8 counters for VM-loaded browser scripts, then the standard Istanbul mapper.
import { spawnSync } from 'node:child_process';
import { readFileSync, readdirSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import v8ToIstanbul from 'v8-to-istanbul';
import istanbul from 'istanbul-lib-coverage';
import { functionsOf } from '../lib/v8cov.mjs';

mkdirSync('.tmp', { recursive: true });
const directory = mkdtempSync(resolve('.tmp/ui-v8-'));
const targets = ['ui/assets/js/api.js', 'ui/assets/js/store.js', 'ui/assets/js/seed.js'].map(file => resolve(file));
const functionMaps = new Map(targets.map(file => [file, new Map()]));
try {
  const tests = readdirSync('tests/ui').filter(file => file.endsWith('.test.mjs')).map(file => 'tests/ui/' + file);
  const run = spawnSync(process.execPath, ['--test', ...tests], { stdio: 'inherit', env: { ...process.env, NODE_V8_COVERAGE: directory } });
  if (run.status !== 0) process.exitCode = run.status || 1;
  const map = istanbul.createCoverageMap({});
  for (const name of readdirSync(directory)) {
    const scripts = JSON.parse(readFileSync(directory + '/' + name)).result;
    for (const script of scripts) {
      const path = script.url.startsWith('file:') ? fileURLToPath(script.url) : script.url;
      const target = targets.find(file => file === path);
      if (!target) continue;
      for (const fn of functionsOf(readFileSync(target, 'utf8'), script.functions)) {
        const previous = functionMaps.get(target).get(fn.id);
        functionMaps.get(target).set(fn.id, { ...fn, covered: fn.covered || Boolean(previous && previous.covered) });
      }
      const converter = v8ToIstanbul(path);
      await converter.load(); converter.applyCoverage(script.functions); map.merge(converter.toIstanbul());
    }
  }
  for (const file of targets) {
    const coverage = map.fileCoverageFor(file), summary = coverage.toSummary();
    const source = readFileSync(file, 'utf8');
    if (file.endsWith('seed.js')) { console.log(`seed.js: ${summary.lines.pct}% lines (demo data; the per-file gate is scripts/ui-coverage.mjs)`); continue; }
    const first = file.endsWith('store.js') ? source.slice(0, source.indexOf('  function initRemote()')).split('\n').length : 1;
    const last = file.endsWith('store.js') ? source.slice(0, source.indexOf('  if (ST.demo) load();')).split('\n').length - 1 : source.split('\n').length;
    const lines = Object.entries(coverage.getLineCoverage()).filter(([line]) => Number(line) >= first && Number(line) <= last);
    const hit = lines.filter(([, count]) => count > 0).length;
    const functions = [...functionMaps.get(file).values()], called = functions.filter(fn => fn.covered).length;
    console.log(`${file.split('/').at(-1)}: ${summary.lines.pct}% lines; new feature region ${hit}/${lines.length} lines (${(hit / lines.length * 100).toFixed(2)}%); ${called}/${functions.length} functions`);
    if (summary.lines.pct < 90 || hit !== lines.length) process.exitCode = 1;
  }
  writeFileSync('.tmp/ui-coverage.json', JSON.stringify(map.toJSON()));
  // The same line data in the shape scripts/ui-coverage.mjs merges with the browser runs.
  mkdirSync('.tmp/coverage', { recursive: true });
  writeFileSync('.tmp/coverage/unit.json', JSON.stringify(Object.fromEntries(targets.map(file => {
    const entries = Object.entries(map.fileCoverageFor(file).getLineCoverage()).map(([line, count]) => [Number(line), count]);
    return [file.slice(file.indexOf('ui/assets')), {
      lines: entries.map(e => e[0]), covered: entries.filter(e => e[1] > 0).map(e => e[0]), functions: [...functionMaps.get(file).values()]
    }];
  }))));
} finally { rmSync(directory, { recursive: true, force: true }); }
