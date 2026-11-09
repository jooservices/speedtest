import assert from 'node:assert/strict';
import test from 'node:test';
import { e2ePort } from './lib.mjs';

test('E2E HTTP port defaults to a free loopback port and validates overrides', async () => {
  const random = await e2ePort('');
  assert.ok(Number.isInteger(random) && random >= 1024 && random <= 65535);
  assert.equal(await e2ePort('18485'), 18485);
  for (const invalid of ['0', '65536', 'abc', '80']) {
    await assert.rejects(e2ePort(invalid), /E2E_HTTP_PORT must be an integer between 1024 and 65535/);
  }
});
