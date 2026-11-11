export function nodesSuite(T, ctx) {
  const { page, assert } = T;
  const { stack, startAgent, fetchAgentConfig, submitScheduledResult, submitFailedResult, sendAgentHeartbeat, receiver } = ctx;
  return {
    async publishedUpdate() {
      T.db("update nodes set agent='speedtest-agent/1.0.0 (linux/amd64)' where name='VM3'");
      try {
        await T.open('#/nodes', 'Nodes');
        await T.row('VM3').click();
        await T.tab('Agent');
        await page.getByRole('button', { name: 'Update to v2.0.0' }).waitFor();
        if (process.env.E2E_SHOTS) await page.screenshot({ path: `${process.env.E2E_SHOTS}/published-agent-update.png` });
        await T.press('Update to v2.0.0');
        await T.toast('Update queued').waitFor();
        assert.equal(T.db("select count(*) from agent_commands c join nodes n on n.id=c.node_id where n.name='VM3' and c.type='update_agent' and c.version='2.0.0'"), '1');
      } finally {
        T.db("update nodes set agent='speedtest-agent/2.0.0' where name='VM3'");
      }
    },
    async listSortsAndOpens() {
      await T.open('#/nodes', 'Nodes');
      for (const name of ['VM1', 'VM2', 'VM3', 'VM4', 'VM5']) await T.row(name).waitFor();
      const header = page.getByRole('columnheader', { name: 'Node' });
      await header.click();
      await T.expectNoLoadError();
      await T.row('VM2').click();
      await page.getByRole('heading', { name: /VM2/, level: 1 }).waitFor();
    },

    // The wizard with the one-line command, a real agent that enrols with the shown key, the first result, a manual run.
    async addWithCommandAndRunTest() {
      T.db(`insert into notification_event_settings(event_key,enabled,severity,channels)
        values ('test.success',true,'info',ARRAY['webhook']),('test.manual_finished',true,'info',ARRAY['webhook'])
        on conflict (event_key) do update set enabled=excluded.enabled,severity=excluded.severity,channels=excluded.channels`);
      await T.open('#/settings', 'Settings');
      await T.tab('Notifications');
      await T.fillAndLeave('URL for Webhook', receiver.url);
      await T.toast('Saved').waitFor();
      await T.settled();
      await page.getByLabel('Enable Webhook').check({ force: true });
      await T.settled();
      const receivedBefore = receiver.bodies.length;
      await T.open('#/nodes', 'Nodes');
      await T.press('Add a node');
      const dialog = T.dialog('Add a node');
      await T.fill('Node name', 'E2E-1', dialog);
      await T.fill('What it measures', 'End to end test line', dialog);
      await T.fill('Expected download', '100', dialog);
      await T.fill('Expected upload', '50', dialog);
      await T.press('Continue', dialog);
      await T.press(/I will run a command/, dialog);
      const command = await dialog.locator('pre, code, .code').first().innerText();
      const token = /--token\s+(\S+)/.exec(command)?.[1];
      assert.ok(token && token.length > 20, 'the wizard shows a real one-time enrolment key: ' + command.slice(0, 80));
      assert.match(command, /install\.sh/);
      await T.press('I ran the command', dialog);
      const agent = await startAgent(stack, { name: 'E2E-1', token });
      await T.see('E2E-1 is online', dialog, 90000);
      await submitScheduledResult(stack, { name: 'E2E-1' });
      await dialog.getByText(/First result: .* down, .* up, .* ping\./).waitFor({ timeout: 90000 });
      const schedules = T.db("select s.interval_minutes||':'||s.offset_minutes||':'||e.interval_minutes||':'||e.offset_minutes from schedules s join nodes n on n.id=s.node_id join engine_schedules e on e.node_id=n.id and e.engine='ookla' where n.name='E2E-1'");
      assert.equal(schedules, '60:0:60:0', 'both center schedule tables start at 60 minutes and offset 0');
      const agentConfig = await fetchAgentConfig(stack, 'E2E-1');
      assert.equal(agentConfig.schedule.interval, 60, 'the enrolled agent receives the 60-minute schedule');
      assert.equal(agentConfig.schedule.offset, 0, 'the enrolled agent receives offset 0');
      const firstResultManual = T.db("select r.manual from results r join nodes n on n.id=r.node_id where n.name='E2E-1' order by r.ts asc limit 1") === 't';
      const firstEventKey = firstResultManual ? 'test.manual_finished' : 'test.success';
      await T.eventually(() => Number(T.db(`select count(*) from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='${firstEventKey}'`)) > 0, 'the first agent result creates its matching notification event');

      await submitScheduledResult(stack, { name: 'E2E-1' });
      await T.eventually(() => Number(T.db("select count(*) from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.success'")) > 0, 'a scheduled successful test creates a notification event');
      const successPayload = JSON.parse(T.db("select e.payload::text from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.success' order by e.ts desc limit 1"));
      for (const field of ['download', 'upload', 'ping', 'jitter', 'loss', 'download_limit', 'upload_limit', 'ping_limit', 'jitter_limit', 'loss_limit']) {
        assert.ok(Object.hasOwn(successPayload, field), `success notification includes ${field}`);
      }
      await T.eventually(() => {
        const deliveryStates = T.db("select coalesce(string_agg(d.status||':'||d.attempts,','),'none') from notification_deliveries d join alert_events e on e.id=d.event_id join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.success' and d.channel='webhook'");
        const received = receiver.bodies.slice(receivedBefore).some(message => /Test success/.test(message.text || ''));
        return received || `deliveries=${deliveryStates}; receiver=${receiver.bodies.length - receivedBefore}`;
      }, 'success notification reaches the configured webhook', 75000);
      const successMessage = receiver.bodies.slice(receivedBefore).map(message => message.text || '').find(message => message.includes('Test success'));
      for (const metric of ['Download', 'upload', 'ping', 'jitter', 'loss']) assert.ok(successMessage.includes(metric), `webhook message lists ${metric}`);
      await T.press('Open node', dialog);
      await page.getByRole('heading', { name: /E2E-1/, level: 1 }).waitFor();

      await T.press('Run test');
      const run = T.dialog('Running speed test');
      await run.waitFor();
      await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
      await T.press('Close', run.locator('.modal-foot'));
      const stored = T.db("select count(*) from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.manual");
      assert.ok(Number(stored) >= 1, 'the manual result is stored on the center: ' + stored);
      await T.eventually(() => Number(T.db("select count(*) from alert_events e join nodes n on n.id=e.node_id where n.name='E2E-1' and e.event_key='test.manual_finished'")) >= 1, 'manual result creates a finished-test event');
      await T.eventually(() => receiver.bodies.slice(receivedBefore).some(message => /Manual test finished/.test(message.text || '')), 'manual completion notification reaches the configured webhook', 75000);

      const nodeID = T.db("select id::text from nodes where name='E2E-1'");
      // Reuse the global failed-test rule seeded for the suite instead of adding a duplicate rule.
      const recoveryRule = '20000000-0000-4000-8000-000000000002';
      const offlineRule = '70000000-0000-4000-8000-000000000002';
      T.db(`INSERT INTO alert_rules(id,name,metric,op,value,for_tests,severity,channels,enabled)
        VALUES('${offlineRule}','E2E node offline','offline','=',1,1,'crit',ARRAY['webhook'],true)`);
      T.db(`INSERT INTO notification_event_settings(event_key,enabled,severity,channels)
        VALUES ('test.failed',true,'warn',ARRAY['webhook']),('test.recovered',true,'info',ARRAY['webhook']),
          ('node.offline',true,'crit',ARRAY['webhook']),('node.online',true,'info',ARRAY['webhook'])
        ON CONFLICT (event_key) DO UPDATE SET enabled=excluded.enabled,severity=excluded.severity,channels=excluded.channels`);
      const recoveryBefore = receiver.bodies.length;
      await submitFailedResult(stack, { name: 'E2E-1' });
      await submitFailedResult(stack, { name: 'E2E-1' });
      await T.eventually(() => Number(T.db(`select count(*) from alert_events where rule_id='${recoveryRule}' and event_key='test.failed'`)) === 1, 'two failed tests open one failed-test alert');
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Test failed/.test(message.text || '')), 'failed-test notification reaches the webhook', 75000);
      await submitScheduledResult(stack, { name: 'E2E-1' });
      await T.eventually(() => {
        const count = Number(T.db(`select count(*) from alert_events where node_id='${nodeID}' and event_key='test.recovered'`));
        return count === 1 || JSON.stringify({ count, state: T.db(`select consecutive_count||':'||active||':'||COALESCE(active_event_id::text,'')||':'||COALESCE(last_evaluated_at::text,'') from alert_state where rule_id='${recoveryRule}' and node_id='${nodeID}'`), results: T.db(`select status||':'||ts::text from results where node_id='${nodeID}' order by ts desc limit 4`) });
      }, 'the next successful test creates a recovery event');
      const recovered = JSON.parse(T.db(`select payload::text from alert_events where node_id='${nodeID}' and event_key='test.recovered' order by ts desc limit 1`));
      assert.ok(recovered.duration, 'the test recovery includes the failed streak duration');
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Tests recovered/.test(message.text || '')), 'test recovery notification reaches the webhook', 75000);

      const stopped = new Promise(resolve => agent.once('exit', resolve));
      agent.kill('SIGTERM');
      await stopped;
      T.db(`UPDATE engine_schedules SET interval_minutes=5 WHERE node_id='${nodeID}' AND engine='ookla';
        UPDATE schedules SET interval_minutes=5 WHERE node_id='${nodeID}';
        UPDATE nodes SET last_seen_at=now()-interval '16 minutes' WHERE id='${nodeID}'`);
      await T.eventually(() => Number(T.db(`select count(*) from alert_events where node_id='${nodeID}' and event_key='node.offline'`)) === 1, 'three missed intervals open one node-offline event', 45000);
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Node offline/.test(message.text || '')), 'node-offline notification reaches the webhook', 75000);
      await sendAgentHeartbeat(stack, { name: 'E2E-1' });
      await T.eventually(() => Number(T.db(`select count(*) from alert_events where node_id='${nodeID}' and event_key='node.online'`)) === 1, 'the restored heartbeat creates a node-online recovery event', 45000);
      const online = JSON.parse(T.db(`select payload::text from alert_events where node_id='${nodeID}' and event_key='node.online' order by ts desc limit 1`));
      assert.ok(online.duration, 'the node recovery includes its downtime');
      await T.eventually(() => receiver.bodies.slice(recoveryBefore).some(message => /Node back online/.test(message.text || '')), 'node-online notification reaches the webhook', 75000);

      await T.open('#/history', 'Results');
      await T.fill('Search results', 'E2E-1');
      await page.getByRole('row').nth(1).waitFor();
      // The same line cannot be enrolled twice with the same one-time key.
      const again = await page.request.post(stack.origin + '/api/agent/enroll', { data: { token, name: 'E2E-copy', agent: 'x' } });
      assert.ok(again.status() >= 400, 'an enrolment key works once, got ' + again.status());
    },

    async demoNodeDefault() {
      await page.goto(stack.origin + '/?demo=1#/nodes');
      await page.getByRole('heading', { name: 'Nodes' }).waitFor();
      await T.press('Add a node');
      const dialog = T.dialog('Add a node');
      await T.fill('Node name', 'Demo default');
      await T.fill('What it measures', 'Demo default schedule');
      await T.fill('Expected download', '100');
      await T.fill('Expected upload', '50');
      await T.press('Continue', dialog);
      await T.press(/I will run a command/, dialog);
      await T.press('I ran the command', dialog);
      await dialog.getByRole('heading', { name: 'Demo default is online' }).waitFor({ timeout: 20000 });
      assert.ok(await dialog.getByText('It will now test every 60 minutes.').isVisible());
      const schedule = await page.evaluate(() => {
        const node = ST.nodes().find(value => value.name === 'Demo default');
        return node && { interval: node.interval, offset: node.offset };
      });
      assert.deepEqual(schedule, { interval: 60, offset: 0 });
      await page.goto(stack.origin + '/?demo=1#/schedules');
      await T.heading('Schedules').waitFor();
      assert.equal(await T.row('Demo default').locator('select').inputValue(), '60');
      assert.equal(await T.row('Demo default').getByRole('spinbutton').inputValue(), '0');
    },

    // Wizard validation: nothing is created from an incomplete or duplicate form.
    // On a phone the list is cards (no sideways scrolling) and a card still opens the node.
    async cardsOnPhone() {
      const original = page.viewportSize();
      await page.setViewportSize({ width: 390, height: 844 });
      await T.open('#/nodes', 'Nodes');
      await T.settled();
      const overflow = await page.evaluate(() => { const w = document.querySelector('.table-wrap'); return w.scrollWidth - w.clientWidth; });
      assert.ok(overflow <= 1, `the node list scrolls sideways by ${overflow}px`);
      assert.equal(await page.locator('table.data thead').evaluate(e => getComputedStyle(e).position), 'absolute', 'the header row is hidden on phones');
      const card = T.row('VM1');
      for (const label of ['Status', 'Public IP']) {
        const cell = card.locator(`td[data-label="${label}"]`);
        await cell.waitFor();
        assert.equal(await cell.evaluate(e => getComputedStyle(e, '::before').content), `"${label}"`, `each value is captioned with its column (${label})`);
      }
      await card.click();
      await page.getByRole('heading', { name: /VM1/, level: 1 }).waitFor();
      await page.setViewportSize(original);
    },

    async wizardValidation() {
      await T.open('#/nodes', 'Nodes');
      await T.press('Add a node');
      const dialog = T.dialog('Add a node');
      await T.press('Continue', dialog);
      await dialog.getByText(/name/i).first().waitFor();
      await T.fill('Node name', 'VM1', dialog);
      await T.fill('Expected download', '100', dialog);
      await T.fill('Expected upload', '50', dialog);
      await T.press('Continue', dialog);
      await T.see('A node with this name already exists', dialog);
      T.expectRefusal(409);
      await T.press('Cancel', dialog);
      assert.equal(T.db("select count(*) from nodes where name='VM1'"), '1', 'a duplicate name creates nothing');
    },

    // Node detail: overview, settings edit (validation then save), agent tab.
    async detailAndSettings() {
      await T.open('#/nodes', 'Nodes');
      await T.row('VM3').click();
      await page.getByRole('heading', { name: /VM3/, level: 1 }).waitFor();
      await T.see('Expected');
      await T.tab('Settings');
      await T.fill('Node name', '', page);
      await T.press('Save changes');
      await T.toast('Node name cannot be empty').waitFor();
      await T.fill('Node name', 'VM3', page);
      await T.fill('What does it measure?', 'VNPT edited', page);
      await T.fill('Expected download', '0');
      await T.press('Save changes');
      await T.toast('Expected speeds must be above zero').waitFor();
      await T.fill('Expected download', '320');
      await T.press('Save changes');
      await T.toast('Settings saved').waitFor();
      assert.equal(T.db("select expected_down_mbps::int || ' ' || label from nodes where name='VM3'"), '320 VNPT edited');
      // A name that another node already uses is refused.
      await T.fill('Node name', 'VM1');
      await T.press('Save changes');
      await T.toast('Another node already has this name.').waitFor();
      T.expectRefusal(409);
      await T.press('Discard');
      await T.tab('Agent');
      await T.see('Program on the machine');
      await T.tab('Overview');
      await T.expectNoLoadError();
    },

    async pauseAndResume() {
      await T.open('#/nodes', 'Nodes');
      await T.row('VM4').click();
      await page.getByRole('heading', { name: /VM4/, level: 1 }).waitFor();
      await T.press('Pause');
      await T.toast('paused').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select enabled from nodes where name='VM4'") === 'f', 'the pause is stored');
      await T.eventually(async () => await page.getByRole('button', { name: 'Run test' }).isDisabled(), 'a paused node cannot be tested');
      await T.press('Resume');
      await T.toast('resumed').waitFor();
      await T.settled();
      await T.eventually(() => T.db("select enabled from nodes where name='VM4'") === 't', 'the resume is stored');
    },

    // Removing a node from the dashboard only (no SSH): the node and its results disappear.
    async removeFromDashboard(name) {
      await T.open('#/nodes', 'Nodes');
      await T.row(name).click();
      await page.getByRole('heading', { name: new RegExp(name), level: 1 }).waitFor();
      await T.tab('Settings');
      await T.press(new RegExp('^Remove ' + name));
      const removal = T.dialog('Remove ' + name);
      await T.press(/Only remove from the dashboard/, removal);
      await T.see('sudo speedtest-agent uninstall --all', removal);
      await T.press(new RegExp('^Remove ' + name), removal);
      await T.heading('Nodes').waitFor();
      assert.equal(T.db(`select count(*) from nodes where name='${name}'`), '0', 'the node is gone from the center');
    },
  };
}
