// Browser end-to-end scenarios against a real command center, real agents and a seeded database.
// Run with `make e2e` (needs a worktree .env, psql, Go and a Chromium; see docs/e2e.md).
import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import http from 'node:http';
import { startStack, startAgent, stopAll, query, root } from './lib.mjs';
import { steps } from './steps.mjs';

const shots = join(root, '.tmp', 'e2e', 'shots');
mkdirSync(shots, { recursive: true });
const stack = await startStack();
const browser = process.env.PW_TEST_CONNECT_WS_ENDPOINT ? await chromium.connect(process.env.PW_TEST_CONNECT_WS_ENDPOINT)
  : await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, acceptDownloads: true });
const page = await context.newPage();
const problems = [];
page.on('console', m => { if (m.type() === 'error' || m.type() === 'warning') problems.push(`${m.type()}: ${m.text()}`); });
page.on('pageerror', e => problems.push(`pageerror: ${e.message}`));

// A local receiver for webhook test messages.
const receiver = { bodies: [], url: '' };
const receiverServer = http.createServer((req, res) => { const chunks = []; req.on('data', c => chunks.push(c)); req.on('end', () => { try { receiver.bodies.push(JSON.parse(Buffer.concat(chunks).toString())); } catch { receiver.bodies.push({ raw: true }); } res.writeHead(200); res.end('ok'); }); });
await new Promise(resolve => receiverServer.listen(0, '127.0.0.1', resolve));
receiver.url = `http://127.0.0.1:${receiverServer.address().port}/hook`;
const S = steps({ page, stack, startAgent, query, problems });
const scenarios = [
  ['signing in needs the right password', S.signInRules],
  ['the dashboard shows the seeded fleet', S.dashboardShowsFleet],
  ['every page draws real data without console errors', S.allPagesDraw],
  ['every page holds its layout at 390, 768 and 1440 px in both themes', S.layoutHolds],
  ['results can be filtered, opened and exported', S.resultsFilterAndExport],
  ['a node is added with the install command and reports', S.addNodeAndRunTest],
  ['schedules are saved on the center', S.schedulesSave],
  ['alert rules can be created, edited, switched and deleted; events acknowledged', S.alertRules],
  ['settings are saved, secrets stay hidden and the enrolment key is shown once', S.settingsSave],
  ['a test notification reaches the configured channel', () => S.notificationTest(receiver)],
  ['signing out ends the session', S.signOut]
];
if (process.env.E2E_TRACE) page.on('response', r => { if (r.url().includes('/api/') && r.request().method() !== 'GET') console.log('  ', r.request().method(), r.url().replace(stack.origin, ''), r.status()); });
let failed = 0;
const only = process.env.E2E_ONLY;
for (const [name, scenario] of scenarios) {
  if (only && !name.includes(only) && !['signing in needs the right password'].includes(name)) continue;
  problems.length = 0;
  // A failed scenario can leave a dialog open; start every scenario from a clean page.
  await page.evaluate(() => document.querySelectorAll('.overlay, .drawer-wrap').forEach(e => e.remove())).catch(() => {});
  const started = Date.now();
  try {
    await scenario();
    if (problems.length) throw new Error('console problems: ' + problems.join(' | '));
    console.log(`PASS ${name} (${((Date.now() - started) / 1000).toFixed(1)}s)`);
  } catch (error) {
    failed++;
    await page.screenshot({ path: join(shots, `FAIL-${name.replaceAll(/[^a-z0-9]+/gi, '-')}.png`) }).catch(() => {});
    console.log(`FAIL ${name}: ${error.message}`);
  }
}
await browser.close();
receiverServer.close();
stopAll();
void query;
process.exit(failed ? 1 : 0);
