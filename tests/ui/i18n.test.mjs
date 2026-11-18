import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromium } from 'playwright';
import { runtime } from './runtime.mjs';
import { startServer } from '../ui-fixtures/server.mjs';

async function openBrowserPage(t, options = {}) {
  const browser = process.env.PW_TEST_CONNECT_WS_ENDPOINT ? await chromium.connect(process.env.PW_TEST_CONNECT_WS_ENDPOINT)
    : await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ?
      { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } : {}) });
  t.after(() => browser.close());
  const context = await browser.newContext(options);
  t.after(() => context.close());
  return context.newPage();
}

async function fulfillListPage(route, rows) {
  const response = await route.fetch();
  const payload = await response.json();
  const url = new URL(route.request().url());
  const page = Number(url.searchParams.get('page') || 1);
  const size = Number(url.searchParams.get('size') || 100);
  payload.data = rows.slice((page - 1) * size, page * size);
  payload.meta.pagination = { current_page: page, per_page: size, total: rows.length, last_page: Math.max(1, Math.ceil(rows.length / size)) };
  await route.fulfill({ response, body: JSON.stringify(payload) });
}

test('AC2 localize keeps English date and number formatting and applies vi-VN formatting', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  const timestamp = Date.parse('2026-11-17T09:08:07Z');
  const dateOptions = { timeZone: 'UTC', hour12: false, day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' };
  ST.setSettings({ timezone: 'UTC', units: 'Mbps' });

  assert.equal(ST.i18n.lang(), 'en');
  assert.equal(ST.fmt.num(12345.6, 1), '12,345.6');
  assert.equal(ST.fmt.date(timestamp, 'full'), new Intl.DateTimeFormat('en-GB', dateOptions).format(timestamp));

  ST.i18n.setLanguage('vi');
  assert.equal(ST.fmt.num(12345.6, 1), '12.345,6');
  assert.equal(ST.fmt.date(timestamp, 'full'), new Intl.DateTimeFormat('vi-VN', dateOptions).format(timestamp));
  assert.equal(ST.i18n.localize(64500, { type: 'identifier' }), '64500');
  assert.equal(ST.fmt.speed(1234.5), '1.234,5 Mbps');
});

test('AC1 demo language preference is applied and survives a reload', () => {
  const storage = new Map();
  const first = runtime('http://localhost', { demo: true, storage });
  first.ST.setSettings({ language: 'vi' });
  assert.equal(first.ST.i18n.lang(), 'vi');

  const reloaded = runtime('http://localhost', { demo: true, storage });
  assert.equal(reloaded.ST.settings().language, 'vi');
  assert.equal(reloaded.ST.i18n.lang(), 'vi');
});

test('AC1 remote language save uses settings API and applies only a successful response', async t => {
  const server = await startServer();
  t.after(server.close);
  const { ST } = runtime(server.origin);
  await ST.load();

  await ST.saveLanguage('vi');
  const write = server.control.requests.find(request => request.method === 'PUT' && request.path === '/api/settings');
  assert.equal(write.body.language, 'vi');
  assert.equal(ST.settings().language, 'vi');
  assert.equal(ST.i18n.lang(), 'vi');

  server.control.failWrites = 1;
  server.control.failWriteStatus = 500;
  await assert.rejects(ST.saveLanguage('en'), { status: 500 });
  assert.equal(ST.settings().language, 'vi');
  assert.equal(ST.i18n.lang(), 'vi');
});

test('AC3 a successful language write falls back to its saved value when reconciliation read fails', async t => {
  const server = await startServer();
  t.after(server.close);
  let failSettingsRead = false;
  const fetch = async (url, init) => {
    const path = new URL(url).pathname;
    const method = (init && init.method || 'GET').toUpperCase();
    if (path === '/api/settings' && method === 'GET' && failSettingsRead) {
      failSettingsRead = false;
      return new Response(JSON.stringify({ type: 'about:blank', title: 'Temporary failure', status: 503,
        detail: 'The settings read failed.' }), { status: 503, headers: { 'content-type': 'application/problem+json' } });
    }
    const response = await globalThis.fetch(url, init);
    if (path === '/api/settings' && method === 'PUT' && response.ok) failSettingsRead = true;
    return response;
  };
  const { ST } = runtime(server.origin, { fetch });
  await ST.load();

  const outcome = await ST.saveLanguage('vi');

  assert.equal(outcome.stale, false);
  assert.equal(server.data.settings.language, 'vi');
  assert.equal(ST.settings().language, 'vi');
  assert.equal(ST.i18n.lang(), 'vi');
});

test('AC3 the newest server language read wins when responses arrive out of order', () => {
  const { ST } = runtime('http://localhost', { demo: true });
  const earlierRead = ST.i18n.beginLanguageRead();
  const latestRead = ST.i18n.beginLanguageRead();

  assert.equal(ST.i18n.applyLanguageRead('en', latestRead), true);
  assert.equal(ST.i18n.applyLanguageRead('vi', earlierRead), false);
  assert.equal(ST.i18n.lang(), 'en');
});

test('AC3 reversed language save responses leave the UI at the last saved server language', async () => {
  const { ST } = runtime('http://localhost', { demo: true });
  let serverLanguage = 'en';
  const writes = [];
  const persist = language => new Promise(resolve => writes.push({ language, resolve: () => resolve({ language }) }));
  const readLanguage = () => {
    const read = ST.i18n.beginLanguageRead();
    return Promise.resolve(ST.i18n.applyLanguageRead(serverLanguage, read));
  };

  const earlierSave = ST.i18n.saveLanguage('vi', persist, readLanguage);
  const laterSave = ST.i18n.saveLanguage('en', persist, readLanguage);
  await Promise.resolve();
  assert.deepEqual(writes.map(write => write.language), ['vi', 'en']);

  serverLanguage = 'en';
  writes[1].resolve();
  await laterSave;
  writes[0].resolve();
  await earlierSave;

  assert.equal(ST.i18n.lang(), 'en');
});

test('AC3 a successful Vietnamese save stays successful when its follow-up read fails', async t => {
  const server = await startServer();
  t.after(server.close);
  const page = await openBrowserPage(t);
  let failFollowUpRead = false;

  await page.route('**/api/settings', async route => {
    const request = route.request();
    if (request.method() === 'GET' && failFollowUpRead) {
      failFollowUpRead = false;
      return route.fulfill({ status: 503, contentType: 'application/problem+json', body: JSON.stringify({
        type: 'about:blank', title: 'Temporary failure', status: 503, detail: 'The settings read failed.'
      }) });
    }
    if (request.method() === 'PUT' && request.postDataJSON().language === 'vi') {
      const response = await route.fetch();
      failFollowUpRead = true;
      return route.fulfill({ response });
    }
    return route.continue();
  });

  await page.goto(`${server.origin}/#/settings`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByLabel('Language', { exact: true }).selectOption('vi');
  await page.getByText('Saved', { exact: true }).waitFor();

  assert.equal(server.data.settings.language, 'vi');
  assert.equal(await page.evaluate(() => ST.settings().language), 'vi');
  assert.equal(await page.evaluate(() => ST.i18n.lang()), 'vi');
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'vi');
  assert.equal(await page.locator('#toasts .toast.error').count(), 0);
  assert.equal(await page.getByText('Could not save language', { exact: true }).count(), 0);
  assert.equal(await page.getByText('Không thể lưu ngôn ngữ', { exact: true }).count(), 0);
});

test('AC3 an earlier save failure is silent after a later language save succeeds', async () => {
  const { ST } = runtime('http://localhost', { demo: true });
  let serverLanguage = 'en';
  let delayFirstRead = true;
  let releaseFirstRead;
  const firstReadGate = new Promise(resolve => { releaseFirstRead = resolve; });
  let firstReadStarted;
  const firstReadReady = new Promise(resolve => { firstReadStarted = resolve; });
  const readLanguage = () => {
    const read = ST.i18n.beginLanguageRead();
    if (delayFirstRead) {
      delayFirstRead = false;
      firstReadStarted();
      return firstReadGate.then(() => ST.i18n.applyLanguageRead(serverLanguage, read));
    }
    return Promise.resolve(ST.i18n.applyLanguageRead(serverLanguage, read));
  };
  const events = [];
  const firstSave = ST.i18n.saveLanguage('vi', () => Promise.reject(new Error('offline')), readLanguage)
    .then(result => { if (!result.stale) events.push('first saved'); }, () => events.push('first error'));
  await firstReadReady;

  const secondSave = ST.i18n.saveLanguage('en', () => {
    serverLanguage = 'en';
    return Promise.resolve({ language: 'en' });
  }, readLanguage).then(result => { if (!result.stale) events.push('second saved'); }, () => events.push('second error'));
  await secondSave;
  releaseFirstRead();
  await firstSave;

  assert.deepEqual(events, ['second saved']);
  assert.equal(ST.i18n.lang(), 'en');
});

test('AC3 a failed English save rereads and keeps the successful Vietnamese server value', async t => {
  const server = await startServer();
  t.after(server.close);
  const page = await openBrowserPage(t);
  let firstWritePersisted;
  const firstPersisted = new Promise(resolve => { firstWritePersisted = resolve; });
  let releaseFirstWrite;
  const firstWriteGate = new Promise(resolve => { releaseFirstWrite = resolve; });
  t.after(() => releaseFirstWrite());

  await page.route('**/api/settings', async route => {
    const request = route.request();
    if (request.method() !== 'PUT') return route.continue();
    const language = request.postDataJSON().language;
    if (language === 'vi') {
      const response = await route.fetch();
      firstWritePersisted();
      await firstWriteGate;
      return route.fulfill({ response });
    }
    if (language === 'en' && server.data.settings.language === 'vi') {
      return route.fulfill({ status: 503, contentType: 'application/problem+json', body: JSON.stringify({
        type: 'about:blank', title: 'Temporary failure', status: 503, detail: 'Retry the settings save.'
      }) });
    }
    return route.continue();
  });

  await page.goto(`${server.origin}/#/settings`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  const selector = page.getByLabel('Language', { exact: true });
  await selector.selectOption('vi');
  await firstPersisted;
  assert.equal(server.data.settings.language, 'vi');
  await page.getByLabel('Language', { exact: true }).selectOption('en');
  releaseFirstWrite();

  await page.getByText('Không thể lưu ngôn ngữ', { exact: true }).waitFor();
  assert.equal(server.data.settings.language, 'vi');
  assert.equal(await page.evaluate(() => ST.settings().language), 'vi');
  assert.equal(await page.evaluate(() => ST.i18n.lang()), 'vi');
  assert.equal(await page.locator('html').getAttribute('lang'), 'vi');
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'vi');
});

test('AC3 R1 a Vietnamese save applies after an English load when the held PUT succeeds', async t => {
  const server = await startServer();
  t.after(server.close);
  const page = await openBrowserPage(t);
  let releasePut;
  const putGate = new Promise(resolve => { releasePut = resolve; });
  let heldPut;
  const putHeld = new Promise(resolve => { heldPut = resolve; });
  t.after(() => releasePut());

  await page.route('**/api/settings', async route => {
    const request = route.request();
    if (request.method() !== 'PUT' || request.postDataJSON().language !== 'vi') return route.continue();
    heldPut();
    await putGate;
    return route.continue();
  });
  await page.goto(`${server.origin}/#/settings`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByLabel('Language', { exact: true }).selectOption('vi');
  await putHeld;

  assert.equal(server.data.settings.language, 'en');
  await page.evaluate(() => ST.load());
  assert.equal(await page.evaluate(() => ST.settings().language), 'en');
  releasePut();
  await page.getByText('Saved', { exact: true }).waitFor();

  assert.equal(server.data.settings.language, 'vi');
  assert.equal(await page.evaluate(() => ST.settings().language), 'vi');
  assert.equal(await page.evaluate(() => ST.i18n.lang()), 'vi');
  assert.equal(await page.locator('html').getAttribute('lang'), 'vi');
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'vi');
});

test('AC3 R2 a failed language save during an English load restores Vietnamese and shows the translated error', async t => {
  const server = await startServer();
  t.after(server.close);
  server.data.settings.language = 'vi';
  const page = await openBrowserPage(t);
  let releasePut;
  const putGate = new Promise(resolve => { releasePut = resolve; });
  let heldPut;
  const putHeld = new Promise(resolve => { heldPut = resolve; });
  t.after(() => releasePut());

  await page.route('**/api/settings', async route => {
    const request = route.request();
    if (request.method() !== 'PUT' || request.postDataJSON().language !== 'en') return route.continue();
    heldPut();
    await putGate;
    return route.fulfill({ status: 500, contentType: 'application/problem+json', body: JSON.stringify({
      type: 'about:blank', title: 'Temporary failure', status: 500, detail: 'Retry the settings save.'
    }) });
  });
  await page.goto(`${server.origin}/#/settings`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByLabel('Language', { exact: true }).selectOption('en');
  await putHeld;

  await page.evaluate(() => ST.load());
  assert.equal(await page.evaluate(() => ST.settings().language), 'vi');
  releasePut();
  await page.getByText('Không thể lưu ngôn ngữ', { exact: true }).waitFor();

  assert.equal(server.data.settings.language, 'vi');
  assert.equal(await page.evaluate(() => ST.settings().language), 'vi');
  assert.equal(await page.evaluate(() => ST.i18n.lang()), 'vi');
  assert.equal(await page.locator('html').getAttribute('lang'), 'vi');
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'vi');
});

test('AC3 R2 an in-flight English settings snapshot cannot revert a saved Vietnamese language', async t => {
  const server = await startServer();
  t.after(server.close);
  let releaseNodes;
  const nodesGate = new Promise(resolve => { releaseNodes = resolve; });
  let nodesRead;
  const nodesReadReady = new Promise(resolve => { nodesRead = resolve; });
  t.after(() => releaseNodes());
  const fetch = async (url, init) => {
    const response = await globalThis.fetch(url, init);
    if (new URL(url).pathname === '/api/nodes') {
      nodesRead();
      await nodesGate;
    }
    return response;
  };
  const { ST } = runtime(server.origin, { fetch });

  const loading = ST.load();
  await nodesReadReady;
  await ST.saveLanguage('vi');
  releaseNodes();
  await loading;

  assert.equal(server.data.settings.language, 'vi');
  assert.equal(ST.settings().language, 'vi');
  assert.equal(ST.i18n.lang(), 'vi');
});

test('AC3 a delayed Vietnamese response rereads English after a newer server save', async t => {
  const server = await startServer();
  t.after(server.close);
  const page = await openBrowserPage(t);
  let firstWritePersisted;
  const firstPersisted = new Promise(resolve => { firstWritePersisted = resolve; });
  let releaseFirstWrite;
  const firstWriteGate = new Promise(resolve => { releaseFirstWrite = resolve; });
  t.after(() => releaseFirstWrite());

  await page.route('**/api/settings', async route => {
    if (route.request().method() !== 'PUT' || route.request().postDataJSON().language !== 'vi') return route.continue();
    const response = await route.fetch();
    firstWritePersisted();
    await firstWriteGate;
    return route.fulfill({ response });
  });
  await page.goto(`${server.origin}/#/settings`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.evaluate(() => {
    const save = ST.saveLanguage;
    ST.saveLanguage = language => save(language).finally(() => { window.languageSaveFinished = true; });
  });
  await page.getByLabel('Language', { exact: true }).selectOption('vi');
  await firstPersisted;
  assert.equal(server.data.settings.language, 'vi');

  const otherClient = runtime(server.origin);
  await otherClient.ST.load();
  await otherClient.ST.saveLanguage('en');
  assert.equal(server.data.settings.language, 'en');

  await page.evaluate(() => ST.load());
  assert.equal(await page.evaluate(() => ST.settings().language), 'en');
  releaseFirstWrite();
  await page.waitForFunction(() => window.languageSaveFinished === true);

  assert.equal(server.data.settings.language, 'en');
  assert.equal(await page.evaluate(() => ST.settings().language), 'en');
  assert.equal(await page.evaluate(() => ST.i18n.lang()), 'en');
  assert.equal(await page.locator('html').getAttribute('lang'), 'en');
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'en');
});

test('AC3 an English snapshot read before a Vietnamese save cannot revert its later server read', async t => {
  const server = await startServer();
  t.after(server.close);
  const page = await openBrowserPage(t);
  let releasePut;
  const putGate = new Promise(resolve => { releasePut = resolve; });
  let heldPut;
  const putHeld = new Promise(resolve => { heldPut = resolve; });
  let releaseNodes;
  const nodesGate = new Promise(resolve => { releaseNodes = resolve; });
  let heldNodes;
  const nodesHeld = new Promise(resolve => { heldNodes = resolve; });
  let captureSnapshotSettings = false;
  let snapshotSettingsRead;
  const snapshotSettingsReady = new Promise(resolve => { snapshotSettingsRead = resolve; });
  let holdSnapshotNodes = false;
  t.after(() => { releasePut(); releaseNodes(); });

  await page.route('**/api/settings', async route => {
    const request = route.request();
    if (request.method() === 'PUT' && request.postDataJSON().language === 'vi') {
      heldPut();
      await putGate;
      return route.continue();
    }
    if (request.method() === 'GET' && captureSnapshotSettings) {
      captureSnapshotSettings = false;
      const response = await route.fetch();
      const payload = await response.json();
      snapshotSettingsRead(payload.data.language);
      return route.fulfill({ response, body: JSON.stringify(payload) });
    }
    return route.continue();
  });
  await page.route('**/api/nodes**', async route => {
    if (!holdSnapshotNodes) return route.continue();
    heldNodes();
    await nodesGate;
    return route.continue();
  });

  await page.goto(`${server.origin}/#/settings`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  await page.getByLabel('Language', { exact: true }).selectOption('vi');
  await putHeld;

  captureSnapshotSettings = true;
  holdSnapshotNodes = true;
  await page.evaluate(() => { window.pendingSnapshotLoad = ST.load(); });
  assert.equal(await snapshotSettingsReady, 'en');
  await nodesHeld;

  releasePut();
  await page.getByText('Saved', { exact: true }).waitFor();
  assert.equal(server.data.settings.language, 'vi');
  releaseNodes();
  await page.evaluate(() => window.pendingSnapshotLoad);

  assert.equal(await page.evaluate(() => ST.settings().language), 'vi');
  assert.equal(await page.evaluate(() => ST.i18n.lang()), 'vi');
  assert.equal(await page.locator('html').getAttribute('lang'), 'vi');
  assert.equal(await page.getByLabel('Language', { exact: true }).inputValue(), 'vi');
});

test('AC2 R3 Vietnamese pages localize UTC bandwidth dates, fractional locations, and monthly counts without changing ASN identifiers', async t => {
  const server = await startServer();
  t.after(server.close);
  server.data.settings.language = 'vi';
  server.data.settings.timezone = 'Asia/Ho_Chi_Minh';
  const page = await openBrowserPage(t, { timezoneId: 'Asia/Ho_Chi_Minh' });

  await page.route('**/api/usage**', async route => {
    if (new URL(route.request().url()).pathname !== '/api/usage') return route.continue();
    const response = await route.fetch();
    const payload = await response.json();
    payload.data.totals.totalBytes = 1536;
    payload.data.series[0].start = '2026-11-17T23:30:00.000Z';
    await route.fulfill({ response, body: JSON.stringify(payload) });
  });
  await page.route('**/api/ip/addresses**', async route => {
    const response = await route.fetch();
    const payload = await response.json();
    payload.data[0].geo = { lat: 1e-7, lon: 106.9876, radiusKm: 1000.5, qualityScore: 75.5, city: 'Ho Chi Minh City',
      region: 'Ho Chi Minh', country: 'Vietnam', countryCode: 'VN', timezone: 'Asia/Ho_Chi_Minh', isp: 'Fixture ISP',
      asn: 64500, asnOrg: 'Fixture Network', source: 'fixture' };
    await route.fulfill({ response, body: JSON.stringify(payload) });
  });
  await page.route('**/api/reports/monthly**', async route => {
    const response = await route.fetch();
    const payload = await response.json();
    payload.data.rows = [{ isp: 'Fixture ISP', asn: 64500, node: 'Contract node', plan: 'Fiber', avgDownMbps: 10,
      p95DownMbps: 20, deliveredPercent: 90, worstHoursUtc: [], failures: 1234, downtimeMinutes: 5678, ipChanges: 9012 }];
    await route.fulfill({ response, body: JSON.stringify(payload) });
  });

  await page.goto(`${server.origin}/#/`);
  await page.waitForFunction(() => ST.remote.status === 'ready');
  const bandwidth = page.locator('section.card').filter({ has: page.getByRole('heading', { name: 'Bandwidth', exact: true }) });
  await bandwidth.getByRole('cell', { name: '17/11/2026 UTC', exact: true }).waitFor();
  assert.equal(await bandwidth.locator('.stat-value').first().innerText(), '1,5 KB');

  await page.goto(`${server.origin}/#/network`);
  await page.getByRole('row').filter({ hasText: '203.0.113.9' }).click();
  const location = await page.getByRole('dialog').innerText();
  assert.ok(location.includes('0,0000001, 106,9876'));
  assert.ok(location.includes('About 1.000,5 km'));
  assert.ok(location.includes('75,5 / 100'));
  assert.ok(location.includes('AS64500'));
  assert.ok(!location.includes('AS64.500'));

  await page.keyboard.press('Escape');
  await page.goto(`${server.origin}/#/isps`);
  const monthPicker = page.locator('select.report-month');
  await monthPicker.waitFor();
  const vietnameseMonths = await monthPicker.locator('option').evaluateAll(options => options.map(option => ({ value: option.value, label: option.label })));
  const viMonth = vietnameseMonths[0];
  assert.match(viMonth.value, /^\d{4}-\d{2}$/);
  assert.equal(viMonth.label, new Intl.DateTimeFormat('vi-VN', { timeZone: 'Asia/Ho_Chi_Minh', year: 'numeric', month: 'long' }).format(Date.parse(`${viMonth.value}-15T12:00:00Z`)));
  const viMonthRequest = page.waitForRequest(request => {
    const url = new URL(request.url());
    return url.pathname === '/api/reports/monthly' && url.searchParams.get('month') === viMonth.value;
  });
  await monthPicker.selectOption(viMonth.value);
  const viRequest = await viMonthRequest;
  assert.equal(new URL(viRequest.url()).searchParams.get('month'), viMonth.value);
  const report = page.locator('[data-report-preview]');
  await report.getByRole('cell', { name: '1.234', exact: true }).waitFor();
  assert.equal(await report.getByRole('cell', { name: '5.678', exact: true }).innerText(), '5.678');
  assert.equal(await report.getByRole('cell', { name: '9.012', exact: true }).innerText(), '9.012');
  assert.ok((await report.innerText()).includes('AS64500'));
  assert.ok(!(await report.innerText()).includes('AS64.500'));

  await page.goto(`${server.origin}/#/settings`);
  await page.getByLabel('Language', { exact: true }).selectOption('en');
  await page.getByText('Saved', { exact: true }).waitFor();
  await page.goto(`${server.origin}/#/isps`);
  const englishMonthPicker = page.locator('select.report-month');
  await englishMonthPicker.waitFor();
  const englishMonths = await englishMonthPicker.locator('option').evaluateAll(options => options.map(option => ({ value: option.value, label: option.label })));
  assert.ok(englishMonths.every(option => /^\d{4}-\d{2}$/.test(option.value) && option.label === option.value));
  const enMonth = englishMonths[0].value;
  const enMonthRequest = page.waitForRequest(request => {
    const url = new URL(request.url());
    return url.pathname === '/api/reports/monthly' && url.searchParams.get('month') === enMonth;
  });
  await englishMonthPicker.selectOption(enMonth);
  const enRequest = await enMonthRequest;
  assert.equal(new URL(enRequest.url()).searchParams.get('month'), enMonth);
  await page.goto(`${server.origin}/#/network`);
  await page.getByRole('row').filter({ hasText: '203.0.113.9' }).click();
  const englishLocation = await page.getByRole('dialog').innerText();
  assert.ok(englishLocation.includes('1e-7, 106.9876'));
  assert.ok(englishLocation.includes('About 1000.5 km'));
  assert.ok(!englishLocation.includes('1,000.5 km'));
});

for (const language of ['en', 'vi']) {
  test(`AC2 displayed counts preserve English and localize Vietnamese (${language})`, async t => {
    const server = await startServer();
    t.after(server.close);
    server.data.settings.language = language;
    const page = await openBrowserPage(t);
    const total = 1234;
    const expected = language === 'vi' ? '1.234' : '1234';
    const now = Date.now();
    const results = Array.from({ length: total }, (_, index) => ({ ...server.data.result, id: `result-${index}`,
      ts: new Date(now - index * 60000).toISOString(), status: 'failed', failureClass: 'rate_limited', down: null, up: null, ping: null }));
    const changes = Array.from({ length: total }, (_, index) => ({ nodeId: server.data.node.id,
      ts: new Date(now - index * 60000).toISOString(), from: `203.0.113.${index % 250 + 1}`, to: `198.51.100.${index % 250 + 1}` }));
    const events = Array.from({ length: total }, (_, index) => ({ ...server.data.event, id: `event-${index}` }));
    const audit = Array.from({ length: total }, (_, index) => ({ id: `audit-${index}`, ts: new Date(now - index * 1000).toISOString(),
      actor: 'owner@example.com', action: 'settings.update', authMethod: 'session', targetType: 'settings', targetId: '', summary: 'Changed settings', remote: '127.0.0.1' }));
    let expandedNodes = false;
    const nodes = Array.from({ length: total }, (_, index) => ({ ...server.data.node,
      id: index === 0 ? server.data.node.id : `node-${index}`, name: `Node ${index + 1}` }));

    await page.route('**/api/results**', async route => {
      if (new URL(route.request().url()).pathname !== '/api/results') return route.continue();
      await fulfillListPage(route, results);
    });
    await page.route('**/api/ip/history**', async route => {
      if (new URL(route.request().url()).pathname !== '/api/ip/history') return route.continue();
      await fulfillListPage(route, changes);
    });
    await page.route('**/api/alerts/events**', async route => {
      if (new URL(route.request().url()).pathname !== '/api/alerts/events') return route.continue();
      await fulfillListPage(route, events);
    });
    await page.route('**/api/nodes**', async route => {
      if (!expandedNodes || new URL(route.request().url()).pathname !== '/api/nodes') return route.continue();
      await fulfillListPage(route, nodes);
    });
    await page.route('**/api/audit**', async route => {
      if (new URL(route.request().url()).pathname !== '/api/audit') return route.continue();
      const response = await route.fetch();
      const payload = await response.json();
      payload.data = audit;
      payload.meta.pagination.total = total;
      await route.fulfill({ response, body: JSON.stringify(payload) });
    });
    await page.route('**/api/dashboard**', async route => {
      const response = await route.fetch();
      const payload = await response.json();
      payload.data.current.ips = total;
      await route.fulfill({ response, body: JSON.stringify(payload) });
    });
    await page.route('**/api/stats/**', async route => {
      const path = new URL(route.request().url()).pathname;
      if (!['/api/stats/summary', '/api/stats/trend', '/api/stats/heatmap', '/api/stats/distribution'].includes(path)) return route.continue();
      const response = await route.fetch();
      const payload = await response.json();
      if (path === '/api/stats/summary') payload.data.forEach(item => { item.current.count = total; });
      if (path === '/api/stats/trend') payload.data.forEach(item => { item.points = [{ ts: new Date(now).toISOString(), value: 90, count: total }]; });
      if (path === '/api/stats/heatmap') {
        payload.data.cells.forEach(cell => { cell.value = null; cell.count = 0; });
        payload.data.cells[0].value = 90;
        payload.data.cells[0].count = total;
      }
      if (path === '/api/stats/distribution') payload.data.forEach(item => {
        item.count = total;
        item.bins = [{ from: 90, to: 91, count: total, percent: 100 }];
      });
      await route.fulfill({ response, body: JSON.stringify(payload) });
    });

    await page.goto(`${server.origin}/#/`);
    await page.waitForFunction(() => ST.remote.status === 'ready');
    const dashboardCount = page.locator('.stat').filter({ hasText: 'IP changes' }).locator('.stat-value');
    await dashboardCount.waitFor();
    assert.equal(await dashboardCount.locator('span').first().innerText(), expected);

    await page.goto(`${server.origin}/#/nodes/node-contract`);
    const nodeSummary = `${expected} failed; ${expected} rate-limited excluded of ${expected}`;
    await page.getByText(nodeSummary, { exact: true }).waitFor();

    await page.goto(`${server.origin}/#/history`);
    const history = page.locator('.pager');
    await page.getByText(language === 'vi' ? '1.234 results' : '1,234 results', { exact: true }).waitFor();
    await page.getByLabel('Rows per page').selectOption('100');
    for (let index = 0; index < 11; index++) await page.getByTitle('Next page').click();
    const pageRange = language === 'vi' ? '1.101–1.200 of 1.234' : '1101–1200 of 1,234';
    await history.getByText(pageRange, { exact: true }).waitFor();

    await page.goto(`${server.origin}/#/stats`);
    await page.getByText(`Contract node: ${expected} samples`, { exact: false }).waitFor();
    const distribution = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'Distribution', exact: true }) });
    await distribution.getByText(`${expected} tests`, { exact: true }).waitFor();
    await distribution.locator('svg[aria-label="Distribution for Contract node"] rect.hit').first().hover();
    await distribution.locator('.tooltip.show').getByText(`${expected} (100%)`, { exact: true }).waitFor();
    const heatmap = page.locator('.card').filter({ has: page.getByRole('heading', { name: 'Week at a glance', exact: true }) });
    const heatCell = heatmap.getByRole('gridcell').first();
    await heatCell.focus();
    await heatCell.getAttribute('aria-label').then(label => assert.ok(label.includes(`${expected} tests`)));
    await heatmap.locator('.tooltip.show').getByText(expected, { exact: true }).waitFor();

    await page.goto(`${server.origin}/#/settings`);
    await page.getByRole('tab', { name: 'Activity log' }).click();
    await page.getByText(`${expected} changes shown.`, { exact: true }).waitFor();

    await page.goto(`${server.origin}/#/alerts`);
    await page.locator('.nav a[href="#/alerts"] .badge').getByText(expected, { exact: true }).waitFor();
    await page.getByRole('tab', { name: new RegExp(`Activity\\s*${expected.replace('.', '\\.')}`) }).waitFor();
    expandedNodes = true;
    await page.evaluate(() => ST.load());
    await page.waitForFunction(() => ST.remote.status === 'ready' && ST.nodes().length === 1234);
    const activeNodeCount = page.locator('.sidebar-actions .chip[title="Nodes reporting"]');
    await activeNodeCount.getByText(`${expected} of ${expected} nodes active`, { exact: true }).waitFor();
  });
}

test('AC2 R4 delta percentages preserve English output and use Vietnamese separators without changing rounding', async t => {
  const server = await startServer();
  t.after(server.close);
  const page = await openBrowserPage(t);
  await page.goto(`${server.origin}/?demo=1#/settings`);
  const percentage = (language, current, previous) => page.evaluate(({ language, current, previous }) => {
    ST.i18n.setLanguage(language);
    return ST.ui.deltaTag(current, previous, true).textContent;
  }, { language, current, previous });

  assert.equal(await percentage('en', 10001, 1), '▲ 1000000.0%');
  assert.equal(await percentage('vi', 10001, 1), '▲ 1.000.000,0%');
  assert.equal(await percentage('en', 1011.5, 1000), '▲ 1.1%');
  assert.equal(await percentage('vi', 1011.5, 1000), '▲ 1,1%');
});
