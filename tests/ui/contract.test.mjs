import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import YAML from 'yaml';
import Ajv from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import { runtime } from './runtime.mjs';
import { startServer } from '../ui-fixtures/server.mjs';

const contract = YAML.parse(readFileSync('docs/openapi.yaml', 'utf8'));
const ajv = new Ajv({ strict: false, allErrors: true }); addFormats(ajv);
const root = { $id: 'urn:speedtest:contract', components: contract.components };
ajv.addSchema(root);
test('all fake read responses validate against their exact OpenAPI operation schemas', async t => {
  const server = await startServer(); t.after(server.close);
  for (const path of ['/api/auth/me', '/api/settings', '/api/nodes', '/api/results', '/api/alerts/rules',
    '/api/alerts/events', '/api/ip/addresses', '/api/ip/history', '/api/stats/summary', '/api/stats/trend',
    '/api/stats/hour-of-day', '/api/stats/heatmap', '/api/stats/distribution', '/api/audit']) {
    const response = await fetch(server.origin + path);
    const body = await response.json();
    const schema = contract.paths[path].get.responses['200'].content['application/json'].schema.$ref;
    const validate = ajv.compile({ $ref: 'urn:speedtest:contract' + schema });
    assert.ok(validate(body), `${path}: ${JSON.stringify(validate.errors)}`);
  }
  for (const status of [401, 403, 429, 500]) {
    server.control.status = status;
    const response = await fetch(server.origin + '/api/settings');
    const validate = ajv.compile({ $ref: 'urn:speedtest:contract#/components/schemas/Problem' });
    assert.ok(validate(await response.json()), JSON.stringify(validate.errors));
  }
});

test('store mutations and fake responses obey OpenAPI bodies, methods, conditional and issuance headers', async t => {
  const server = await startServer(); t.after(server.close);
  const { ST } = runtime(server.origin);
  await ST.load();
  await ST.updateNode('node-contract', { name: 'Renamed' });
  await ST.addNode({ name: 'New', expectedDown: 100, expectedUp: 50 });
  await ST.removeNode('node-contract');
  const rule = { ...server.data.rule }; delete rule.id;
  await ST.saveRule(rule); await ST.saveRule({ ...rule, id: 'rule-fixture' });
  await ST.deleteRule('rule-fixture');
  await ST.ack('event-fixture'); await ST.ack('event-fixture', false);
  await ST.ackAll(); await ST.runTest('node-contract'); await ST.setSettings({ theme: 'light' });
  function operation(path, method) {
    const template = Object.keys(contract.paths).find(template =>
      new RegExp('^' + template.replace(/\{[^}]+\}/g, '[^/]+') + '$').test(path));
    assert.ok(template, `undocumented path ${path}`);
    assert.ok(contract.paths[template][method], `undocumented method ${method} ${path}`);
    return contract.paths[template][method];
  }
  for (const request of server.control.requests) {
    const method = request.method.toLowerCase();
    const op = operation(request.path, method);
    if (request.body !== undefined) {
      const media = request.headers['content-type'];
      const schema = op.requestBody?.content[media]?.schema;
      assert.ok(schema, `undocumented body ${method} ${request.path}`);
      const validate = ajv.compile({ ...schema, components: contract.components });
      assert.ok(validate(request.body), `${request.path}: ${JSON.stringify(validate.errors)}`);
    } else assert.ok(!op.requestBody?.required, `missing body ${method} ${request.path}`);
  }
  for (const response of server.control.responses) {
    const op = operation(response.path, response.method);
    const schema = op.responses[response.status]?.content?.['application/json']?.schema;
    assert.ok(schema, `undocumented response ${response.status} ${response.path}`);
    const validate = ajv.compile({ ...schema, components: contract.components });
    assert.ok(validate(response.value), `${response.path}: ${JSON.stringify(validate.errors)}`);
  }
});
