import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { e2ePort } from './lib.mjs';

test('E2E HTTP port defaults to a free loopback port and validates overrides', async () => {
  const random = await e2ePort('');
  assert.ok(Number.isInteger(random) && random >= 1024 && random <= 65535);
  assert.equal(await e2ePort('18485'), 18485);
  for (const invalid of ['0', '65536', 'abc', '80']) {
    await assert.rejects(e2ePort(invalid), /E2E_HTTP_PORT must be an integer between 1024 and 65535/);
  }
});

test('browser gates declare the application port for Workspace routing', () => {
  const profile = JSON.parse(readFileSync(new URL('../../.ai/project.json', import.meta.url), 'utf8'));
  for (const name of ['e2e', 'ui-coverage']) {
    const stage = profile.stages.find(candidate => candidate.name === name);
    assert.equal(stage?.browser?.portEnv, 'E2E_HTTP_PORT', `${name} must use the Workspace browser host`);
  }
});
