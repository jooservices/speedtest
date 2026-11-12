// Browser JavaScript coverage: turns Playwright's raw V8 entries into covered line sets per UI source file and
// merges the results of several runs (E2E against the real center, demo/API smoke) by union.
import { mkdirSync, readFileSync, writeFileSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';

const UI = /\/assets\/js\/(?!vendor)[^?]+\.js$/;

// Covered characters: ranges are nested (outer first); the innermost range decides.
function coveredMask(text, functions) {
  const mask = new Uint8Array(text.length);
  for (const fn of functions) for (const r of fn.ranges) mask.fill(r.count > 0 ? 1 : 0, r.startOffset, Math.min(r.endOffset, text.length));
  return mask;
}

export function linesOf(text, mask) {
  const lines = [], covered = [];
  let offset = 0, n = 0;
  for (const raw of text.split('\n')) {
    n++;
    const start = offset; offset += raw.length + 1;
    const code = raw.trim();
    if (!code || code.startsWith('//') || code.startsWith('/*') || code.startsWith('*') || /^[\])};,]+$/.test(code)) continue;
    lines.push(n);
    for (let i = start; i < start + raw.length; i++) if (mask[i] && !/\s/.test(text[i])) { covered.push(n); break; }
  }
  return { lines, covered };
}

export function functionsOf(text, functions) {
  return functions.flatMap(fn => {
    const range = fn.ranges[0];
    if (!range || (fn.functionName === '' && range.startOffset === 0 && range.endOffset >= text.length)) return [];
    const line = text.slice(0, range.startOffset).split('\n').length;
    return [{ id: `${range.startOffset}:${range.endOffset}`, name: fn.functionName || '(anonymous)', line, covered: range.count > 0 }];
  });
}

// entries come from page.coverage.stopJSCoverage(); returns { file: { lines: [...], covered: [...] } }
export function summarise(entries) {
  const out = {};
  for (const e of entries) {
    const m = UI.exec(new URL(e.url).pathname);
    if (!m) continue;
    const file = m[0].replace(/^\//, 'ui/');
    const source = e.source ?? '', { lines, covered } = linesOf(source, coveredMask(source, e.functions));
    const functions = functionsOf(source, e.functions);
    // The page is reloaded many times, so one file appears once per load: union them.
    const o = out[file] ??= { lines: [], covered: [], functions: [] };
    o.lines = [...new Set([...o.lines, ...lines])]; o.covered = [...new Set([...o.covered, ...covered])];
    const merged = new Map(o.functions.map(fn => [fn.id, fn]));
    for (const fn of functions) {
      const previous = merged.get(fn.id);
      merged.set(fn.id, { ...fn, covered: fn.covered || Boolean(previous && previous.covered) });
    }
    o.functions = [...merged.values()];
  }
  return out;
}

export function save(path, summary) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(summary));
}

export function merge(paths) {
  const all = {};
  for (const p of paths) {
    if (!existsSync(p)) continue;
    for (const [file, v] of Object.entries(JSON.parse(readFileSync(p, 'utf8')))) {
      const a = all[file] ??= { lines: new Set(), covered: new Set(), functions: new Map() };
      v.lines.forEach(l => a.lines.add(l)); v.covered.forEach(l => a.covered.add(l));
      for (const fn of v.functions || []) {
        const previous = a.functions.get(fn.id);
        a.functions.set(fn.id, { ...fn, covered: fn.covered || Boolean(previous && previous.covered) });
      }
    }
  }
  return all;
}

// Collects coverage for a page across navigations: Chromium reports only the scripts of the current document, so
// what has run is taken before every goto/reload and at the end, and the pieces are united. finish(name) writes
// .tmp/coverage/<name>.json.
export async function trackCoverage(page) {
  const parts = {};
  await page.coverage.startJSCoverage({ resetOnNavigation: false });
  const take = async () => {
    const part = summarise(await page.coverage.stopJSCoverage());
    await page.coverage.startJSCoverage({ resetOnNavigation: false });
    for (const [file, v] of Object.entries(part)) {
      const a = parts[file] ??= { lines: new Set(), covered: new Set(), functions: new Map() };
      v.lines.forEach(l => a.lines.add(l)); v.covered.forEach(l => a.covered.add(l));
      for (const fn of v.functions) {
        const previous = a.functions.get(fn.id);
        a.functions.set(fn.id, { ...fn, covered: fn.covered || Boolean(previous && previous.covered) });
      }
    }
  };
  for (const method of ['goto', 'reload']) { const raw = page[method].bind(page); page[method] = async (...args) => { await take(); return raw(...args); }; }
  return { take, async finish(name) {
    await take();
    save(join('.tmp', 'coverage', `${name}.json`), Object.fromEntries(Object.entries(parts).map(([f, v]) => [f, {
      lines: [...v.lines], covered: [...v.covered], functions: [...v.functions.values()]
    }])));
  } };
}
