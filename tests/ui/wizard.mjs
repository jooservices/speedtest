// The add-node wizard against the contract fixture: SSH install with host-key confirmation, live progress and failure.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { startServer } from '../ui-fixtures/server.mjs';
import { trackCoverage } from '../lib/v8cov.mjs';

const server = await startServer();
const browser = process.env.PW_TEST_CONNECT_WS_ENDPOINT ? await chromium.connect(process.env.PW_TEST_CONNECT_WS_ENDPOINT)
  : await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
const page = await browser.newPage();
const coverage = await trackCoverage(page);
const problems = [];
page.on('pageerror', e => problems.push(e.message));
page.on('console', m => { if (['error', 'warning'].includes(m.type())) problems.push(m.text()); });

async function describeNode(name) {
  await page.goto(`${server.origin}/#/nodes`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByRole('button', { name: 'Add a node' }).first().click();
  const dialog = page.getByRole('dialog', { name: 'Add a node' });
  await dialog.getByLabel('Node name').fill(name);
  await dialog.getByLabel('Expected download').fill('100');
  await dialog.getByLabel('Expected upload').fill('50');
  await dialog.getByRole('button', { name: 'Continue' }).click();
  await dialog.getByLabel('Machine address').fill('192.0.2.7');
  await dialog.getByLabel('User name').fill('ubuntu');
  await dialog.getByLabel('Password', { exact: true }).fill('correct horse battery');
  return dialog;
}

try {
  // Happy path: probe, explicit fingerprint confirmation, installation with live progress, node online.
  let dialog = await describeNode('Wizard 1');
  await dialog.getByRole('button', { name: 'Check the machine' }).click();
  await dialog.getByText('SHA256:fixtureFingerprint0123456789').waitFor();
  assert.equal(server.control.requests.some(r => r.path.endsWith('/installations')), false, 'no credential may be sent before the fingerprint is confirmed');
  const probeCall = server.control.requests.find(r => r.path === '/api/installer/probes');
  assert.deepEqual(probeCall.body, { host: '192.0.2.7', port: 22 }, 'the probe carries no credential');
  assert.ok(probeCall.headers['idempotency-key']);
  await dialog.getByRole('button', { name: 'Trust it and install' }).click();
  await dialog.getByText('Downloading the program').waitFor();
  await dialog.getByText('Wizard 1 is online').or(dialog.getByText('Contract node is online')).waitFor({ timeout: 10000 });
  const install = server.control.lastInstall;
  assert.equal(install.fingerprint, 'SHA256:fixtureFingerprint0123456789');
  assert.equal(install.probeId, 'c0ffee00-0000-4000-8000-000000000001');
  assert.deepEqual({ ...install.ssh, secret: '(hidden)' }, { host: '192.0.2.7', port: 22, user: 'ubuntu', auth: 'password', secret: '(hidden)' });
  assert.ok(!(await page.content()).includes('correct horse battery'), 'the password never stays in the page');
  await dialog.getByRole('button', { name: 'Open node' }).click();

  // Failure path: a plain-language reason, nothing left half installed, and a way back.
  server.control.installFails = true;
  dialog = await describeNode('Wizard 2');
  await dialog.getByRole('button', { name: 'Check the machine' }).click();
  await dialog.getByRole('button', { name: 'Trust it and install' }).click();
  await dialog.getByText('Sudo is not allowed for this account.').waitFor();
  await dialog.getByText('Nothing was left half-installed.').waitFor();
  await dialog.getByRole('button', { name: 'Back' }).click();
  await dialog.getByLabel('Machine address').waitFor();

  // Missing details are refused before anything is sent.
  await dialog.getByLabel('Machine address').fill('not a host!');
  await dialog.getByRole('button', { name: 'Check the machine' }).click();
  await dialog.getByText('Enter a valid IP address or host name for the machine.').waitFor();
  await page.keyboard.press('Escape');

  // Removing a node: uninstall the program first, then delete the node, only after the machine confirms.
  server.control.installFails = false;
  const deletes = () => server.control.requests.filter(r => r.method === 'DELETE' && r.path.startsWith('/api/nodes/'));
  async function openRemoval() {
    await page.goto(`${server.origin}/#/nodes/node-contract`);
    await page.waitForFunction(() => ST.remote.status === 'ready');
    await page.getByRole('tab', { name: 'Settings' }).click();
    await page.getByRole('button', { name: /^Remove / }).click();
    return page.getByRole('dialog', { name: /^Remove / });
  }
  let removal = await openRemoval();
  await removal.getByLabel('Machine address').fill('192.0.2.7');
  await removal.getByLabel('User name').fill('ubuntu');
  await removal.getByLabel('Password', { exact: true }).fill('correct horse battery');
  await removal.getByRole('button', { name: 'Check the machine' }).click();
  await removal.getByText('SHA256:fixtureFingerprint0123456789').waitFor();
  assert.equal(deletes().length, 0, 'nothing is deleted before the machine is clean');
  await removal.getByRole('button', { name: 'Trust it and remove' }).click();
  await page.waitForFunction(() => location.hash === '#/nodes', null, { timeout: 10000 });
  assert.equal(server.control.lastInstallKind, 'uninstall');
  assert.equal(deletes().length, 1, 'the node is deleted once the removal succeeded');
  assert.ok(deletes()[0].query.get('confirm') === 'true');
  assert.ok(!(await page.content()).includes('correct horse battery'));

  // A removal that fails keeps the node and says how to finish by hand.
  server.control.installFails = true;
  removal = await openRemoval();
  await removal.getByLabel('Machine address').fill('192.0.2.7');
  await removal.getByLabel('User name').fill('ubuntu');
  await removal.getByLabel('Password', { exact: true }).fill('x');
  await removal.getByRole('button', { name: 'Check the machine' }).click();
  await removal.getByRole('button', { name: 'Trust it and remove' }).click();
  await removal.getByText('Sudo is not allowed for this account.').waitFor();
  await removal.getByText(/sudo speedtest-agent uninstall --all/).waitFor();
  assert.equal(deletes().length, 1, 'a failed removal must not delete the node');
  await removal.getByRole('button', { name: 'Remove from the dashboard anyway' }).click();
  await page.waitForFunction(() => location.hash === '#/nodes');
  assert.equal(deletes().length, 2);

  // Dashboard only: the one-line command is shown instead of any SSH step.
  server.control.installFails = false;
  removal = await openRemoval();
  await removal.getByRole('button', { name: /Only remove from the dashboard/ }).click();
  await removal.getByText('sudo speedtest-agent uninstall --all').waitFor();
  await removal.getByRole('button', { name: /^Remove Contract node/ }).click();
  await page.waitForFunction(() => location.hash === '#/nodes');
  assert.equal(deletes().length, 3);
  assert.deepEqual(problems, [], 'no console problems');
  console.log('PASS wizard: SSH install with confirmation, live progress, failure and validation; node removal with uninstall');
} finally {
  await coverage.finish('wizard');
  await browser.close();
  await server.close();
}
