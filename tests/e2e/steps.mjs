// Reusable steps: scenarios are built from these so a change to the UI is fixed in one place.
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';

export function steps({ page, stack, startAgent, query, problems }) {
  const url = hash => `${stack.origin}/${hash}`;
  const EMAIL = 'jooservices@gmail.com', PASSWORD = 'password';
  const heading = name => page.getByRole('heading', { name, level: 1 });
  const toast = text => page.locator('#toasts').getByText(text).first();

  const S = {
    async open(hash, title) {
      await page.goto(url(hash));
      await heading(title).waitFor({ timeout: 15000 });
    },
    async signIn(email = EMAIL, password = PASSWORD) {
      await page.goto(url('#/signin'));
      await page.getByLabel('Email').fill(email);
      await page.getByLabel('Password').fill(password);
      await page.getByRole('button', { name: 'Sign in' }).click();
    },
    async expectNoLoadError() {
      assert.equal(await page.getByText('Could not load data').count(), 0, 'the page reports a load error');
      assert.equal(await page.getByText('Something went wrong').count(), 0, 'the page failed to draw');
    },

    async signInRules() {
      await page.goto(url('#/signin'));
      await heading('Sign in').waitFor();
      assert.equal(await page.locator('.sidebar').isVisible(), false, 'the menu must be hidden before sign-in');
      await page.getByRole('button', { name: 'Sign in' }).click();
      await page.getByText('Enter your email and password.').waitFor();
      await S.signIn(EMAIL, 'not the password');
      await page.getByText('The email or password is not right').waitFor();
      // The browser logs the refused request itself; that is the expected outcome of a wrong password, not a defect.
      for (let i = problems.length - 1; i >= 0; i--) if (/status of 401/.test(problems[i])) problems.splice(i, 1);
      assert.equal(await page.getByLabel('Password').inputValue(), '', 'a failed attempt clears the password');
      await S.signIn();
      await heading('Dashboard').waitFor({ timeout: 15000 });
      await page.getByRole('link', { name: 'Nodes' }).first().waitFor();
    },

    async dashboardShowsFleet() {
      await S.open('#/', 'Dashboard');
      await page.getByText('5 of 5 nodes active').waitFor();
      for (const label of ['Download vs. expected', 'Upload vs. expected', 'Median latency', 'Tests succeeded', 'IP changes']) await page.getByText(label).first().waitFor();
      for (const name of ['VM1', 'VM2', 'VM3', 'VM4', 'VM5']) await page.locator('.node-card').filter({ hasText: name }).first().waitFor();
      const changes = await page.locator('.stat').filter({ hasText: 'IP changes' }).locator('.stat-value').innerText();
      assert.match(changes, /^[1-9]\d*\s*change/, 'the seeded address changes must be counted, got ' + JSON.stringify(changes));
      await S.expectNoLoadError();
    },

    async allPagesDraw() {
      for (const [hash, title] of [['#/nodes', 'Nodes'], ['#/history', 'Results'], ['#/stats', 'Statistics'], ['#/network', 'IP & location'], ['#/schedules', 'Schedules'], ['#/alerts', 'Alerts'], ['#/settings', 'Settings']]) {
        await S.open(hash, title);
        await page.waitForLoadState('networkidle');
        await S.expectNoLoadError();
      }
      await S.open('#/nodes', 'Nodes');
      await page.getByRole('row').filter({ hasText: 'VM1' }).first().click();
      await page.getByRole('heading', { name: /VM1/, level: 1 }).waitFor();
      for (const tab of ['Settings', 'Agent', 'Overview']) { await page.getByRole('tab', { name: tab }).click(); await S.expectNoLoadError(); }
    },

    // Every page, in both themes, at phone, tablet and desktop width: nothing may overflow the viewport or leave a console error.
    async layoutHolds() {
      const routes = [['#/', 'Dashboard'], ['#/nodes', 'Nodes'], ['#/history', 'Results'], ['#/stats', 'Statistics'], ['#/network', 'IP & location'], ['#/schedules', 'Schedules'], ['#/alerts', 'Alerts'], ['#/settings', 'Settings']];
      const original = page.viewportSize();
      for (const theme of ['dark', 'light']) {
        await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);
        for (const width of [390, 768, 1440]) {
          await page.setViewportSize({ width, height: 900 });
          for (const [hash, title] of routes) {
            await S.open(hash, title);
            await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);
            await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
            const overflow = await page.evaluate(() => {
              const wide = [];
              for (const el of document.querySelectorAll('.content *')) {
                const box = el.getBoundingClientRect();
                if (box.width && box.right > document.documentElement.clientWidth + 1 && !el.closest('.table-wrap, .tabs, .seg, pre, .legend, .leaflet-container')) wide.push(el.tagName + '.' + el.className);
              }
              return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, wide: wide.slice(0, 3) };
            });
            assert.ok(overflow.page <= 1, `${title} overflows the page by ${overflow.page}px at ${width}px (${theme}): ${overflow.wide.join(', ')}`);
            assert.deepEqual(overflow.wide, [], `${title} has elements outside the viewport at ${width}px (${theme})`);
          }
        }
      }
      await page.setViewportSize(original);
      await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
    },

    async resultsFilterAndExport() {
      await S.open('#/history', 'Results');
      await page.getByText('1,680 results').waitFor();
      const statusSelect = page.locator('select', { has: page.locator('option', { hasText: 'Any status' }) });
      await statusSelect.selectOption('failed');
      const failed = await page.locator('text=/\\d[\\d,]* results?/').first().innerText();
      assert.ok(/^(\d+) results?$/.test(failed.replace(/,/g, '')) && parseInt(failed) < 1680, `status filter: ${failed}`);
      await statusSelect.selectOption('all');
      await page.getByLabel('Search results').fill('VM3');
      await page.getByText('336 results').waitFor();
      await page.getByLabel('Minimum download').fill('100000');
      await page.getByText('0 results').waitFor();
      await page.getByLabel('Minimum download').fill('');
      await page.getByLabel('Search results').fill('');
      await page.getByRole('row').nth(1).click();
      await page.getByRole('dialog').or(page.locator('.drawer')).first().waitFor();
      await page.keyboard.press('Escape');
      const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Export CSV' }).click()]);
      const text = readFileSync(await download.path(), 'utf8');
      assert.ok(text.startsWith('time,node,status,download_mbps'), 'CSV header');
      assert.ok(text.split('\n').length > 1000, 'the export holds the rows');
    },

    async addNodeAndRunTest() {
      await S.open('#/nodes', 'Nodes');
      await page.getByRole('button', { name: 'Add a node' }).first().click();
      const dialog = page.getByRole('dialog', { name: 'Add a node' });
      await dialog.getByLabel('Node name').fill('E2E-1');
      await dialog.getByLabel('What it measures').fill('End to end test line');
      await dialog.getByLabel('Expected download').fill('100');
      await dialog.getByLabel('Expected upload').fill('50');
      await dialog.getByRole('button', { name: 'Continue' }).click();
      await dialog.getByRole('button', { name: /I will run a command/ }).click();
      const command = await dialog.locator('pre, code, .code').first().innerText();
      const token = /--token\s+(\S+)/.exec(command)?.[1];
      assert.ok(token && token.length > 20, 'the wizard shows a real one-time enrolment key: ' + command.slice(0, 80));
      assert.match(command, /install\.sh/);
      await dialog.getByRole('button', { name: 'I ran the command' }).click();
      await startAgent(stack, { name: 'E2E-1', token });
      await dialog.getByText('E2E-1 is online').waitFor({ timeout: 90000 });
      await dialog.getByText(/First result: .* down, .* up, .* ping\./).waitFor({ timeout: 90000 });
      await dialog.getByRole('button', { name: 'Open node' }).click();
      await page.getByRole('heading', { name: /E2E-1/, level: 1 }).waitFor();

      await page.getByRole('button', { name: 'Run test' }).click();
      const run = page.getByRole('dialog', { name: 'Running speed test' });
      await run.waitFor();
      await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
      await run.locator('.modal-foot').getByRole('button', { name: 'Close' }).click();
      const stored = query(stack.db, "select count(*) from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.manual");
      assert.ok(Number(stored) >= 1, 'the manual result is stored on the center: ' + stored);
      await S.open('#/history', 'Results');
      await page.getByLabel('Search results').fill('E2E-1');
      await page.getByRole('row').nth(1).waitFor();

      // The same line cannot be enrolled twice with the same one-time key.
      const again = await page.request.post(stack.origin + '/api/agent/enroll', { data: { token, name: 'E2E-copy', agent: 'x' } });
      assert.ok(again.status() >= 400, 'an enrolment key works once, got ' + again.status());
    },

    async schedulesSave() {
      await S.open('#/schedules', 'Schedules');
      const row = page.getByRole('row').filter({ hasText: 'VM1' }).first();
      await row.locator('select').selectOption('60');
      await toast('hour').waitFor();
      await page.reload();
      await heading('Schedules').waitFor();
      assert.equal(await page.getByRole('row').filter({ hasText: 'VM1' }).first().locator('select').inputValue(), '60', 'the interval is stored on the center');
      assert.equal(query(stack.db, "select interval_minutes from schedules s join nodes n on n.id=s.node_id where n.name='VM1'"), '60');
      await page.getByRole('button', { name: 'Spread out evenly' }).click();
      await toast('Start times spread out').waitFor();
      const offsets = query(stack.db, "select count(distinct offset_minutes) from schedules");
      assert.ok(Number(offsets) > 1, 'staggering gives different start minutes');
      // The spread-out request reloads the data and redraws the page; type only after that has settled.
      await page.waitForLoadState('networkidle');
      await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
      await page.getByLabel('Pause from').fill('23:00');
      await page.getByLabel('Pause from').blur();
      await page.getByLabel('Pause until').fill('05:00');
      await page.getByLabel('Pause until').blur();
      await toast('Saved').waitFor();
      assert.equal(query(stack.db, "select value #>> '{}' from settings where key='quietStart'"), '23:00');
    },

    async alertRules() {
      await S.open('#/alerts', 'Alerts');
      await page.getByRole('tab', { name: /Rules/ }).click();
      await page.getByRole('button', { name: 'New rule' }).click();
      const dialog = page.getByRole('dialog', { name: 'New alert rule' });
      await dialog.getByPlaceholder('e.g. Evening slowdown').fill('E2E slow evening');
      await dialog.getByRole('button', { name: 'Create rule' }).click();
      await toast('Rule created').waitFor();
      await page.getByText('E2E slow evening').waitFor();
      assert.equal(query(stack.db, "select count(*) from alert_rules where name='E2E slow evening'"), '1');
      await page.locator('.list-row').filter({ hasText: 'E2E slow evening' }).getByRole('button', { name: 'Edit' }).click();
      const edit = page.getByRole('dialog', { name: 'Edit alert rule' });
      await edit.getByPlaceholder('e.g. Evening slowdown').fill('E2E renamed rule');
      await edit.getByRole('button', { name: 'Save rule' }).click();
      await toast('Rule saved').waitFor();
      await page.getByText('E2E renamed rule').waitFor();
      await page.locator('.list-row').filter({ hasText: 'E2E renamed rule' }).locator('.switch').click();
      await toast('turned off').waitFor();
      assert.equal(query(stack.db, "select enabled from alert_rules where name='E2E renamed rule'"), 'f');
      await page.locator('.list-row').filter({ hasText: 'E2E renamed rule' }).getByRole('button', { name: /Delete/ }).click();
      await page.getByRole('dialog').getByRole('button', { name: 'Delete rule' }).click();
      await toast('Rule deleted').waitFor();
      assert.equal(query(stack.db, "select count(*) from alert_rules where name like 'E2E%'"), '0');
      // Rule without a name is refused in plain language, nothing is stored.
      await page.getByRole('button', { name: 'New rule' }).click();
      await page.getByRole('dialog').getByRole('button', { name: 'Create rule' }).click();
      await page.getByText('Give the rule a name.').waitFor();
      await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click();

      await page.getByRole('tab', { name: /Activity/ }).click();
      const before = Number(query(stack.db, 'select count(*) from alert_events where not acked'));
      assert.ok(before >= 2);
      await page.getByRole('button', { name: 'Acknowledge', exact: true }).first().click();
      await toast('Alert acknowledged').waitFor();
      assert.equal(Number(query(stack.db, 'select count(*) from alert_events where not acked')), before - 1);
      await page.getByRole('button', { name: 'Acknowledge all' }).click();
      await toast('All alerts acknowledged').waitFor();
      assert.equal(query(stack.db, 'select count(*) from alert_events where not acked'), '0');
    },

    async settingsSave() {
      await S.open('#/settings', 'Settings');
      await page.locator('.setting').filter({ hasText: 'Decimal places' }).locator('select').selectOption('2');
      await toast('Saved').waitFor();
      assert.equal(query(stack.db, "select value->>'decimals' from settings where key='display'"), '2');
      await page.getByRole('tab', { name: 'Notifications' }).click();
      await page.getByLabel('URL for Webhook').fill('https://hooks.example.com/e2e-secret-path');
      await page.getByLabel('Address for Email').fill('ops@example.com');
      await page.getByLabel('Enable Email').check({ force: true });
      await toast('Saved').waitFor();
      await page.waitForLoadState('networkidle'); // the earlier 'Saved' toast may still be showing; wait for every save to finish
      const settings = await (await page.request.get(stack.origin + '/api/settings')).text();
      assert.ok(!settings.includes('e2e-secret-path'), 'secrets are never returned');
      assert.ok(settings.includes('"configured":true'), 'the read model says a secret is configured');
      assert.ok(Number(query(stack.db, 'select count(*) from settings where secret_ciphertext is not null')) >= 1, 'the secret is stored encrypted');
      await page.getByRole('tab', { name: 'Access' }).click();
      await page.getByRole('button', { name: 'Generate a new key' }).click();
      await page.getByRole('dialog').getByRole('button', { name: 'Generate' }).click();
      const key = await page.locator('pre, code, .code').first().innerText();
      assert.ok(key.length > 20, 'the new key is shown');
      await page.getByText('It will not be shown again.').waitFor();
      await page.reload();
      await page.getByRole('tab', { name: 'Access' }).click();
      assert.ok(!(await page.content()).includes(key.trim()), 'the key is not shown again after a reload');
    },

    // Statistics: every section draws from the loaded results and the metric switch changes the table.
    async statisticsPage() {
      await S.open('#/stats', 'Statistics');
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      await page.getByRole('columnheader', { name: 'Average' }).waitFor();
      const before = await page.locator('table').first().innerText();
      await page.getByRole('tab', { name: 'Upload' }).first().click();
      await page.getByRole('heading', { name: 'Summary by node' }).waitFor();
      assert.notEqual(await page.locator('table').first().innerText(), before, 'switching to upload changes the figures');
      for (const section of ['Trend', 'Time of day', 'Week at a glance', 'Distribution']) await page.getByRole('heading', { name: section }).waitFor();
      await S.expectNoLoadError();
    },

    // IP & location: the recorded addresses are listed and the lookup asks the center.
    async ipPage() {
      await S.open('#/network', 'IP & location');
      await page.getByRole('heading', { name: 'Every address seen' }).waitFor();
      // The live map shows one marker per location; hovering it names the nodes there.
      await page.locator('.leaflet-container').waitFor();
      const markers = page.locator('.leaflet-marker-icon');
      await markers.first().waitFor();
      assert.ok(await markers.count() >= 1, 'the map shows the node locations');
      await markers.first().hover();
      await page.locator('.leaflet-tooltip').first().waitFor();
      await page.getByRole('link', { name: 'OpenStreetMap' }).waitFor();
      await page.getByRole('cell', { name: '203.0.113.11' }).first().waitFor();
      await page.getByLabel('IP address to look up').fill('not an address');
      await page.getByRole('button', { name: 'Look up' }).click();
      await page.getByText('Enter a valid IPv4 address').waitFor();
      await page.getByLabel('IP address to look up').fill('203.0.113.11');
      await page.getByRole('button', { name: 'Look up' }).click();
      await page.getByText('Hanoi').first().waitFor();
      await page.getByRole('cell', { name: '203.0.113.11' }).first().click();
      await page.getByRole('heading', { name: 'IP address details' }).waitFor();
    },

    // The top-bar button flips the theme and the choice survives a reload (stored on the center).
    async themeSwitch() {
      await S.open('#/', 'Dashboard');
      const theme = () => page.evaluate(() => document.documentElement.getAttribute('data-theme'));
      const start = await theme();
      await page.getByRole('button', { name: 'Toggle theme' }).click();
      await page.waitForFunction(t => document.documentElement.getAttribute('data-theme') !== t, start);
      const flipped = await theme();
      assert.notEqual(flipped, start);
      await page.waitForLoadState('networkidle');
      await page.reload();
      await heading('Dashboard').waitFor();
      assert.equal(await theme(), flipped, 'the theme survives a reload');
      await page.getByRole('button', { name: 'Toggle theme' }).click();
      await page.waitForFunction(t => document.documentElement.getAttribute('data-theme') !== t, flipped);
      await page.waitForLoadState('networkidle');
    },

    // The center sends a test message through the channel and the owner sees the outcome.
    async notificationTest(receiver) {
      await S.open('#/settings', 'Settings');
      await page.getByRole('tab', { name: 'Notifications' }).click();
      await page.getByLabel('URL for Webhook').fill(receiver.url);
      await page.getByLabel('URL for Webhook').blur();
      await toast('Saved').waitFor();
      await page.getByLabel('Enable Webhook').check({ force: true });
      await page.waitForLoadState('networkidle');
      await page.locator('.setting').filter({ hasText: 'Webhook' }).getByRole('button', { name: 'Send a test' }).click();
      await toast('Test message sent to Webhook').waitFor({ timeout: 30000 });
      assert.ok(receiver.bodies.length >= 1, 'the receiver got the message');
      assert.ok(!JSON.stringify(receiver.bodies[0]).includes('secret'), 'no secret in the message');
      // A channel that is not set up is refused in plain words.
      await page.locator('.setting').filter({ hasText: 'Telegram' }).getByRole('button', { name: 'Send a test' }).click();
      await toast('Telegram is not set up yet').waitFor();
      // The browser logs the refused request itself; refusing an unconfigured channel is the expected outcome.
      for (let i = problems.length - 1; i >= 0; i--) if (/status of 422/.test(problems[i])) problems.splice(i, 1);
    },

    async signOut() {
      await page.getByRole('button', { name: 'Sign out' }).click();
      await heading('Sign in').waitFor();
      await page.goto(url('#/nodes'));
      await heading('Sign in').waitFor({ timeout: 15000 });
      const me = await (await page.request.get(stack.origin + '/api/nodes')).status();
      assert.equal(me, 401, 'the API refuses a signed-out visitor');
    }
  };
  return S;
}
