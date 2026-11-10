import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { functionsOf, merge, summarise } from '../lib/v8cov.mjs';

const source = 'function hit() { return true; }\nconst miss = () => false;';
const start = token => source.indexOf(token);
const ranges = (token, count) => [{ startOffset: start(token), endOffset: source.indexOf('}', start(token)) + 1, count }];

test('function coverage excludes the script wrapper and counts actual calls', () => {
  const functions = functionsOf(source, [
    { functionName: '', ranges: [{ startOffset: 0, endOffset: source.length, count: 1 }] },
    { functionName: 'hit', ranges: ranges('function hit', 1) },
    { functionName: 'miss', ranges: [{ startOffset: start('() =>'), endOffset: source.length, count: 0 }] },
  ]);
  assert.deepEqual(functions.map(fn => [fn.name, fn.covered]), [['hit', true], ['miss', false]]);
});

test('browser coverage unions repeated functions across document loads', () => {
  const entry = functions => ({ url: 'http://127.0.0.1/assets/js/sample.js', source, functions });
  const result = summarise([
    entry([{ functionName: 'hit', ranges: ranges('function hit', 1) }]),
    entry([{ functionName: 'miss', ranges: [{ startOffset: start('() =>'), endOffset: source.length, count: 1 }] }]),
  ]);
  assert.deepEqual(result['ui/assets/js/sample.js'].functions.map(fn => [fn.name, fn.covered]), [['hit', true], ['miss', true]]);
});

test('coverage gate unions function counters from separate runs', () => {
  mkdirSync('.tmp', { recursive: true });
  const directory = mkdtempSync(join('.tmp', 'speedtest-coverage-'));
  try {
    const first = join(directory, 'first.json'), second = join(directory, 'second.json');
    writeFileSync(first, JSON.stringify({ 'ui/assets/js/sample.js': { lines: [1], covered: [1], functions: [{ id: '0:1', name: 'hit', line: 1, covered: false }] } }));
    writeFileSync(second, JSON.stringify({ 'ui/assets/js/sample.js': { lines: [2], covered: [2], functions: [{ id: '0:1', name: 'hit', line: 1, covered: true }] } }));
    const result = merge([first, second])['ui/assets/js/sample.js'];
    assert.deepEqual([...result.lines], [1, 2]);
    assert.equal(result.functions.get('0:1').covered, true);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
