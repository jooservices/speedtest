import { createHash } from 'node:crypto';
import { request as apiRequest } from 'playwright';

export function settingsSuite(T, ctx) {
  const { page, assert } = T;
  const { receiver } = ctx;
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
    // General: every preference is saved on the center and survives a reload.
    async preferences() {
      await T.open('#/settings', 'Settings');
      await T.choose(setting('Decimal places').locator('select'), '2');
      await T.eventually(() => T.db("select value->>'decimals' from settings where key='display'") === '2', 'the selected decimal precision is saved');
      assert.equal(T.db("select value->>'decimals' from settings where key='display'"), '2');
      await T.settled();
      await T.choose(setting('Speed units').locator('select'), 'Mbps');
      await T.settled();
      await T.choose(setting('Time zone').locator('select'), 'UTC');
      await T.settled();
      await setting('12-hour clock').locator('.switch').click();
      await T.settled();
      await T.choose(setting('Default time range').locator('select'), '24h');
      await T.settled();
      await page.getByRole('radio', { name: 'Light' }).or(page.getByRole('tab', { name: 'Light' })).first().click();
      await T.settled();
      assert.deepEqual(JSON.parse(T.db("select value from settings where key='display'")).hour12, true);
      await page.reload();
      await T.heading('Settings').waitFor();
      assert.equal(await setting('Decimal places').locator('select').inputValue(), '2', 'the preference survives a reload');
      assert.equal(await setting('Time zone').locator('select').inputValue(), 'UTC');
      // Put the defaults back so later scenarios see the usual numbers.
      await T.choose(setting('Decimal places').locator('select'), '1'); await T.settled();
      await T.choose(setting('Speed units').locator('select'), 'auto'); await T.settled();
      await T.choose(setting('Time zone').locator('select'), 'browser'); await T.settled();
      await setting('12-hour clock').locator('.switch').click(); await T.settled();
      await T.choose(setting('Default time range').locator('select'), '7d'); await T.settled();
      await page.getByRole('radio', { name: 'Dark' }).or(page.getByRole('tab', { name: 'Dark' })).first().click(); await T.settled();
    },

    // The speed-test and location tabs keep their choices; the licence key is write-only.
    async engineAndLocation() {
      await T.open('#/settings', 'Settings');
      await T.tab('Speed test');
      await T.choose(setting('Parallel connections').locator('select'), '8');
      await T.toast('Saved').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select value->>'defaultThreads' from settings where key='display'") === '8', 'the default parallel connections are stored');
      await T.choose(setting('Server choice').locator('select'), 'pinned');
      await T.settled();
      await T.tab('Location');
      await T.fillAndLeave('MaxMind license key', 'e2e-licence-key-123');
      await T.settled();
      await setting('Update the database weekly').locator('.switch').click();
      await T.settled();
      const read = (await T.api('/api/settings')).text;
      assert.ok(!read.includes('e2e-licence-key-123'), 'the licence key is never returned');
      await page.reload();
      await T.tab('Speed test');
      assert.equal(await setting('Parallel connections').locator('select').inputValue(), '8');
      await T.choose(setting('Parallel connections').locator('select'), '4'); await T.settled();
      await T.choose(setting('Server choice').locator('select'), 'auto'); await T.settled();
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
      await T.settled();
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
      await T.see('Events and message templates');
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
      await T.toast('Saved').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='retentionDays'") === '90', 'retention period is saved');
      assert.equal(T.db("select value #>> '{}' from settings where key='retentionDays'"), '90');
      const csv = await T.download(() => T.press('CSV', setting('Export all results')));
      assert.ok(csv.startsWith('time,node,status,download_mbps'), 'the CSV export has its header');
      const json = await T.download(() => T.press('JSON', setting('Export all results')));
      assert.ok(Array.isArray(JSON.parse(json)) || JSON.parse(json).data, 'the JSON export parses');
      await T.choose(setting('Keep results for').locator('select'), '365');
      await T.toast('Saved').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='retentionDays'") === '365', 'default retention period is restored');
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
      assert.ok(entries.every(e => e.actor === 'jooservices@gmail.com' && e.summary && !/secret|password/i.test(JSON.stringify(e).replace('Changed the password', ''))), 'entries name the owner and hold no secrets');
      assert.ok(Number(T.db('select count(*) from audit_log')) >= entries.length, 'the entries are stored on the center');
      const refused = await T.api('/api/audit?action=' + 'a'.repeat(61));
      assert.equal(refused.status, 422);
      T.expectRefusal(422);
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
