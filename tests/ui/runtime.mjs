import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { webcrypto } from 'node:crypto';

export function runtime(origin, options = {}) {
  const storage = new Map();
  const url = new URL(origin + (options.demo ? '/?demo=1' : '/'));
  const context = vm.createContext({ console, URL, URLSearchParams, Headers, AbortController, structuredClone,
    crypto: webcrypto, location: url, setTimeout, clearTimeout, document: { body: true },
    localStorage: { getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) },
    fetch: (path, init) => (options.fetch || fetch)(new URL(path, origin), init) });
  context.window = context;
  for (const file of ['seed', 'api', ...(options.apiOnly ? [] : ['store'])]) {
    vm.runInContext(readFileSync(`ui/assets/js/${file}.js`, 'utf8'), context, { filename: resolve(`ui/assets/js/${file}.js`) }); // NOSONAR: loads the repository's own UI scripts into a test sandbox
  }
  return { ST: context.ST, context, storage };
}
