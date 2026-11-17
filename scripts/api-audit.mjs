#!/usr/bin/env node
// Audits docs/openapi.yaml against the REST rules in docs/api-standard.md.
//
// Every finding must have exactly one row in the "Deviations and decisions" table of
// docs/api-standard.md, with the decision fix, alias or exception and a reason. An exception
// also needs an x-rest-exception on each operation it covers, starting with the finding key.
//
// Usage:
//   node scripts/api-audit.mjs                 print the report, exit 0
//   node scripts/api-audit.mjs --check         exit 1 when a finding is open or a row is stale
//   node scripts/api-audit.mjs --list          print the findings only (no decision table needed)
//   node scripts/api-audit.mjs --spec F --standard F   audit other files (used by the tests)
//
// Exit codes: 0 ok, 1 open problems (--check), 2 cannot read or parse an input.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

export const SECTION = 'Deviations and decisions';
export const DECISIONS = new Set(['fix', 'alias', 'exception']);
// Status codes the standard's table allows; any other code on an operation is a finding.
export const STANDARD_CODES = new Set([
  '200', '201', '202', '204', '304', '400', '401', '403', '404', '405',
  '409', '412', '413', '422', '428', '429', '500',
]);
// Path segments that name an action. As the last segment the operation must be a POST; before
// an {id} the segment is an action, not a collection, so the plural rule does not apply to it.
export const ACTION_SEGMENTS = new Set([
  'ack', 'run', 'enroll', 'login', 'logout', 'stagger', 'heartbeat', 'resets', 'previews', 'test',
]);
// Every method OpenAPI defines for a path item.
const HTTP = ['get', 'post', 'put', 'patch', 'delete', 'head', 'options', 'trace'];
const PROBLEM = 'application/problem+json';
const MERGE_PATCH = 'application/merge-patch+json';

const isParam = (segment) => /^\{[^}]+\}$/.test(segment ?? '');
// An action segment is one at the action position: the last segment, or one before an {id}.
// The plural exemption and the POST rule both use this predicate, so they cannot disagree.
const isActionSegment = (segments, i) =>
  ACTION_SEGMENTS.has(segments[i]) && (i === segments.length - 1 || isParam(segments[i + 1]));
const hasActionSegment = (segments) => segments.some((_, i) => isActionSegment(segments, i));
const inScope = (p) => p === '/api' || p.startsWith('/api/');

function deref(doc, node) {
  let cur = node;
  for (let i = 0; cur && typeof cur === 'object' && typeof cur.$ref === 'string' && i < 20; i++) {
    if (!cur.$ref.startsWith('#/')) return cur;
    cur = cur.$ref
      .slice(2)
      .split('/')
      .reduce((acc, key) => acc?.[key.replace(/~1/g, '/').replace(/~0/g, '~')], doc);
  }
  return cur;
}

function pathEntries(doc) {
  return Object.entries(doc.paths ?? {});
}

function methodsOf(item) {
  return HTTP.filter((m) => item[m]);
}

function operationAt(doc, opId) {
  const space = opId.indexOf(' ');
  const method = opId.slice(0, space).toLowerCase();
  const p = opId.slice(space + 1);
  return doc.paths?.[p]?.[method];
}

// Each finding: { key, rule, ops: ["METHOD /path", ...], summary }.
// A path-level key covers every operation of the path; an operation-level key covers one.
export function computeFindings(doc) {
  const findings = [];
  const add = (key, rule, ops, summary) => findings.push({ key, rule, ops, summary });

  for (const [p, item] of pathEntries(doc)) {
    if (!inScope(p)) continue;
    const methods = methodsOf(item);
    const allOps = methods.map((m) => `${m.toUpperCase()} ${p}`);
    const segments = p.split('/').filter(Boolean);
    const last = segments.at(-1);

    if (segments.some((s) => !isParam(s) && !/^[a-z0-9-]+$/.test(s))) {
      add(`path-charset ${p}`, 'path-charset', allOps,
        `${p} has a segment that is not lowercase letters, digits and hyphens`);
    }
    if (p.includes('//') || (p.length > 1 && p.endsWith('/'))) {
      add(`path-shape ${p}`, 'path-shape', allOps, `${p} has an empty segment or a trailing slash`);
    }
    const pluralBefore = segments.some((s, i) =>
      !isParam(s) && isParam(segments[i + 1]) && !s.endsWith('s') && !isActionSegment(segments, i));
    const pluralLast = methods.includes('get') && !isParam(last) && !last.endsWith('s');
    if (pluralBefore || pluralLast) {
      add(`plural ${p}`, 'plural', allOps,
        `${p} names a resource that is not in the plural (or a singleton)`);
    }

    // Path-level parameters are objects like operation-level ones; both are resolved the same way.
    const pathParams = (item.parameters ?? []).map((x) => deref(doc, x)).filter(Boolean);
    for (const m of methods) {
      const op = item[m];
      const id = `${m.toUpperCase()} ${p}`;
      const responses = op.responses ?? {};
      // A parameter counts only where it is declared: pagination is a query parameter, If-Match a header.
      const allParams = [...pathParams, ...(op.parameters ?? [])].map((x) => deref(doc, x)).filter(Boolean);
      const queryNames = new Set(allParams.filter((x) => x.in === 'query').map((x) => x.name));
      const headerNames = new Set(allParams.filter((x) => x.in === 'header').map((x) => String(x.name).toLowerCase()));

      if (m === 'get' && !isParam(last) && last.endsWith('s') && !(queryNames.has('page') && queryNames.has('size'))) {
        add(`list-page ${id}`, 'list-page', [id], `${id} returns a collection without page and size`);
      }
      if (m === 'get' && op.requestBody) {
        add(`get-body ${id}`, 'get-body', [id], `${id} declares a request body`);
      }
      if (m === 'patch') {
        const content = deref(doc, op.requestBody)?.content ?? {};
        if (!(MERGE_PATCH in content)) {
          add(`patch-media ${id}`, 'patch-media', [id], `${id} does not accept ${MERGE_PATCH}`);
        }
      }
      if ((m === 'put' || m === 'patch') && segments.some(isParam)) {
        // The standard: If-Match is sent, a missing one answers 428, a stale one answers 412.
        const missing = [];
        if (!headerNames.has('if-match')) missing.push('an If-Match header parameter');
        if (!('412' in responses)) missing.push('a 412 response');
        if (!('428' in responses)) missing.push('a 428 response');
        if (missing.length) {
          add(`if-match ${id}`, 'if-match', [id], `${id} updates an item without ${missing.join(', ')}`);
        }
      }
      if (hasActionSegment(segments) && m !== 'post') {
        add(`action-method ${id}`, 'action-method', [id], `${id} is an action, which must be POST`);
      }
      if (!op.deprecated || op['x-canonical-path'] !== `/api/v1${p.slice('/api'.length)}`) {
        add(`version ${id}`, 'version', [id],
          `${id} is not deprecated with x-canonical-path /api/v1 plus the same suffix`);
      }

      for (const code of Object.keys(responses)) {
        // A range key (4XX, 5XX) is an error class; the standard covers every 4xx and 5xx.
        const range = /^[1-5]XX$/i.test(code) ? code.toUpperCase() : null;
        if (!range && !/^\d{3}$/.test(code)) continue;
        const response = deref(doc, responses[code]) ?? {};
        if (!range && !STANDARD_CODES.has(code)) {
          add(`status ${id} ${code}`, 'status', [id], `${id} answers ${code}, which the standard's table does not list`);
        }
        if (!range && (code === '201' || code === '202') &&
            !Object.keys(response.headers ?? {}).some((h) => h.toLowerCase() === 'location')) {
          add(`location ${id} ${code}`, 'location', [id], `${id} answers ${code} without a Location header`);
        }
        const isError = range ? range.startsWith('4') || range.startsWith('5') : Number(code) >= 400;
        if (isError && !(PROBLEM in (response.content ?? {}))) {
          const shown = range ?? code;
          add(`problem ${id} ${shown}`, 'problem', [id], `${id} answers ${shown} without ${PROBLEM}`);
        }
      }
    }
  }
  return findings;
}

// Reads the table under "## Deviations and decisions": | `key` | decision | reason |
export function parseDecisions(markdown) {
  const lines = markdown.split('\n');
  const start = lines.findIndex((l) => l.trim() === `## ${SECTION}`);
  if (start < 0) throw new Error(`section "## ${SECTION}" not found`);
  const rows = [];
  for (let i = start + 1; i < lines.length && !/^##\s/.test(lines[i]); i++) {
    const m = /^\|\s*`([^`]+)`\s*\|\s*([^|]*?)\s*\|\s*(.*?)\s*\|\s*$/.exec(lines[i]);
    if (m) rows.push({ key: m[1], decision: m[2], reason: m[3], line: i + 1 });
  }
  return rows;
}

// Returns { findings, rows, problems }; a problem is { key, text } and is always open.
export function audit(doc, standardMarkdown) {
  const findings = computeFindings(doc);
  const rows = parseDecisions(standardMarkdown);
  const byKey = new Map(findings.map((f) => [f.key, f]));
  const rowByKey = new Map();
  const problems = [];

  for (const row of rows) {
    if (rowByKey.has(row.key)) problems.push({ key: row.key, text: `line ${row.line}: duplicate decision row` });
    rowByKey.set(row.key, row);
    if (!DECISIONS.has(row.decision)) {
      problems.push({ key: row.key, text: `line ${row.line}: decision "${row.decision}" is not fix, alias or exception` });
    }
    if (!row.reason) problems.push({ key: row.key, text: `line ${row.line}: no reason given` });
    if (!byKey.has(row.key)) problems.push({ key: row.key, text: `line ${row.line}: row names no current finding (stale)` });
  }

  for (const f of findings) {
    const row = rowByKey.get(f.key);
    if (!row) {
      problems.push({ key: f.key, text: `no decision row: ${f.summary}` });
      continue;
    }
    if (row.decision !== 'exception') continue;
    for (const id of f.ops) {
      const ext = operationAt(doc, id)?.['x-rest-exception'];
      const list = Array.isArray(ext) ? ext : [ext];
      if (!list.some((e) => typeof e === 'string' && e.startsWith(`${f.key}:`))) {
        problems.push({ key: f.key, text: `${id} has no x-rest-exception starting with "${f.key}:"` });
      }
    }
  }
  return { findings, rows, problems };
}

function report({ findings, rows, problems }) {
  const decided = new Map(rows.map((r) => [r.key, r.decision]));
  const out = [];
  for (const f of findings) {
    out.push(`${(decided.get(f.key) ?? 'OPEN').padEnd(9)} ${f.key}`);
  }
  const open = problems.length;
  out.push('');
  const decidedCount = findings.filter((f) => decided.has(f.key)).length;
  out.push(`api-audit: ${findings.length} findings, ${decidedCount} with a decision row, ${open} open problems`);
  for (const p of problems) out.push(`open: ${p.key}: ${p.text}`);
  return out.join('\n');
}

function parseArgs(argv) {
  const opts = {
    check: false,
    spec: path.join(ROOT, 'docs', 'openapi.yaml'),
    standard: path.join(ROOT, 'docs', 'api-standard.md'),
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--check') opts.check = true;
    else if (a === '--list') opts.list = true;
    else if (a === '--spec') opts.spec = argv[++i];
    else if (a === '--standard') opts.standard = argv[++i];
    else throw new Error(`unknown argument: ${a}`);
  }
  return opts;
}

function main(argv) {
  let opts;
  let result;
  try {
    opts = parseArgs(argv);
    const doc = YAML.parse(fs.readFileSync(opts.spec, 'utf8'));
    if (opts.list) {
      const findings = computeFindings(doc);
      process.stdout.write(findings.map((f) => `${f.key}\t${f.summary}`).join('\n') + `\n${findings.length} findings\n`);
      return 0;
    }
    const standard = fs.readFileSync(opts.standard, 'utf8');
    result = audit(doc, standard);
  } catch (err) {
    process.stderr.write(`api-audit: ${err.message}\n`);
    return 2;
  }
  process.stdout.write(`${report(result)}\n`);
  return opts.check && result.problems.length > 0 ? 1 : 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}
