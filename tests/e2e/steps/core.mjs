// Steps: the smallest user actions and checks. Everything above (suites, scenarios) is built from these, so a UI
// change is fixed in one place.
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';

export const OWNER = { email: 'jooservices@gmail.com', password: 'password' };

export function coreSteps(ctx) {
  const { page, stack, query, problems } = ctx;
  const T = {
    page,
    assert,
    url: hash => `${stack.browserOrigin}/${hash}`,
    heading: name => page.getByRole('heading', { name, level: 1 }),
    bandwidthCard: () => page.locator('section.card').filter({ has: page.getByRole('heading', { name: 'Bandwidth', exact: true }) }),
    bandwidthBucketDate: () => T.bandwidthCard().locator('table').last().locator('tbody tr').first().locator('td').first().innerText(),
    bandwidthRangeNumber: () => T.bandwidthCard().locator('.stat-value').first().innerText(),
    toast: text => page.locator('#toasts').getByText(text).first(),
    languageSelector: () => page.getByLabel('Language', { exact: true }),
    savedLanguage: () => query(stack.db, "select value->>'language' from settings where key='display'"),
    async chooseLanguage(language) { await T.choose(T.languageSelector(), language); },
    async reloadSettings() { await page.reload(); await T.heading('Settings').waitFor(); },
    async documentLanguage() { return page.locator('html').getAttribute('lang'); },
    async failNextLanguageSave(language) {
      let refused = false;
      await page.route('**/api/settings', async route => {
        const request = route.request();
        if (!refused && request.method() === 'PUT' && request.postDataJSON().language === language) {
          refused = true;
          await route.fulfill({ status: 500, contentType: 'application/problem+json', body: JSON.stringify({
            type: 'about:blank', title: 'Temporary failure', status: 500, detail: 'Retry the settings save.'
          }) });
          return;
        }
        await route.continue();
      });
    },
    async allowLanguageSaves() { await page.unroute('**/api/settings'); },

    // ---- navigation and session ----
    async open(hash, title) {
      await page.goto(T.url(hash));
      await T.heading(title).waitFor({ timeout: 45000 }); // a loaded machine can take far longer than 15 s to draw a page
    },
    async signIn(email = OWNER.email, password = OWNER.password) {
      await page.goto(T.url('#/signin'));
      await page.getByLabel('Email').fill(email);
      await page.getByLabel('Password').fill(password);
      await page.getByRole('button', { name: 'Sign in' }).click();
    },
    async signOut() { await page.getByRole('button', { name: 'Sign out' }).click(); await T.heading('Sign in').waitFor(); },

    // ---- waiting ----
    // Every request has finished and the page has redrawn twice: use before typing into a page that just reloaded data.
    async settled() {
      await page.waitForLoadState('networkidle');
      await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
    },

    // ---- actions ----
    async press(name, scope = page, options = {}) { await scope.getByRole('button', { name, ...options }).first().click(); },
    async tab(name) { await page.getByRole('tab', { name }).first().click(); },
    async fill(label, value, scope = page, exact = false) { await scope.getByLabel(label, { exact }).fill(value); },
    async fillAndLeave(label, value, scope = page) { const f = scope.getByLabel(label); await f.fill(value); await f.blur(); },
    async choose(select, value) { await select.selectOption(value); },
    selectWith: option => page.locator('select', { has: page.locator('option', { hasText: option }) }),
    row: text => page.getByRole('row').filter({ hasText: text }).first(),
    summaryRow: text => page.locator('.card').filter({ hasText: 'Summary by node' }).getByRole('row').filter({ hasText: text }).first(),
    dialog: name => page.getByRole('dialog', { name }),
    async dismiss() { await page.keyboard.press('Escape'); },
    async see(text, scope = page, timeout) { await scope.getByText(text).first().waitFor(timeout ? { timeout } : undefined); },
    async download(trigger) {
      const [file] = await Promise.all([page.waitForEvent('download'), trigger()]);
      const directory = mkdtempSync(join(process.cwd(), '.tmp', 'e2e-download-'));
      const path = join(directory, file.suggestedFilename());
      try { await file.saveAs(path); return readFileSync(path, 'utf8'); }
      finally { rmSync(directory, { recursive: true, force: true }); }
    },
    async downloadBytes(trigger) {
      const [file] = await Promise.all([page.waitForEvent('download'), trigger()]);
      const directory = mkdtempSync(join(process.cwd(), '.tmp', 'e2e-download-'));
      const path = join(directory, file.suggestedFilename());
      try { await file.saveAs(path); return readFileSync(path); }
      finally { rmSync(directory, { recursive: true, force: true }); }
    },

    // Polls a condition (a stored value, a file) for a few seconds: for results that are saved a moment after the click.
    async eventually(check, message, timeout = 8000) {
      const end = Date.now() + timeout; let last;
      while (Date.now() < end) { last = await check(); if (last === true) return; await page.evaluate(() => new Promise(r => setTimeout(r, 150))); }
      throw new Error(`${message} (last value: ${JSON.stringify(last)})`);
    },

    // ---- checks ----
    async expectNoLoadError() {
      assert.equal(await page.getByText('Could not load data').count(), 0, 'the page reports a load error');
      assert.equal(await page.getByText('Something went wrong').count(), 0, 'the page failed to draw');
    },
    async expectCount(locator, expected, message) { assert.equal(await locator.count(), expected, message); },

    // ---- the center, for checking what was really stored ----
    db: sql => query(stack.db, sql),
    async api(path) { const res = await page.request.get(stack.browserOrigin + path); return { status: res.status(), body: res.status() < 300 && /json/.test(res.headers()['content-type'] || '') ? await res.json() : null, text: await res.text() }; },

    // The browser logs a refused request itself; where refusing is the expected outcome, drop that log line.
    expectRefusal(status) { for (let i = problems.length - 1; i >= 0; i--) if (new RegExp(`status of ${status}`).test(problems[i])) problems.splice(i, 1); },
  };
  return T;
}
