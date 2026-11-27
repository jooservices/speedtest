// Fixture tests for scripts/api-audit.mjs. Each test writes a small spec and decision table
// into a temporary directory and runs the audit CLI on them; the repository's own files are not used.
import { test, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const AUDIT = path.join(ROOT, 'scripts', 'api-audit.mjs');
const dirs = [];

after(() => {
  for (const d of dirs) fs.rmSync(d, { recursive: true, force: true });
});

// The one deviation in the base spec is the PUT action below: an action must be a POST.
// Its PUT carries If-Match, 412 and 428, so it has no if-match finding.
const SPEC = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/widgets:
    get:
      operationId: listWidgets
      deprecated: true
      x-canonical-path: "/api/v1/widgets"
      parameters:
        - name: page
          in: query
        - name: size
          in: query
      responses:
        "200":
          description: ok
  /api/widgets/{id}/ack:
    put:
      operationId: ackWidget
      deprecated: true
      x-canonical-path: "/api/v1/widgets/{id}/ack"
      parameters:
        - name: If-Match
          in: header
          required: true
          schema:
            type: string
      responses:
        "200":
          description: ok
        "412":
          description: stale
          content:
            application/problem+json:
              schema:
                type: object
        "428":
          description: If-Match is required
          content:
            application/problem+json:
              schema:
                type: object
`;

// The table as a repository would hold it: the heading, a header row and one row per finding.
const TABLE_HEAD = `# Standard

## Deviations and decisions

| Finding | Decision | Reason |
| --- | --- | --- |
`;

function fixture({ spec = SPEC, rows = '' } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-audit-'));
  dirs.push(dir);
  const specPath = path.join(dir, 'openapi.yaml');
  const standardPath = path.join(dir, 'api-standard.md');
  fs.writeFileSync(specPath, spec);
  fs.writeFileSync(standardPath, TABLE_HEAD + rows);
  return { specPath, standardPath };
}

function runAudit(args, { specPath, standardPath }) {
  const result = spawnSync(process.execPath, [AUDIT, ...args, '--spec', specPath, '--standard', standardPath], {
    encoding: 'utf8',
  });
  return { code: result.status, out: result.stdout + result.stderr };
}

test('a spec whose only finding has an alias row passes --check', () => {
  const f = fixture({
    rows: '| `action-method PUT /api/widgets/{id}/ack` | alias | Clients send PUT today; keep it. |\n',
  });
  const r = runAudit(['--check'], f);
  assert.equal(r.code, 0, r.out);
  assert.match(r.out, /1 findings, 1 with a decision row, 0 open problems/);
});

test('a finding without a decision row exits 1 and names the finding (AC7)', () => {
  const r = runAudit(['--check'], fixture());
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: action-method PUT \/api\/widgets\/\{id\}\/ack: no decision row/);
});

test('an exception without x-rest-exception exits 1 and names the operation (AC6)', () => {
  const rows = '| `action-method PUT /api/widgets/{id}/ack` | exception | Kept for old clients. |\n';
  const r = runAudit(['--check'], fixture({ rows }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /PUT \/api\/widgets\/\{id\}\/ack has no x-rest-exception starting with "action-method PUT \/api\/widgets\/\{id\}\/ack:"/);
});

test('an exception with a matching x-rest-exception passes --check (AC6)', () => {
  const rows = '| `action-method PUT /api/widgets/{id}/ack` | exception | Kept for old clients. |\n';
  // The extension belongs to the operation, so it goes right after its operationId line.
  const spec = SPEC.replace(
    '      operationId: ackWidget\n',
    '      operationId: ackWidget\n      x-rest-exception: "action-method PUT /api/widgets/{id}/ack: Kept for old clients."\n',
  );
  const r = runAudit(['--check'], fixture({ spec, rows }));
  assert.equal(r.code, 0, r.out);
});

test('a decision row that names no current finding exits 1 and names the row (AC7)', () => {
  const rows =
    '| `action-method PUT /api/widgets/{id}/ack` | alias | Kept. |\n' +
    '| `plural /api/gone` | exception | Removed from the spec. |\n';
  const r = runAudit(['--check'], fixture({ rows }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: plural \/api\/gone: line \d+: row names no current finding \(stale\)/);
});

test('--list names each rule that the fixture breaks', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/Widgets.csv:
    get:
      operationId: exportWidgets
      deprecated: true
      x-canonical-path: "/api/v1/Widgets.csv"
      responses:
        "200":
          description: ok
  /api/gadgets/{id}:
    patch:
      operationId: updateGadget
      deprecated: true
      x-canonical-path: "/api/v1/gadgets/{id}"
      requestBody:
        content:
          application/json:
            schema:
              type: object
      responses:
        "207":
          description: partial
        "409":
          description: conflict
          content:
            text/plain:
              schema:
                type: string
  /api/gadgets:
    post:
      operationId: createGadget
      deprecated: true
      x-canonical-path: "/api/v1/gadgets"
      responses:
        "201":
          description: created
`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  for (const key of [
    'path-charset /api/Widgets.csv',
    'plural /api/Widgets.csv',
    'patch-media PATCH /api/gadgets/{id}',
    'if-match PATCH /api/gadgets/{id}',
    'status PATCH /api/gadgets/{id} 207',
    'problem PATCH /api/gadgets/{id} 409',
    'location POST /api/gadgets 201',
  ]) {
    assert.match(r.out, new RegExp(`^${key.replace(/[{}()/.*+?^$|[\]\\]/g, '\\$&')}\\t`, 'm'), `missing ${key}\n${r.out}`);
  }
});

test('the audit fails with exit 2 when the decision section is missing', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-audit-'));
  dirs.push(dir);
  const standardPath = path.join(dir, 'api-standard.md');
  fs.writeFileSync(standardPath, '# no table here\n');
  const r = runAudit(['--check'], { specPath: fixture().specPath, standardPath });
  assert.equal(r.code, 2, r.out);
  assert.match(r.out, /section "## Deviations and decisions" not found/);
});

// The repository's own files: the table must cover every finding, and removing one row must fail.
test('the repository passes --check with 0 open findings (AC7)', () => {
  const result = spawnSync(process.execPath, [AUDIT, '--check'], { cwd: ROOT, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /, 0 open problems/);
});

test('removing one row of the repository table exits 1 and names the finding (AC7)', () => {
  const standard = fs.readFileSync(path.join(ROOT, 'docs', 'api-standard.md'), 'utf8');
  const removed = 'action-method PUT /api/alerts/events/{id}/ack';
  const lines = standard.split('\n');
  const kept = lines.filter((l) => !l.startsWith(`| \`${removed}\``));
  assert.equal(lines.length - kept.length, 1, 'expected exactly one row for the finding');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'api-audit-'));
  dirs.push(dir);
  const standardPath = path.join(dir, 'api-standard.md');
  fs.writeFileSync(standardPath, kept.join('\n'));
  const r = runAudit(['--check'], { specPath: path.join(ROOT, 'docs', 'openapi.yaml'), standardPath });
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, new RegExp(`open: ${removed.replace(/[{}()/.*+?^$|[\]\\]/g, '\\$&')}: no decision row`));
});

// ---- Negative and positive fixtures for each enforcement branch (review R1 to R7) ----

const PROBLEM_CONTENT = `          content:
            application/problem+json:
              schema:
                type: object
`;

// One PUT on an item. Each concurrency piece can be left out on its own.
function putItemSpec({ ifMatch = true, r412 = true, r428 = true } = {}) {
  const params = ifMatch
    ? `      parameters:
        - name: If-Match
          in: header
          required: true
          schema:
            type: string
`
    : '';
  const r412Block = r412 ? `        "412":\n          description: stale\n${PROBLEM_CONTENT}` : '';
  const r428Block = r428 ? `        "428":\n          description: missing\n${PROBLEM_CONTENT}` : '';
  return `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/widgets/{id}:
    put:
      operationId: updateWidget
      deprecated: true
      x-canonical-path: "/api/v1/widgets/{id}"
${params}      responses:
        "200":
          description: ok
${r412Block}${r428Block}`;
}

// R1: the If-Match header, the 412 and the 428 are checked on their own, not by the 428 alone.
for (const [name, opts, expected] of [
  ['complete', {}, null],
  ['without the If-Match parameter', { ifMatch: false }, 'an If-Match header parameter'],
  ['without the 412 response', { r412: false }, 'a 412 response'],
  ['without the 428 response', { r428: false }, 'a 428 response'],
]) {
  test(`if-match: a PUT on an item ${name} (R1)`, () => {
    const r = runAudit(['--list'], fixture({ spec: putItemSpec(opts) }));
    assert.equal(r.code, 0, r.out);
    const line = r.out.split('\n').find((l) => l.startsWith('if-match PUT /api/widgets/{id}\t'));
    if (expected === null) {
      assert.equal(line, undefined, r.out);
    } else {
      assert.ok(line, `missing if-match finding\n${r.out}`);
      assert.match(line, new RegExp(expected));
    }
  });
}

// R2: a PATCH without a request body is checked too; it must still declare merge-patch.
test('patch-media: a PATCH without a request body is a named finding (R2)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/widgets/{id}:
    patch:
      operationId: patchWidget
      deprecated: true
      x-canonical-path: "/api/v1/widgets/{id}"
      parameters:
        - name: If-Match
          in: header
          required: true
          schema:
            type: string
      responses:
        "200":
          description: ok
        "412":
          description: stale
${PROBLEM_CONTENT}        "428":
          description: missing
${PROBLEM_CONTENT}`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  assert.match(r.out, /^patch-media PATCH \/api\/widgets\/\{id\}\t.*does not accept application\/merge-patch\+json$/m, r.out);
});

// R3: pagination is a query parameter. A header named page or size does not satisfy it.
function listSpec(params, components = '') {
  return `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/widgets:
    get:
      operationId: listWidgets
      deprecated: true
      x-canonical-path: "/api/v1/widgets"
      parameters:
${params}
      responses:
        "200":
          description: ok
${components}`;
}
const PAGE_SIZE_HEADER = `        - name: page
          in: header
          schema:
            type: integer
        - name: size
          in: header
          schema:
            type: integer
`;
const PAGE_SIZE_QUERY = `        - name: page
          in: query
          schema:
            type: integer
        - name: size
          in: query
          schema:
            type: integer
`;
const PAGE_SIZE_REFS = `        - $ref: "#/components/parameters/Page"
        - $ref: "#/components/parameters/Size"
`;
const PAGE_SIZE_COMPONENTS = `components:
  parameters:
    Page:
      name: page
      in: query
      schema:
        type: integer
    Size:
      name: size
      in: query
      schema:
        type: integer
`;

test('list-page: page and size as headers do not satisfy pagination (R3)', () => {
  const r = runAudit(['--list'], fixture({ spec: listSpec(PAGE_SIZE_HEADER) }));
  assert.equal(r.code, 0, r.out);
  assert.match(r.out, /^list-page GET \/api\/widgets\t/m, r.out);
});

test('list-page: query page and size satisfy pagination (R3)', () => {
  const r = runAudit(['--list'], fixture({ spec: listSpec(PAGE_SIZE_QUERY) }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /^list-page /m, r.out);
});

test('list-page: referenced query parameters satisfy pagination (R3)', () => {
  const r = runAudit(['--list'], fixture({ spec: listSpec(PAGE_SIZE_REFS, PAGE_SIZE_COMPONENTS) }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /^list-page /m, r.out);
});

// R4: HEAD, OPTIONS and TRACE are operations too: they need version metadata, a standard
// status and a problem body for errors.
test('HEAD, OPTIONS and TRACE without version metadata are named findings (R4)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/nodes/{id}:
    head:
      operationId: headNode
      responses:
        "503":
          description: down
          content:
            text/plain:
              schema:
                type: string
    options:
      operationId: optionsNode
      responses:
        "200":
          description: ok
    trace:
      operationId: traceNode
      responses:
        "200":
          description: ok
`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  for (const key of [
    'version HEAD /api/nodes/{id}\t',
    'version OPTIONS /api/nodes/{id}\t',
    'version TRACE /api/nodes/{id}\t',
    'status HEAD /api/nodes/{id} 503\t',
    'problem HEAD /api/nodes/{id} 503\t',
  ]) {
    assert.ok(r.out.includes(key), `missing ${key}\n${r.out}`);
  }
});

// R5: range keys (4XX, 5XX) are checked for problem content as well as numeric codes.
test('a 4XX or 5XX response with text/plain is a named problem finding (R5)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/nodes:
    get:
      operationId: listNodes
      deprecated: true
      x-canonical-path: "/api/v1/nodes"
      parameters:
${PAGE_SIZE_QUERY}      responses:
        "200":
          description: ok
        "4XX":
          description: client error
          content:
            text/plain:
              schema:
                type: string
        "5XX":
          description: server error
          content:
            text/plain:
              schema:
                type: string
`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  assert.ok(r.out.includes('problem GET /api/nodes 4XX\t'), r.out);
  assert.ok(r.out.includes('problem GET /api/nodes 5XX\t'), r.out);
});

// R6: the standard permits a verb segment before an {id} (its own example is
// /api/notifications/test/{channel}); a noun before an {id} still needs the plural.
test('the action allowance: /api/notifications/test/{channel} is not a plural finding (R6)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/notifications/test/{channel}:
    post:
      operationId: testNotification
      deprecated: true
      x-canonical-path: "/api/v1/notifications/test/{channel}"
      responses:
        "202":
          description: accepted
          headers:
            Location:
              schema:
                type: string
  /api/gadget/{id}:
    get:
      operationId: getGadget
      deprecated: true
      x-canonical-path: "/api/v1/gadget/{id}"
      responses:
        "200":
          description: ok
`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  assert.ok(!r.out.includes('plural /api/notifications/test/{channel}'), r.out);
  assert.ok(r.out.includes('plural /api/gadget/{id}\t'), 'a noun before {id} must still be a plural finding\n' + r.out);
});

// R7: the remaining enforcement branches, each with its own negative case and a nonzero --check.
test('path-shape: a trailing slash and an empty segment are named findings (R7)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/widgets/:
    get:
      operationId: a
      deprecated: true
      x-canonical-path: "/api/v1/widgets/"
      parameters:
${PAGE_SIZE_QUERY}      responses:
        "200":
          description: ok
  /api//gadgets:
    get:
      operationId: b
      deprecated: true
      x-canonical-path: "/api/v1//gadgets"
      responses:
        "200":
          description: ok
`;
  const r = runAudit(['--check'], fixture({ spec }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: path-shape \/api\/widgets\/: no decision row/);
  assert.match(r.out, /open: path-shape \/api\/\/gadgets: no decision row/);
});

test('get-body: a GET with a request body is a named finding (R7)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/gadget/{id}:
    get:
      operationId: getGadget
      deprecated: true
      x-canonical-path: "/api/v1/gadget/{id}"
      requestBody:
        content:
          application/json:
            schema:
              type: object
      responses:
        "200":
          description: ok
`;
  const r = runAudit(['--check'], fixture({ spec }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: get-body GET \/api\/gadget\/\{id\}: no decision row/);
});

test('version: a missing deprecated flag or a wrong x-canonical-path is named (R7)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/widgets:
    get:
      operationId: a
      x-canonical-path: "/api/v1/widgets"
      parameters:
${PAGE_SIZE_QUERY}      responses:
        "200":
          description: ok
  /api/gadgets:
    get:
      operationId: b
      deprecated: true
      x-canonical-path: "/api/v2/gadgets"
      responses:
        "200":
          description: ok
`;
  const r = runAudit(['--check'], fixture({ spec }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: version GET \/api\/widgets: no decision row/);
  assert.match(r.out, /open: version GET \/api\/gadgets: no decision row/);
});

test('duplicate decision rows exit 1 and name the key (R7)', () => {
  const rows =
    '| `action-method PUT /api/widgets/{id}/ack` | alias | Kept. |\n' +
    '| `action-method PUT /api/widgets/{id}/ack` | alias | Kept again. |\n';
  const r = runAudit(['--check'], fixture({ rows }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: action-method PUT \/api\/widgets\/\{id\}\/ack: line \d+: duplicate decision row/);
});

test('an invalid decision exits 1 and names the key (R7)', () => {
  const rows = '| `action-method PUT /api/widgets/{id}/ack` | maybe | Kept. |\n';
  const r = runAudit(['--check'], fixture({ rows }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: action-method PUT \/api\/widgets\/\{id\}\/ack: line \d+: decision "maybe" is not fix, alias or exception/);
});

test('a row with an empty reason exits 1 and names the key (R7)', () => {
  const rows = '| `action-method PUT /api/widgets/{id}/ack` | alias |  |\n';
  const r = runAudit(['--check'], fixture({ rows }));
  assert.equal(r.code, 1, r.out);
  assert.match(r.out, /open: action-method PUT \/api\/widgets\/\{id\}\/ack: line \d+: no reason given/);
});

// R1: parameters declared on the path item are inherited by every operation. Path-level
// query and header parameters must satisfy the same rules as operation-level ones.
// The path-level list sits two columns shallower than the operation-level blocks, so dedent them.
const dedent = (block) => block.replace(/^ {2}/gm, '');
const PATH_LEVEL_PAGING = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/nodes:
    parameters:
${dedent(PAGE_SIZE_QUERY)}    get:
      operationId: listNodes
      deprecated: true
      x-canonical-path: "/api/v1/nodes"
      responses:
        "200":
          description: ok
`;

test('list-page: inline path-level page and size satisfy pagination (R1)', () => {
  const r = runAudit(['--list'], fixture({ spec: PATH_LEVEL_PAGING }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /^list-page /m, r.out);
});

test('list-page: referenced path-level page and size satisfy pagination (R1)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/nodes:
    parameters:
${dedent(PAGE_SIZE_REFS)}    get:
      operationId: listNodes
      deprecated: true
      x-canonical-path: "/api/v1/nodes"
      responses:
        "200":
          description: ok
${PAGE_SIZE_COMPONENTS}`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /^list-page /m, r.out);
});

// The If-Match header declared on the path item satisfies the concurrency rule, inline or referenced.
function patchItemSpec(paramBlock, components = '') {
  return `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/nodes/{id}:
    parameters:
${paramBlock}    patch:
      operationId: updateNode
      deprecated: true
      x-canonical-path: "/api/v1/nodes/{id}"
      requestBody:
        content:
          application/merge-patch+json:
            schema:
              type: object
      responses:
        "200":
          description: ok
        "412":
          description: stale
${PROBLEM_CONTENT}        "428":
          description: missing
${PROBLEM_CONTENT}${components}`;
}

test('if-match: an inline If-Match header on the path item satisfies the rule (R1)', () => {
  const block = `      - name: If-Match
        in: header
        required: true
        schema:
          type: string
`;
  const r = runAudit(['--list'], fixture({ spec: patchItemSpec(block) }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /^if-match /m, r.out);
});

test('if-match: a referenced If-Match header on the path item satisfies the rule (R1)', () => {
  const block = `      - $ref: "#/components/parameters/IfMatch"
`;
  const components = `components:
  parameters:
    IfMatch:
      name: If-Match
      in: header
      required: true
      schema:
        type: string
`;
  const r = runAudit(['--list'], fixture({ spec: patchItemSpec(block, components) }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /^if-match /m, r.out);
});

// R2: the action path accepts POST; any other method on it is a named action-method finding.
test('the action path: POST passes and GET is a named action-method finding (R2)', () => {
  const spec = `openapi: 3.1.0
info:
  title: fixture
  version: 1.0.0
paths:
  /api/notifications/test/{channel}:
    post:
      operationId: testNotification
      deprecated: true
      x-canonical-path: "/api/v1/notifications/test/{channel}"
      responses:
        "202":
          description: accepted
          headers:
            Location:
              schema:
                type: string
    get:
      operationId: getTestNotification
      deprecated: true
      x-canonical-path: "/api/v1/notifications/test/{channel}"
      responses:
        "200":
          description: ok
`;
  const r = runAudit(['--list'], fixture({ spec }));
  assert.equal(r.code, 0, r.out);
  assert.doesNotMatch(r.out, /action-method POST /m, r.out);
  assert.match(r.out, /^action-method GET \/api\/notifications\/test\/\{channel\}\t/m, r.out);
  assert.doesNotMatch(r.out, /^plural /m, r.out);
});
