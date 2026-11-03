// Validates every JSON API response the browser received from the real center against docs/openapi.yaml.
import { readFileSync } from 'node:fs';
import YAML from 'yaml';
import Ajv from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import { root } from './lib.mjs';

const contract = YAML.parse(readFileSync(`${root}/docs/openapi.yaml`, 'utf8'));
const ajv = new Ajv({ strict: false, allErrors: true });
addFormats(ajv);

function operation(path, method) {
  // A literal path (/api/schedules/rules) wins over a parametrised one (/api/schedules/{id}).
  const template = contract.paths[path] ? path : Object.keys(contract.paths).find(t => new RegExp('^' + t.replace(/\{[^}]+\}/g, '[^/]+') + '$').test(path));
  return template ? { template, op: contract.paths[template][method.toLowerCase()] } : {};
}

// Returns a recorder; call .check() at the end for the list of violations and the number of responses seen.
export function recordContract(page, origin) {
  const seen = [];
  page.on('response', async response => {
    const url = new URL(response.url());
    if (url.origin !== origin || !url.pathname.startsWith('/api/')) return;
    if (!(response.headers()['content-type'] || '').includes('json')) return;
    let body; try { body = await response.json(); } catch { return; }
    seen.push({ method: response.request().method(), path: url.pathname, status: response.status(), body });
  });
  return {
    check() {
      const violations = [], counted = new Set();
      for (const r of seen) {
        const { template, op } = operation(r.path, r.method);
        const label = `${r.method} ${r.path} -> ${r.status}`;
        if (!op) { violations.push(`${label}: not documented in openapi.yaml`); continue; }
        let def = op.responses[r.status];
        if (def?.$ref) def = contract.components.responses[def.$ref.split('/').pop()];
        const media = def?.content?.['application/json'] || def?.content?.['application/problem+json'];
        const schema = media?.schema;
        if (!schema) { violations.push(`${label}: status not documented for ${template}`); continue; }
        const validate = ajv.compile({ ...schema, components: contract.components });
        if (!validate(r.body)) violations.push(`${label}: ${ajv.errorsText(validate.errors, { dataVar: 'body' }).slice(0, 300)}`);
        counted.add(label);
      }
      return { violations: [...new Set(violations)], responses: seen.length };
    }
  };
}
