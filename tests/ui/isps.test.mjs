import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('../../ui/assets/js/pages/isps.js', import.meta.url), 'utf8');

function pendingISPPage() {
  let resolve, reject, signal;
  const pending = new Promise((yes, no) => { resolve = yes; reject = no; });
  const root = { children: [], appendChild(child) { this.children.push(child); } };
  const ui = {
    h: (tag, attrs, ...children) => ({ tag, attrs, children }),
    pageHead: title => title,
    empty: title => title,
    btn: title => title,
    clear: target => { target.children = []; },
  };
  const ST = {
    demo: false, pages: {}, ui, fmt: {}, settings: () => ({ range: '6h' }), engineQuery: () => '',
    api: { request(_path, options) { signal = options?.signal; return pending; } },
  };
  vm.runInNewContext(source, { window: { ST }, AbortController });
  const cleanup = ST.pages.isps(root);
  return { root, cleanup, resolve, reject, signal: () => signal };
}

for (const outcome of ['success', 'failure']) {
  test(`late ISP list ${outcome} cannot replace another page`, async () => {
    const page = pendingISPPage();
    page.root.children = ['Nodes page'];
    page.cleanup?.();
    if (outcome === 'success') page.resolve({ data: [] });
    else page.reject(new Error('late ISP error'));
    await Promise.resolve();
    await Promise.resolve();
    assert.deepEqual(page.root.children, ['Nodes page']);
    assert.equal(page.signal()?.aborted, true);
  });
}
