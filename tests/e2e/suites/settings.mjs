import { createHash } from 'node:crypto';
import { request as apiRequest } from 'playwright';
import { join } from 'node:path';
import { deliveryHistorySteps } from '../steps/delivery-history.mjs';

function localizedBytes(value) {
  const units = ['B', 'KB', 'MB', 'GB'];
  let amount = Number(value), unit = 0;
  while (amount >= 1024 && unit < units.length - 1) { amount /= 1024; unit++; }
  const digits = unit ? 1 : 0;
  return new Intl.NumberFormat('vi-VN', { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(amount) + ' ' + units[unit];
}

async function checkLocationSettingsLayout(page, assert) {
	const originalTheme = await page.evaluate(() => document.documentElement.getAttribute('data-theme') || 'dark');
	const originalViewport = page.viewportSize();
	if (process.env.E2E_SHOTS) {
		await page.waitForFunction(() => document.querySelectorAll('.toast').length === 0, null, { timeout: 10000 });
	}
	try {
		for (const theme of ['dark', 'light']) {
			for (const width of [390, 768, 1440]) {
				await page.setViewportSize({ width, height: 900 });
				await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);
				await page.waitForTimeout(220);
				await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
				const overflow = await page.evaluate(() => {
					const sidebar = document.getElementById('sidebar').getBoundingClientRect();
					const sidebarStyle = getComputedStyle(document.getElementById('sidebar'));
					const wide = [...document.querySelectorAll('.content *')].filter(el => {
						const box = el.getBoundingClientRect();
						return box.width && box.right > document.documentElement.clientWidth + 1 && !el.closest('.tabs, .seg, pre');
					}).slice(0, 3).map(el => el.tagName + '.' + el.className);
					return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, sidebarRight: sidebar.right,
						sidebarClass: document.getElementById('sidebar').className, transform: sidebarStyle.transform, innerWidth, clientWidth: document.documentElement.clientWidth,
						mobile: matchMedia('(max-width: 900px)').matches, wide };
				});
				assert.ok(overflow.page <= 1, `Location settings overflow by ${overflow.page}px at ${width}px (${theme})`);
				assert.ok(width > 900 || overflow.sidebarRight <= 1, `Location settings menu covers the page at ${width}px (${theme}): ${JSON.stringify(overflow)}`);
				assert.deepEqual(overflow.wide, [], `Location settings elements overflow at ${width}px (${theme}): ${overflow.wide.join(', ')}`);
				if (process.env.E2E_SHOTS) {
					await page.screenshot({ path: join(process.env.E2E_SHOTS, `${theme}-${width}-settings-location.png`), fullPage: true });
				}
			}
		}
	} finally {
		await page.setViewportSize(originalViewport);
		await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), originalTheme);
	}
}

export function settingsSuite(T, ctx) {
  const { page, assert } = T;
  const { receiver } = ctx;
  const deliveryHistory = deliveryHistorySteps(T);
  const setting = title => page.locator('.setting').filter({ hasText: title });
  async function startBearerInstallationStream(raw, tokenID) {
    const jobID = '10000000-0000-4000-8000-000000000087';
    const eventID = '10000000-0000-4000-8000-000000000088';
    const nodeID = T.db("select id::text from nodes where name='VM5'");
    assert.ok(nodeID, 'the installation stream fixture has a seeded node');
    T.db(`insert into installer_jobs(id,node_id,principal,status,step) values ('${jobID}','${nodeID}','e2e-owner','running','connecting')`);
    T.db(`insert into installer_events(id,installation_id,step,message,pct,status) values ('${eventID}','${jobID}','connecting','Waiting for the confirmed SSH connection.',10,'running')`);
    const controller = new AbortController();
    let reader;
    try {
      const response = await fetch(`${ctx.stack.origin}/api/installations/${jobID}/events`, { headers: { Authorization: `Bearer ${raw}` }, signal: controller.signal });
      assert.equal(response.status, 200, 'a bearer-only request opens the installation event stream');
      reader = response.body.getReader();
      const first = await reader.read();
      assert.equal(first.done, false, 'the stream sends the current installation event');
      assert.match(new TextDecoder().decode(first.value), /event: progress/);
      const lastUsed = T.db(`select last_used_at::text from api_access_tokens where id='${tokenID}'`);
      let closed = false;
      const watch = (async () => {
        try {
          while (true) {
            if ((await reader.read()).done) { closed = true; return; }
          }
        } catch { closed = true; }
      })();
      void watch;
      await T.eventually(() => T.db(`select last_used_at::text from api_access_tokens where id='${tokenID}'`) !== lastUsed, 'the open stream revalidates its bearer token', 15000);
      assert.equal(closed, false, 'the stream remains open after successful bearer revalidation');
      return {
        async waitForRevocation() { await T.eventually(() => closed, 'the stream closes after token revocation', 15000); },
        async cleanup() {
          controller.abort();
          if (reader) await reader.cancel().catch(() => {});
          T.db(`delete from installer_jobs where id='${jobID}'`);
        },
      };
    } catch (error) {
      controller.abort();
      if (reader) await reader.cancel().catch(() => {});
      T.db(`delete from installer_jobs where id='${jobID}'`);
      throw error;
    }
  }
  return {
    // Language: the saved preference survives reload and a failed update restores it in the active locale.
    async languagePreference() {
      await T.open('#/settings', 'Settings');
      await T.chooseLanguage('vi');
      await T.toast('Saved').waitFor();
      await T.eventually(() => T.savedLanguage() === 'vi', 'Vietnamese language is stored');
      await T.reloadSettings();
      assert.equal(await T.languageSelector().inputValue(), 'vi', 'the selected language survives reload');
      assert.equal(await T.documentLanguage(), 'vi', 'the document language follows the saved setting');

      await T.open('#/', 'Dashboard');
      const usagePayload = page.waitForResponse(response => new URL(response.url()).pathname === '/api/usage').then(response => response.json());
      await page.reload();
      await T.heading('Dashboard').waitFor();
      assert.equal(await T.documentLanguage(), 'vi', 'the document language is restored after a dashboard reload');
      const usage = (await usagePayload).data;
      const latestBucket = Math.max(...usage.series.map(item => Date.parse(item.start)));
      assert.ok(Number.isFinite(latestBucket), 'the seeded bandwidth series includes a UTC bucket');
      const expectedDate = new Intl.DateTimeFormat('vi-VN', { timeZone: 'UTC', year: 'numeric', month: '2-digit', day: '2-digit' }).format(latestBucket) + ' UTC';
      await T.eventually(async () => await T.bandwidthBucketDate() === expectedDate, 'the latest bandwidth bucket is displayed in vi-VN UTC format');
      const totalBytes = Number(usage.totals.totalBytes);
      assert.ok(totalBytes >= 1024, 'the seeded bandwidth total shows a localized fractional unit');
      const expectedNumber = localizedBytes(totalBytes);
      assert.ok(expectedNumber.includes(','), 'the displayed bandwidth number has a Vietnamese decimal separator');
      await T.eventually(async () => await T.bandwidthRangeNumber() === expectedNumber, 'the bandwidth total is displayed in vi-VN number format');
      await T.open('#/settings', 'Settings');

      await T.failNextLanguageSave('en');
      try {
        await T.chooseLanguage('en');
        await T.toast('Không thể lưu ngôn ngữ').waitFor();
        T.expectRefusal(500);
        assert.equal(await T.languageSelector().inputValue(), 'vi', 'a failed save restores the previous language');
        assert.equal(T.savedLanguage(), 'vi', 'a failed save leaves the stored language unchanged');
      } finally {
        await T.allowLanguageSaves();
      }

      await T.chooseLanguage('en');
      await T.toast('Saved').waitFor();
      await T.eventually(() => T.savedLanguage() === 'en', 'English is restored for later scenarios');
      assert.equal(await T.documentLanguage(), 'en');
    },

    // General: every preference is saved on the center and survives a reload.
    async preferences() {
      await T.open('#/settings', 'Settings');
      await T.choose(setting('Decimal places').locator('select'), '2');
      await T.toast('Saved').waitFor();
      await T.eventually(() => T.db("select value->>'decimals' from settings where key='display'") === '2', 'the decimal preference is stored');
      assert.equal(T.db("select value->>'decimals' from settings where key='display'"), '2');
      await T.settled();
      await T.choose(setting('Speed units').locator('select'), 'Mbps');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.units === 'Mbps', 'speed units are stored');
      await T.choose(setting('Time zone').locator('select'), 'UTC');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.timezone === 'UTC', 'time zone is stored');
      const clock = setting('12-hour clock').locator('input[type=checkbox]');
      if (!(await clock.isChecked())) await setting('12-hour clock').locator('.switch').click();
      await T.eventually(async () => (await T.api('/api/settings')).body.data.hour12 === true, '12-hour clock is enabled');
      await T.choose(setting('Default time range').locator('select'), '24h');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.range === '24h', 'default time range is stored');
      await page.getByRole('radio', { name: 'Light' }).or(page.getByRole('tab', { name: 'Light' })).first().click();
      await T.eventually(async () => (await T.api('/api/settings')).body.data.theme === 'light', 'light theme is stored');
      assert.equal((await T.api('/api/settings')).body.data.hour12, true);
      await page.reload();
      await T.heading('Settings').waitFor();
      assert.equal(await setting('Decimal places').locator('select').inputValue(), '2', 'the preference survives a reload');
      assert.equal(await setting('Time zone').locator('select').inputValue(), 'UTC');
      // Put the defaults back so later scenarios see the usual numbers.
      await T.choose(setting('Decimal places').locator('select'), '1'); await T.settled();
      await T.choose(setting('Speed units').locator('select'), 'auto'); await T.settled();
      await T.choose(setting('Time zone').locator('select'), 'browser'); await T.settled();
      if (await clock.isChecked()) await setting('12-hour clock').locator('.switch').click();
      await T.eventually(async () => (await T.api('/api/settings')).body.data.hour12 === false, '12-hour clock default is restored');
      await T.choose(setting('Default time range').locator('select'), '7d'); await T.settled();
      await page.getByRole('radio', { name: 'Dark' }).or(page.getByRole('tab', { name: 'Dark' })).first().click(); await T.settled();
    },

    // The speed-test and location tabs keep their choices; the licence key is write-only.
    async engineAndLocation() {
      await T.open('#/settings', 'Settings');
      await T.tab('Speed test');
      await T.choose(setting('Parallel connections').locator('select'), '8');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.defaultThreads === 8, 'parallel connections are stored');
      await T.choose(setting('Server choice').locator('select'), 'pinned');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.serverMode === 'pinned', 'server choice is stored');
      await T.tab('Location');
      await T.see('Provider budgets');
      await setting('Provider budgets').getByText('IPinfo Lite', { exact: true }).waitFor();
		await checkLocationSettingsLayout(page, assert);
      const providers = await T.api('/api/ip/providers');
      assert.ok(Array.isArray(providers.body.data) && providers.body.data.some(p => p.name === 'ipwho' && Number.isInteger(p.requestsToday)), 'provider switches and persisted usage are visible');
      assert.ok(!providers.text.toLowerCase().includes('token'), 'provider status never returns a credential');
      await T.fillAndLeave('MaxMind license key', 'e2e-licence-key-123');
      await T.settled();
      await setting('Update the database weekly').locator('.switch').click();
      await T.settled();
      const read = (await T.api('/api/settings')).text;
      assert.ok(!read.includes('e2e-licence-key-123'), 'the licence key is never returned');
      await page.reload();
      await T.tab('Speed test');
      assert.equal(await setting('Parallel connections').locator('select').inputValue(), '8');
      await T.choose(setting('Parallel connections').locator('select'), '4');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.defaultThreads === 4, 'default parallel connections are restored');
      await T.choose(setting('Server choice').locator('select'), 'auto');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.serverMode === 'auto', 'automatic server choice is restored');
    },

    // Secrets are write-only and stored encrypted.
    async notificationSecrets() {
      let templateReads = 0;
      const countTemplateRead = response => {
        if (response.request().method() === 'GET' && response.url().includes('/api/notifications/templates/')) templateReads++;
      };
      page.on('response', countTemplateRead);
      try {
        await T.open('#/settings', 'Settings');
        await T.tab('Notifications');
        await T.settled();
        assert.equal(templateReads, 0, 'opening the notification catalog does not fetch every template');
        await T.fillAndLeave('URL for Webhook', 'https://hooks.example.com/e2e-secret-path');
        await T.toast('Saved').waitFor();
        await T.settled();
        await T.fillAndLeave('Address for Email', 'ops@example.com');
        await T.settled();
        await page.getByLabel('Enable Email').check({ force: true });
        await T.settled();
        assert.equal(templateReads, 0, 'saving channel settings does not refetch event templates');
        await T.eventually(async () => (await T.api('/api/settings')).text.includes('"configured":true'), 'the read model says a secret is configured');
        const read = (await T.api('/api/settings')).text;
        assert.ok(!read.includes('e2e-secret-path'), 'secrets are never returned');
        assert.ok(Number(T.db('select count(*) from settings where secret_ciphertext is not null')) >= 1, 'the secret is stored encrypted');
      } finally {
        page.off('response', countTemplateRead);
      }
    },

    // The center sends a test message and the owner sees the outcome; an unset channel is refused in plain words.
    async notificationTest() {
      await T.open('#/settings', 'Settings');
      await T.tab('Notifications');
      await T.fillAndLeave('URL for Webhook', receiver.url);
      await T.toast('Saved').waitFor();
      await T.settled();
      await page.getByLabel('Enable Webhook').check({ force: true });
      await T.eventually(async () => {
        const settings = (await T.api('/api/settings')).body.data;
        return settings.notify.webhook.on && settings.notify.webhook.configured;
      }, 'the webhook is enabled and configured');
      await T.press('Send a test', setting('Webhook'));
      await T.toast('Test message sent to Webhook').waitFor({ timeout: 30000 });
      assert.ok(receiver.bodies.length >= 1, 'the receiver got the message');
      assert.ok(!JSON.stringify(receiver.bodies[0]).includes('secret'), 'no secret in the message');
      await T.press('Send a test', setting('Telegram'));
      await T.toast('Telegram is not set up yet').waitFor();
      T.expectRefusal(422);
    },

    // Event catalog: edit, preview, deliver, and reset the exact event template.
    async notificationEventCatalog() {
      await T.open('#/settings', 'Settings');
      await T.tab('Notifications');
      await T.fillAndLeave('URL for Webhook', receiver.url);
      await T.toast('Saved').waitFor();
      await T.settled();
      await page.getByLabel('Enable Webhook').check({ force: true });
      await T.settled();
      await T.see('Events and message templates');
      const quality = page.locator('[data-event-key="quality.ping_high"]');
      await quality.waitFor();
      await quality.locator('details > summary').click();
      const consecutive = quality.getByLabel('Threshold for tests for Ping above limit');
      await consecutive.fill('3');
      await T.press('Save event settings', quality);
      await T.toast('Event settings saved').waitFor();
      assert.equal(T.db("select thresholds->>'for_tests' from notification_event_settings where event_key='quality.ping_high'"), '3', 'quality alerts expose a configurable integer streak');
      await consecutive.fill('2');
      await T.press('Save event settings', quality);
      await T.toast('Event settings saved').waitFor();

      const card = page.locator('[data-event-key="test.success"]');
      await card.waitFor();
      const editor = card.locator('textarea');
      await editor.waitFor();
      assert.equal(await editor.isDisabled(), true, 'template content is loaded on request');
      await T.press('Load', card);
      await T.eventually(async () => (await editor.inputValue()).includes('{node}'), 'the default template loads');
      const original = await editor.inputValue();
      assert.ok(original.includes('{node}'), 'the default template loads');
      await card.locator('details > summary').click();
      await card.getByLabel('Route Test success to webhook').check({ force: true });
      await T.press('Save event settings', card);
      await T.toast('Event settings saved').waitFor();
      assert.equal(T.db("select enabled::text from notification_event_settings where event_key='test.success'"), 'true', 'saving channels preserves the event default');
      await card.getByLabel('Template channel for Test success').selectOption('webhook');
      await T.eventually(async () => (await editor.inputValue()).includes('{node}'), 'the webhook template loads');
      await editor.fill('{"event":"{event}","text":"E2E preview for {node} download {download}"}');
      await T.eventually(async () => (await card.locator('pre').innerText()) === '{"event":"Test success","text":"E2E preview for Viettel Fast 50 download 180 Mbps"}', 'the live preview exactly renders sample data');
      await T.press('Save template', card);
      await T.toast('Template saved').waitFor();
      assert.ok(T.db("select body from notification_templates where event_key='test.success' and channel='webhook'").includes('E2E preview'), 'custom webhook template is stored');
      const deliveredBefore = receiver.bodies.length;
      await T.press('Send test', card);
      await T.eventually(() => {
        const state = T.db("select d.status||':'||d.attempts||':'||coalesce(d.error,'') from notification_deliveries d join alert_events e on e.id=d.event_id where e.event_key='test.success' and e.detail='Template test' and d.channel='webhook' order by d.created_at desc limit 1");
        if (state.startsWith('failed:')) throw new Error('template test delivery failed: ' + state);
        return state.startsWith('sent:') ? true : state || 'no template test delivery';
      }, 'the template test delivery reaches the configured webhook', 30000);
      await T.toast('Test message delivered').waitFor({ timeout: 30000 });
      await T.eventually(() => receiver.bodies.slice(deliveredBefore).some(body => body.text === 'E2E preview for Viettel Fast 50 download 180 Mbps'), 'the configured webhook receives the exact edited event template');
      const activity = await T.api('/api/alerts/events?state=all');
      const sampleEvent = activity.body.data.find(event => event.title === 'Test success');
      assert.ok(sampleEvent, 'the template test is recorded in alert activity');
      assert.equal(sampleEvent.nodeId, null, 'system template tests have no node relationship');
      await T.press('Reset to default', card);
      await T.toast('Template reset').waitFor();
      assert.equal(Number(T.db("select count(*) from notification_templates where event_key='test.success' and channel='webhook'")), 0, 'reset removes the override');
      await card.getByLabel('Route Test success to webhook').uncheck({ force: true });
      await T.press('Save event settings', card);
      await T.toast('Event settings saved').waitFor();
      const globalToggle = card.getByRole('checkbox', { name: 'Enable Test success', exact: true });
      await globalToggle.evaluate(input => input.scrollIntoView({ block: 'center' }));
      assert.equal(await globalToggle.isChecked(), true, 'test success is enabled by default');
      await globalToggle.uncheck();
      await T.toast('Saved').waitFor();
      await T.eventually(() => T.db("select enabled::text from notification_event_settings where event_key='test.success'") === 'false', 'event toggle is stored');
      await globalToggle.check();
      await T.settled();
      const nodeToggle = card.getByLabel('Enable Test success for VM1');
      await nodeToggle.waitFor();
      await T.eventually(() => nodeToggle.isEnabled(), 'the per-node setting has loaded');
      await nodeToggle.uncheck({ force: true });
      await T.eventually(() => T.db("select ns.enabled::text from notification_event_node_settings ns join nodes n on n.id=ns.node_id where ns.event_key='test.success' and n.name='VM1'") === 'false', 'per-node event setting is stored');
      await nodeToggle.check({ force: true });
      await T.eventually(() => T.db("select ns.enabled::text from notification_event_node_settings ns join nodes n on n.id=ns.node_id where ns.event_key='test.success' and n.name='VM1'") === 'true', 'per-node event setting can be restored');
    },

    // Quiet hours for messages: both ends or neither; the critical exemption; stored with the owner's time zone.
    async notificationQuietHours() {
      await T.open('#/settings', 'Settings');
      await T.tab('Notifications');
      // Migrations seed the normal quiet window, so clear it before testing an incomplete edit.
      await T.fillAndLeave('Hold messages from', '');
      await T.fillAndLeave('Hold messages until', '');
      await T.settled();
      await T.fillAndLeave('Hold messages from', '22:00');
      await T.settled();
      assert.notEqual(T.db("select value #>> '{}' from settings where key='notifyQuietStart'"), '22:00', 'a half-typed window is not saved');
      await T.fillAndLeave('Hold messages until', '07:00');
      await T.toast('Saved').waitFor();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='notifyQuietStart'") === '22:00', 'the quiet window is stored');
      assert.equal(T.db("select value #>> '{}' from settings where key='notifyQuietEnd'"), '07:00');
      assert.ok(T.db("select value #>> '{}' from settings where key='notifyQuietTimezone'").length > 2, 'the owner time zone is stored with it');
      await page.getByLabel('Critical alerts are still delivered').uncheck();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='notifyQuietCritical'") === 'false', 'the critical exemption is stored');
      await page.reload();
      await T.tab('Notifications');
      assert.equal(await page.getByLabel('Hold messages from').inputValue(), '22:00', 'the window survives a reload');
      // Put the migration defaults back: quiet hours enabled, critical alerts delivered.
      await T.fillAndLeave('Hold messages from', '23:00'); await T.settled();
      await T.fillAndLeave('Hold messages until', '07:00'); await T.settled();
      await page.getByLabel('Critical alerts are still delivered').check(); await T.settled();
    },

    // Data: the retention period and the full exports.
    async dataRetentionAndExport() {
      await T.open('#/settings', 'Settings');
      await T.tab('Data');
      await T.choose(setting('Keep results for').locator('select'), '90');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.retentionDays === 90, 'retention period is stored');
      await T.toast('Saved').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='retentionDays'") === '90', 'retention period is saved');
      assert.equal(T.db("select value #>> '{}' from settings where key='retentionDays'"), '90');
      const csv = await T.download(() => T.press('CSV', setting('Export all results')));
      assert.ok(csv.startsWith('time,node,status,download_mbps'), 'the CSV export has its header');
      const json = await T.download(() => T.press('JSON', setting('Export all results')));
      assert.ok(Array.isArray(JSON.parse(json)) || JSON.parse(json).data, 'the JSON export parses');
      await T.choose(setting('Keep results for').locator('select'), '365');
      await T.eventually(async () => (await T.api('/api/settings')).body.data.retentionDays === 365, 'default retention is restored');
      await T.settled();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='retentionDays'") === '365', 'the default retention period is restored');
      await T.toast('Saved').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='retentionDays'") === '365', 'default retention period is restored');
    },

    async bandwidthBudgets() {
      await budgetReadFailureRetries(T);
      await T.open('#/settings', 'Settings');
      await T.tab('Bandwidth');
      await page.getByRole('heading', { name: 'Monthly bandwidth budgets' }).waitFor();
      const usage = (await T.api('/api/usage')).body.data;
      assert.ok(usage.monthToDate.totalBytes > 0, 'the seeded results have measured traffic');
      const amountGiB = Math.max(0.1, Math.floor((usage.monthToDate.totalBytes / 1073741824 / 0.9) * 10) / 10);
      const form = page.locator('form');
      await T.choose(form.getByLabel('Scope'), 'global');
      await T.fill('Monthly budget (GiB)', String(amountGiB), form);
      await T.choose(form.getByLabel('At 100%'), 'pause');
      await form.getByRole('button', { name: 'Save budget' }).click();
      await T.toast('Bandwidth budget saved').waitFor();
      await T.eventually(() => T.db("select count(*) from usage_budgets where scope_key='global'") === '1', 'the global budget is stored');
      assert.equal(T.db("select action from usage_budgets where scope_key='global'"), 'pause');
      const vm1 = usage.byNodeEngine.filter(row => row.nodeName === 'VM1').reduce((sum, row) => sum + row.monthToDate.totalBytes, 0);
      const nodeBudgetGiB = Math.max(0.1, Math.floor((vm1 / 1073741824 / 0.9) * 10) / 10);
      const nodeForm = page.locator('form');
      await T.choose(nodeForm.getByLabel('Scope'), '10000000-0000-4000-8000-000000000001');
      await T.fill('Monthly budget (GiB)', String(nodeBudgetGiB), nodeForm);
      await T.choose(nodeForm.getByLabel('At 100%'), 'widen');
      await nodeForm.getByRole('button', { name: 'Save budget' }).click();
      await T.toast('Bandwidth budget saved').waitFor();
      await T.eventually(() => T.db('select count(*) from usage_budgets') === '2', 'the node budget is stored alongside the global budget');
      assert.equal(T.db("select count(*) from usage_budgets"), '2');
      await T.open('#/settings', 'Settings');
      await T.tab('Bandwidth');
      const adviceBudgetForm = page.locator('form');
      await adviceBudgetForm.getByLabel('Scope').selectOption('global');
      await adviceBudgetForm.getByLabel('Monthly budget (GiB)').fill('10000');
      await adviceBudgetForm.getByLabel('At 100%').selectOption('pause');
      await adviceBudgetForm.getByRole('button', { name: 'Save budget' }).click();
      await T.toast('Bandwidth budget saved').waitFor();
      await T.open('#/nodes/10000000-0000-4000-8000-000000000001', /VM1/);
      await T.tab('Settings');
      await T.see('Recommended minimum interval', page);
      await T.fillAndLeave('VM1 Ookla proposed interval in minutes', '120', page);
      await T.see('Estimate for every 120 minutes', page);
      await T.fillAndLeave('VM1 Ookla proposed monthly budget in GiB', '0.001', page);
      await T.see('This budget cannot be met even at a 24-hour interval.', page);
      await T.fillAndLeave('VM1 Ookla proposed monthly budget in GiB', '1000', page);
      await T.see('Recommended minimum interval', page);
      await page.screenshot({ path: '.tmp/e2e/shots/JOOST-80-node-bandwidth-advice.png', fullPage: true });
      await T.open('#/settings', 'Settings');
      await T.tab('Bandwidth');
      const warningForm = page.locator('form');
      await warningForm.getByLabel('Scope').selectOption('global');
      await warningForm.getByLabel('Monthly budget (GiB)').fill(String(amountGiB));
      await warningForm.getByLabel('At 100%').selectOption('pause');
      await warningForm.getByRole('button', { name: 'Save budget' }).click();
      await T.toast('Bandwidth budget saved').waitFor();
      await T.open('#/', 'Dashboard');
      await T.see('Monthly bandwidth budget is at least 80% used', page.locator('#view'));
      await T.see('The configured schedule policy applies when the budget is reached.', page.locator('#view'));
      await page.screenshot({ path: '.tmp/e2e/shots/JOOST-80-dashboard-budget-warning.png', fullPage: true });
      await T.open('#/settings', 'Settings');
      await T.tab('Bandwidth');
      const limitForm = page.locator('form');
      const exhaustedGiB = Math.max(0.001, Number((usage.monthToDate.totalBytes / 1073741824 * 0.5).toFixed(3)));
      await T.choose(limitForm.getByLabel('Scope'), 'global');
      await T.fill('Monthly budget (GiB)', String(exhaustedGiB), limitForm);
      await T.choose(limitForm.getByLabel('At 100%'), 'pause');
      await limitForm.getByRole('button', { name: 'Save budget' }).click();
      await T.toast('Bandwidth budget saved').waitFor();
      await T.eventually(() => T.db('select count(*) from engine_schedules where enabled') === '0', 'the global policy pauses every schedule at 100%');
      await T.open('#/', 'Dashboard');
      await T.see('Monthly bandwidth budget reached', page.locator('#view'));
      await T.see('Tests are paused until the budget resets.', page.locator('#view'));
      await page.screenshot({ path: '.tmp/e2e/shots/JOOST-80-dashboard-budget-limit.png', fullPage: true });
      assert.ok(Number(T.db("select count(*) from audit_log where action='usage_budget.tests_paused'")) > 0, 'the automatic pause is audited');
      await T.open('#/settings', 'Settings');
      await T.tab('Bandwidth');
      await page.locator('.list-row').filter({ hasText: 'Global' }).getByRole('button', { name: 'Remove' }).click();
      await T.toast('Budget removed').waitFor();
      await T.eventually(() => T.db("select count(*) from usage_budgets where scope_key='global'") === '0', 'the global budget is removed');
      assert.equal(T.db("select count(*) from usage_budgets where scope_key='global'"), '0');
      T.db("update engine_schedules set interval_minutes=1440,offset_minutes=0,enabled=true,revision=revision+1 where node_id='10000000-0000-4000-8000-000000000001' and engine='ookla'");
      const maxForm = page.locator('form');
      await maxForm.getByLabel('Scope').selectOption('global');
      await maxForm.getByLabel('Monthly budget (GiB)').fill(String(exhaustedGiB));
      await maxForm.getByLabel('At 100%').selectOption('widen');
      await maxForm.getByRole('button', { name: 'Save budget' }).click();
      await T.toast('Bandwidth budget saved').waitFor();
      await T.eventually(() => T.db("select count(*) from engine_schedules where node_id='10000000-0000-4000-8000-000000000001' and engine='ookla' and interval_minutes=1440 and not enabled") === '1', 'a maximal schedule is paused when widening cannot increase it');
      await T.open('#/', 'Dashboard');
      await T.see('Schedules already at 24 hours are paused; shorter schedules are widened.', page.locator('#view'));
      await T.open('#/settings', 'Settings');
      await T.tab('Bandwidth');
      await page.locator('.list-row').filter({ hasText: 'Global' }).getByRole('button', { name: 'Remove' }).click();
      await T.toast('Budget removed').waitFor();
      await T.eventually(() => T.db("select count(*) from engine_schedules where node_id='10000000-0000-4000-8000-000000000001' and engine='ookla' and interval_minutes=1440 and enabled") === '1', 'removing the global budget restores the original maximal schedule');
      await page.locator('.list-row').filter({ hasText: 'VM1' }).getByRole('button', { name: 'Remove' }).click();
      await T.toast('Budget removed').waitFor();
      await T.eventually(() => T.db('select count(*) from usage_budgets') === '0', 'all budgets are removed');
      assert.equal(T.db('select count(*) from usage_budgets'), '0');
    },

    async demoBandwidthPersistenceAndUTC() {
      const demoContext = await ctx.browser.newContext({ timezoneId: 'America/Los_Angeles', viewport: { width: 1440, height: 900 } });
      const demoPage = await demoContext.newPage();
      try {
        await demoPage.goto(`${ctx.stack.origin}/?demo=1#/stats`);
        await demoPage.getByRole('heading', { name: 'Statistics', level: 1 }).waitFor();
        assert.equal(await demoPage.evaluate(() => new Date('2026-11-02T00:00:00Z').getTimezoneOffset()), 480, 'the browser context is eight hours behind UTC');
        const latest = await demoPage.evaluate(() => ST.results({ nodeIds: ['n1'], engine: 'ookla' }).slice(-1)[0].ts);
        const utcDay = new Date(latest).toISOString().slice(0, 10) + ' UTC';
        const daily = demoPage.locator('#view table').filter({ hasText: 'Day (UTC)' });
        await daily.getByText(utcDay, { exact: true }).first().waitFor();

        await demoPage.goto(`${ctx.stack.origin}/?demo=1#/settings`);
        await demoPage.getByRole('heading', { name: 'Settings', level: 1 }).waitFor();
        await demoPage.getByRole('tab', { name: 'Bandwidth' }).click();
        const form = demoPage.locator('form');
        await form.getByLabel('Scope').selectOption('global');
        await form.getByLabel('Monthly budget (GiB)').fill('1');
        await form.getByLabel('At 100%').selectOption('pause');
        await form.getByRole('button', { name: 'Save budget' }).click();
        await demoPage.locator('#toasts').getByText('Bandwidth budget saved').waitFor();
        await demoPage.reload();
        await demoPage.getByRole('tab', { name: 'Bandwidth' }).click();
        await demoPage.locator('.list-row').filter({ hasText: 'Global' }).waitFor();
        await demoPage.locator('.list-row').filter({ hasText: 'Global' }).getByRole('button', { name: 'Remove' }).click();
        await demoPage.locator('#toasts').getByText('Budget removed').waitFor();
        await demoPage.reload();
        await demoPage.getByRole('tab', { name: 'Bandwidth' }).click();
        assert.equal(await demoPage.locator('.list-row').filter({ hasText: 'Global' }).count(), 0, 'removal persists after reload');

        await demoPage.locator('form').getByLabel('Monthly budget (GiB)').fill('1');
        await demoPage.locator('form').getByRole('button', { name: 'Save budget' }).click();
        await demoPage.locator('#toasts').getByText('Bandwidth budget saved').waitFor();
        // The global budget is shared: the advice for one node and engine reserves what the other nodes and engines consume (as the center does).
        const advice = await demoPage.evaluate(() => ST.bandwidth.advice(ST.node('n1'), 'ookla', 60, 0).data);
        assert.ok(advice.sharedReservedBytes > 0, 'the other nodes and engines reserve part of the shared global budget');
        assert.equal(advice.budgetBytes, 1073741824, 'the budget shown is the configured total');
        await demoPage.getByRole('tab', { name: 'Data' }).click();
        await demoPage.locator('#view').getByRole('button', { name: 'Reset demo data' }).click();
        await demoPage.getByRole('dialog', { name: 'Reset demo data?' }).getByRole('button', { name: 'Reset' }).click();
        await demoPage.getByRole('heading', { name: 'Settings', level: 1 }).waitFor();
        assert.equal(await demoPage.evaluate(() => localStorage.getItem('st.demo.v1')), null, 'reset clears saved demo budgets with the rest of demo state');
      } finally {
        await demoContext.close();
      }
    },

    // The activity log lists the changes made earlier in the run, filters by kind and never shows secrets.
    async activityLog() {
      await T.open('#/settings', 'Settings');
      await T.tab('Activity log');
      await T.see('Changed settings');
      await T.see('Added a node');
      await T.see('Removed a node');
      assert.ok(await page.getByRole('row').count() > 5, 'the earlier changes are listed');
      assert.ok(!(await page.content()).includes('e2e-secret-path'), 'no secret value appears in the log');
      await T.choose(T.selectWith('Nodes'), 'node.');
      await T.settled();
      assert.equal(await page.getByText('Changed settings').count(), 0, 'the node filter hides settings changes');
      await T.see('Added a node');
      const entries = (await T.api('/api/audit?size=100')).body.data;
      assert.ok(entries.some(e => e.actor === 'system:usage-budget' && e.action === 'usage_budget.tests_paused'), 'automatic budget actions are attributed to the system');
      assert.ok(entries.every(e => ['jooservices@gmail.com', 'system:usage-budget'].includes(e.actor) && e.summary && !/secret|password/i.test(JSON.stringify(e).replace('Changed the password', ''))), 'entries name the owner or system and hold no secrets');
      assert.ok(Number(T.db('select count(*) from audit_log')) >= entries.length, 'the entries are stored on the center');
      const refused = await T.api('/api/audit?action=' + 'a'.repeat(61));
      assert.equal(refused.status, 422);
      T.expectRefusal(422);
    },

    // Delivery history reads the authenticated list endpoint and exposes only safe delivery metadata.
    async deliveryHistory() {
      deliveryHistory.seed();
      let loadingGate, removeFailure, injectedMarkup, emptyLaterHandler;
      const originalTheme = await page.evaluate(() => document.documentElement.getAttribute('data-theme') || 'dark');
      try {
        removeFailure = await deliveryHistory.failNextList();
        await deliveryHistory.open();
        await page.getByRole('alert').waitFor();
        const initialError = await page.getByRole('alert').innerText();
        assert.match(initialError, /Could not load delivery history/);
        assert.equal(await deliveryHistory.pageStatus().count(), 0, 'SC-SET-12 initial failure has no default page count');
        assert.equal(await page.getByRole('navigation', { name: 'Delivery history pagination' }).isVisible(), false, 'SC-SET-12 initial failure hides pagination');
        assert.equal(await deliveryHistory.rows().count(), 0, 'SC-SET-12 initial failure has no rows presented as current data');
        for (const secret of ['provider response canary', 'message body canary', 'destination-secret-canary']) {
          assert.ok(!initialError.includes(secret), `the request-error state hides ${secret}`);
        }
        T.expectRefusal(500);
        await removeFailure();
        removeFailure = null;
        loadingGate = await deliveryHistory.blockNextList();
        const retryRequest = page.waitForRequest(request => new URL(request.url()).pathname === '/api/notifications/deliveries', { timeout: 10000 });
        await Promise.all([deliveryHistory.retry(), retryRequest]);
        await deliveryHistory.loadingState().waitFor();
        assert.match(await deliveryHistory.loadingState().innerText(), /Loading delivery history/);
        assert.equal(await deliveryHistory.statusDisabled(), 'true', 'filters expose their loading state accessibly');
        assert.equal(await deliveryHistory.pageStatus().count(), 0, 'SC-SET-12 loading state does not present stale page counts');
        await loadingGate.release();
        await page.getByText('safe failure detail 12', { exact: true }).waitFor();
        assert.equal(await deliveryHistory.statusDisabled(), 'false', 'filters become available after loading');
        await deliveryHistory.waitForPage(1);
        assert.equal(await deliveryHistory.rows().count(), 11, 'page one shows ten deliveries plus its header');
        assert.match(await deliveryHistory.rows().nth(1).innerText(), /safe failure detail 12/, 'newest delivery appears first');
        assert.match(await deliveryHistory.rows().nth(2).innerText(), /safe failure detail 11/, 'the next newest delivery follows');

        await deliveryHistory.focusStatus();
        await deliveryHistory.tabKey();
        assert.equal(await deliveryHistory.activeName(), 'Channel', 'Tab moves from Status to Channel');
        await deliveryHistory.tabKey();
        assert.equal(await deliveryHistory.activeName(), 'Delivery history table', 'Tab reaches the named, scrollable table region');
        await deliveryHistory.tabKey();
        assert.equal(await deliveryHistory.activeName(), 'Next delivery history page', 'Tab reaches pagination');
        await deliveryHistory.enterKey();
        await deliveryHistory.waitForPage(2);
        assert.equal(await deliveryHistory.activePaginationName(), 'Previous delivery history page', 'SC-SET-12 keyboard Next restores focus to the available direction at the last page');
        assert.ok(await deliveryHistory.rows().count() >= 3, 'page two contains the remaining fixtures and its header');
        assert.match(await deliveryHistory.rows().nth(1).innerText(), /Telegram/, 'older page starts with its newest remaining delivery');
        assert.match(await deliveryHistory.rows().nth(2).innerText(), /Slack/);

        await deliveryHistory.enterKey();
        await deliveryHistory.waitForPage(1);
        assert.equal(await deliveryHistory.activePaginationName(), 'Next delivery history page', 'SC-SET-12 keyboard Previous restores focus to the available direction at the first page');

        loadingGate = await deliveryHistory.blockNextList();
        await deliveryHistory.focusNext();
        const pendingPage = deliveryHistory.enterKey();
        await loadingGate.started;
        await page.getByLabel('Channel', { exact: true }).focus();
        await loadingGate.release();
        await pendingPage;
        await deliveryHistory.waitForPage(2);
        assert.equal(await deliveryHistory.activeName(), 'Channel', 'SC-SET-12 pagination does not steal focus the owner moved during loading');
        await loadingGate.remove();
        loadingGate = null;
        await deliveryHistory.previous();
        await deliveryHistory.waitForPage(1);

        emptyLaterHandler = async route => {
          const response = await route.fetch();
          const body = await response.json();
          if (new URL(route.request().url()).searchParams.get('page') === '2') body.data = [];
          await route.fulfill({ response, body: JSON.stringify(body) });
        };
        await page.route('**/api/notifications/deliveries**', emptyLaterHandler);
        await deliveryHistory.focusNext();
        await deliveryHistory.enterKey();
        await page.getByText('No deliveries on this page', { exact: true }).waitFor();
        assert.equal(await page.getByText('No deliveries match these filters', { exact: true }).count(), 0, 'SC-SET-12 a count/row race does not claim that all filtered deliveries disappeared');
        await deliveryHistory.previous();
        await deliveryHistory.waitForPage(1);
        await page.unroute('**/api/notifications/deliveries**', emptyLaterHandler);
        emptyLaterHandler = null;

        await deliveryHistory.status('failed');
        await page.getByText('safe failure detail 12', { exact: true }).waitFor();
        assert.equal(await deliveryHistory.rows().count(), 7, 'the failed filter shows all six failures');
        await deliveryHistory.channel('email');
        await page.getByText('safe failure detail 11', { exact: true }).waitFor();
        assert.equal(await deliveryHistory.rows().count(), 3, 'status and channel filters combine');
        await deliveryHistory.status('pending');
        await deliveryHistory.waitForPage(1);
        await deliveryHistory.channel('email');
        await page.getByText('No deliveries match these filters', { exact: true }).waitFor();
        assert.equal(await page.getByTestId('delivery-history-results').getByRole('table').count(), 0, 'an empty filter result has no stale table');
        await deliveryHistory.clearFilters();
        await page.getByText('safe failure detail 12', { exact: true }).waitFor();

        removeFailure = await deliveryHistory.failNextList();
        await deliveryHistory.status('failed');
        const requestError = await page.getByRole('alert').innerText();
        const failedFilterRequests = removeFailure.requests;
        assert.match(requestError, /Could not load delivery history/);
        assert.equal(await deliveryHistory.pageStatus().count(), 0, 'SC-SET-12 filter failure hides stale pagination counts');
        assert.equal(await page.getByRole('navigation', { name: 'Delivery history pagination' }).isVisible(), false, 'SC-SET-12 filter failure hides pagination');
        assert.equal(await page.getByLabel('Status', { exact: true }).inputValue(), 'failed', 'SC-SET-12 failure retains the selected status filter');
        assert.equal(await deliveryHistory.rows().count(), 0, 'SC-SET-12 filter failure clears stale rows');
        for (const secret of ['provider response canary', 'message body canary', 'destination-secret-canary']) {
          assert.ok(!requestError.includes(secret), `the request-error state hides ${secret}`);
        }
        T.expectRefusal(500);
        await removeFailure();
        assert.equal(failedFilterRequests.at(-1).searchParams.get('status'), 'failed', 'SC-SET-12 failed request used the selected status filter');
        removeFailure = null;
        await deliveryHistory.retry();
        await deliveryHistory.waitForPage(1);
        assert.equal(await deliveryHistory.rows().count(), 7, 'retry restores the six failed deliveries');
        assert.equal(await page.getByLabel('Status', { exact: true }).inputValue(), 'failed', 'SC-SET-12 Retry retains the selected status filter');

        await deliveryHistory.status('');
        await deliveryHistory.waitForPage(1);
        await deliveryHistory.channel('');
        await deliveryHistory.waitForPage(1);
        await page.getByText('safe failure detail 12', { exact: true }).waitFor();
        removeFailure = await deliveryHistory.failNextList();
        await deliveryHistory.focusNext();
        await deliveryHistory.enterKey();
        await page.getByRole('alert').waitFor();
        assert.equal(await deliveryHistory.pageStatus().count(), 0, 'SC-SET-12 page failure hides old totals and page labels');
        assert.equal(await deliveryHistory.rows().count(), 0, 'SC-SET-12 page failure clears previous page rows');
        assert.equal(await deliveryHistory.retryFocused(), true, 'SC-SET-12 failed pagination moves focus to Retry');
        T.expectRefusal(500);
        await removeFailure();
        const failedPageRequests = removeFailure.requests;
        removeFailure = null;
        const pageRetryRequest = page.waitForRequest(request => new URL(request.url()).pathname === '/api/notifications/deliveries', { timeout: 10000 });
        const [, retriedRequest] = await Promise.all([deliveryHistory.retry(), pageRetryRequest]);
        const retryURL = new URL(retriedRequest.url());
        await deliveryHistory.waitForPage(2);
        assert.equal(failedPageRequests.at(-1).searchParams.get('page'), '2', 'SC-SET-12 failed page request used page two');
        assert.equal(retryURL.searchParams.get('page'), '2', 'SC-SET-12 Retry requests the selected page');
        assert.equal(await deliveryHistory.activePaginationName(), 'Previous delivery history page', 'SC-SET-12 Retry restores focus to an enabled pagination control');
        assert.ok(await deliveryHistory.rows().count() >= 3, 'SC-SET-12 retry restores data for the selected page');
        await deliveryHistory.previous();
        await deliveryHistory.waitForPage(1);

        injectedMarkup = await deliveryHistory.injectMarkupIntoNextFailure();
        await deliveryHistory.channel('email');
        await page.getByText(injectedMarkup.markup, { exact: true }).waitFor();
        assert.equal(await page.locator('img[data-delivery-injected="true"]').count(), 0, 'failure detail is rendered as text, not markup');
        await injectedMarkup.remove();
        injectedMarkup = null;
        await deliveryHistory.status('');
        await deliveryHistory.channel('');
        await page.getByText('safe failure detail 12', { exact: true }).waitFor();

        for (const theme of ['dark', 'light']) {
          await page.evaluate(value => document.documentElement.setAttribute('data-theme', value), theme);
          for (const width of [390, 768]) {
            await deliveryHistory.setViewport(width);
            await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
            const layout = await deliveryHistory.layout();
            assert.ok(layout.pageOverflow <= 1, `${theme} layout overflows the ${width}px viewport by ${layout.pageOverflow}px`);
            assert.deepEqual(layout.overflow, [], `${theme} delivery history content overflows at ${width}px`);
            if (width === 390) assert.equal(layout.tableDisplay, 'block', 'the table becomes stacked cards on a phone');
          }
        }
        await deliveryHistory.restoreViewport();
        T.db("DELETE FROM notification_deliveries WHERE id::text IN ('d1260000-0000-4000-8000-000000000001', 'd1260000-0000-4000-8000-000000000002', 'd1260000-0000-4000-8000-000000000003', 'd1260000-0000-4000-8000-000000000004')");
        const remaining = Number(T.db('SELECT count(*) FROM notification_deliveries'));
        assert.ok(remaining > 0 && remaining <= 10, 'the fixture now fits on one page before requesting old page two');
        const outOfRangeRequest = page.waitForRequest(request => {
          const url = new URL(request.url());
          return url.pathname === '/api/notifications/deliveries' && url.searchParams.get('page') === '2';
        }, { timeout: 10000 });
        const clampedRequest = page.waitForRequest(request => {
          const url = new URL(request.url());
          return url.pathname === '/api/notifications/deliveries' && url.searchParams.get('page') === '1';
        }, { timeout: 10000 });
        await deliveryHistory.focusNext();
        await Promise.all([deliveryHistory.enterKey(), outOfRangeRequest, clampedRequest]);
        await deliveryHistory.waitForPage(1);
        assert.equal(await deliveryHistory.rows().count(), remaining + 1, 'a page beyond the new last page reloads every remaining delivery plus the table header');
        assert.equal(await deliveryHistory.activeName(), 'Status', 'clamping to one page restores focus to an enabled filter');
      } finally {
        if (loadingGate) await loadingGate.remove();
        if (removeFailure) await removeFailure();
        if (injectedMarkup) await injectedMarkup.remove();
        if (emptyLaterHandler) await page.unroute('**/api/notifications/deliveries**', emptyLaterHandler);
        await deliveryHistory.restoreViewport();
        await page.evaluate(value => document.documentElement.setAttribute('data-theme', value), originalTheme);
        deliveryHistory.cleanup();
      }
    },

    // Access: the enrolment key is shown once; the dashboard address is editable.
    async enrolmentKeyShownOnce() {
      await T.open('#/settings', 'Settings');
      await T.tab('Access');
      await T.press('Generate a new key');
      await T.press('Generate', page.getByRole('dialog'));
      const key = await page.locator('pre, code, .code').first().innerText();
      assert.ok(key.length > 20, 'the new key is shown');
      await T.see('It will not be shown again.');
      await page.reload();
      await T.tab('Access');
      assert.ok(!(await page.content()).includes(key.trim()), 'the key is not shown again after a reload');
      // Cancelling the confirmation changes nothing.
      await T.press('Generate a new key');
      await T.press('Cancel', page.getByRole('dialog'));
    },

    async apiTokenCanBeCreatedUsedAndRevoked() {
      await T.open('#/settings', 'Settings');
      await T.tab('Access');
      const tokenName = page.getByLabel('Token name');
      const create = page.getByRole('button', { name: 'Create API token' });
      await tokenName.fill('e2e-read-token');
      await page.getByLabel('Scope').selectOption('read');
      let failOnce = true;
      await page.route('**/api/access-tokens', async route => {
        if (route.request().method() === 'POST' && failOnce) {
          failOnce = false;
          await route.fulfill({ status: 422, contentType: 'application/problem+json', body: JSON.stringify({ type: '/problems/validation', status: 422, title: 'Validation failed', detail: 'Try again.' }) });
          return;
        }
        await route.continue();
      });
      await create.click();
      await T.eventually(async () => !(await create.isDisabled()), 'a failed create can be retried');
      T.expectRefusal(422);
      assert.equal(await tokenName.inputValue(), 'e2e-read-token', 'a failed create leaves the name available for retry');
      await page.unroute('**/api/access-tokens');

      let releasePost, requestSeen;
      const postGate = new Promise(resolve => { releasePost = resolve; });
      const arrived = new Promise(resolve => { requestSeen = resolve; });
      let postCount = 0;
      await page.route('**/api/access-tokens', async route => {
        if (route.request().method() !== 'POST') return route.continue();
        postCount++;
        requestSeen();
        await postGate;
        await route.continue();
      });
      const submission = create.click();
      await arrived;
      assert.equal(await create.isDisabled(), true, 'the create button stays disabled while its request is pending');
      await create.evaluate(button => button.click());
      await page.waitForTimeout(100);
      assert.equal(postCount, 1, 'a second click cannot issue another token while creation is pending');
      releasePost();
      await submission;
      await page.unroute('**/api/access-tokens');
      await T.see('API token created');
      const disclosures = page.locator('.token-disclosure .recovery-codes');
      const raw = (await disclosures.nth(0).innerText()).trim();
      assert.ok(raw.startsWith('st_at_'), 'the bearer credential is shown after creation');
      const storedHash = T.db("select encode(token_hash,'hex') from api_access_tokens where name='e2e-read-token'");
      assert.ok(storedHash === createHash('sha256').update(raw).digest('hex'), 'only its SHA-256 digest is stored');
      const tokenID = T.db("select id::text from api_access_tokens where name='e2e-read-token'");
      await T.eventually(async () => await T.row('e2e-read-token').getByText(tokenID.slice(0, 8), { exact: true }).count() === 1, 'the non-secret token ID is shown for audit correlation');

      await tokenName.fill('e2e-expiring-token');
      await page.getByLabel('Optional expiry').fill(new Date(Date.now() + 86400000).toISOString().slice(0, 16));
      await create.click();
      await T.see('API token created');
      const expiringRaw = (await disclosures.nth(1).innerText()).trim();
      assert.ok(expiringRaw.startsWith('st_at_'), 'the second credential is shown');
      assert.equal(await disclosures.count(), 2, 'issuing a second token retains the first one-time disclosure');
      await page.getByRole('button', { name: 'I saved this token' }).first().click();
      assert.equal(await disclosures.count(), 1, 'a disclosure can be removed after explicit acknowledgement');

      const client = await apiRequest.newContext();
      try {
        const read = await client.get(ctx.stack.origin + '/api/nodes', { headers: { Authorization: `Bearer ${raw}` } });
        assert.equal(read.status(), 200, 'bearer GET works without a session cookie');
        const result = await read.json();
        assert.equal(result.success, true);
        const isps = await client.get(ctx.stack.origin + '/api/isps?range=7d', { headers: { Authorization: `Bearer ${raw}` } });
        assert.equal(isps.status(), 200, 'read scope can list ISPs without a session cookie');
        assert.ok(Array.isArray((await isps.json()).data), 'the ISP response keeps its list envelope');
        const planWrite = await client.post(ctx.stack.origin + '/api/isps/00000000-0000-4000-8000-000000000001/plans', {
          headers: { Authorization: `Bearer ${raw}` }, data: { name: 'Blocked plan', advertisedDown: 100, advertisedUp: 50 },
        });
        assert.equal(planWrite.status(), 403, 'read scope refuses ISP plan writes');
        const write = await client.post(ctx.stack.origin + '/api/nodes/123e4567-e89b-12d3-a456-426614174000/run', { headers: { Authorization: `Bearer ${raw}` } });
        assert.equal(write.status(), 403, 'read scope refuses a write operation');
        await T.eventually(() => T.db("select last_used_at is not null from api_access_tokens where name='e2e-read-token'") === 't', 'successful bearer use updates last-used time');
      } finally {
        await client.dispose();
      }
      T.db("update api_access_tokens set expires_at=now()-interval '1 minute' where name='e2e-expiring-token'");
      await page.reload();
      await T.tab('Access');
      await T.eventually(async () => await T.row('e2e-read-token').getByText('Active').count() === 1, 'an unexpired token is active');
      await T.eventually(async () => await T.row('e2e-expiring-token').getByText('Expired').count() === 1, 'an elapsed expiry is shown as expired');
      await T.press('Revoke', T.row('e2e-expiring-token'));
      await T.press('Revoke', page.getByRole('dialog'));
      await T.see('API token revoked');
      await T.eventually(async () => await T.row('e2e-expiring-token').getByText('Revoked').count() === 1, 'revoked takes precedence over expired');
      const item = T.row('e2e-read-token');
      const installationStream = await startBearerInstallationStream(raw, tokenID);
      try {
        await T.press('Revoke', item);
        await T.press('Revoke', page.getByRole('dialog'));
        await T.see('API token revoked');
        await T.eventually(() => T.db("select revoked_at is not null from api_access_tokens where name='e2e-read-token'") === 't', 'the second revoke request is committed');
        await installationStream.waitForRevocation();
      } finally {
        await installationStream.cleanup();
      }
      const clientAfterRevoke = await apiRequest.newContext();
      try {
        const response = await clientAfterRevoke.get(ctx.stack.origin + '/api/nodes', { headers: { Authorization: `Bearer ${raw}` } });
        assert.equal(response.status(), 401, 'revocation stops bearer access immediately');
      } finally {
        await clientAfterRevoke.dispose();
      }
      await page.reload();
      await T.tab('Access');
      assert.ok(!(await page.content()).includes(raw), 'the token is not shown again after a reload');
    },
  };
}

async function budgetReadFailureRetries(T) {
  const { page, assert } = T;
  let reads = 0;
  const countRead = request => {
    const url = new URL(request.url());
    if (request.method() === 'GET' && url.pathname === '/api/usage/budgets') reads++;
  };
  page.on('request', countRead);
  await page.route('**/api/usage/budgets', async route => {
    const url = new URL(route.request().url());
    if (route.request().method() === 'GET' && url.pathname === '/api/usage/budgets' && reads === 1) {
      await route.fulfill({ status: 503, contentType: 'application/problem+json', body: JSON.stringify({ type: 'about:blank', title: 'Temporary failure', status: 503, detail: 'Retry the budget request.' }) });
      return;
    }
    await route.continue();
  });
  await page.reload();
  await T.open('#/settings', 'Settings');
  await T.tab('Bandwidth');
  await page.getByRole('heading', { name: 'Budgets are unavailable' }).waitFor();
  await page.waitForTimeout(150);
  assert.equal(reads, 1, 'a failed budget request does not retry itself');
  T.expectRefusal(503);
  await page.getByRole('button', { name: 'Retry budgets' }).click();
  await page.getByRole('heading', { name: 'Monthly bandwidth budgets' }).waitFor();
  assert.equal(reads, 2, 'the accessible retry makes one new request');
  await page.unroute('**/api/usage/budgets');
  page.off('request', countRead);
}
