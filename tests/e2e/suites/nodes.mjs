async function runManual(T, name, message) {
  const { page } = T;
  await T.open('#/nodes', 'Nodes');
  const row = T.row(name);
  try {
    await row.waitFor({ timeout: 10000 });
  } catch (error) {
    const exists = T.db(`SELECT count(*) FROM nodes WHERE name='${name}'`);
    throw new Error(`node row ${name} did not render at ${page.url()} (database rows: ${exists}): ${error.message}`);
  }
  await row.click();
  await page.getByRole('heading', { name: new RegExp(name), level: 1 }).waitFor();
  await T.press('Run test');
  const dialog = T.dialog('Running speed test');
  await dialog.waitFor();
  await dialog.getByText(message).waitFor({ timeout: 90000 });
  await T.press('Close', dialog.locator('.modal-foot'));
}

async function restartAgentAndWait(T, agent, name, scenario = 'rate_limited') {
  const since = T.db(`SELECT last_seen_at::text FROM nodes WHERE name='${name}'`);
  await agent.restart({ scenario });
  await T.eventually(() => T.db(`SELECT (last_seen_at > '${since}'::timestamptz)::int FROM nodes WHERE name='${name}'`) === '1', 'the restarted rate-limited agent checked in');
}

async function verifyRateLimitedAgent(T, agent, name) {
  const { assert } = T;
  T.db("UPDATE alert_rules SET for_tests=1 WHERE metric='failed'");
  try {
    await restartAgentAndWait(T, agent, name);
    await runManual(T, name, /Failed: speedtest: rate_limited/);
    const stored = T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.failure_class='rate_limited' AND r.manual`);
    assert.equal(stored, '1', 'the real Ookla error is spooled, ingested and classified');
    const alerts = T.db(`SELECT count(*) FROM alert_events e JOIN nodes n ON n.id=e.node_id JOIN alert_rules r ON r.id=e.rule_id WHERE n.name='${name}' AND r.metric='failed'`);
    assert.equal(alerts, '0', 'rate-limited ingestion does not trigger a one-test failure alert');
    await restartAgentAndWait(T, agent, name);
    await runManual(T, name, /Failed: Test deferred: ookla provider rate-limit backoff is active until/);
    assert.equal(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.failure_class='rate_limited' AND r.manual`), '1', 'restart preserves the deadline and prevents a second engine attempt');
    await runManual(T, name, /Failed: Test deferred: ookla provider rate-limit backoff is active until/);
    assert.equal(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.failure_class='rate_limited' AND r.manual`), '1', 'repeated manual requests remain deferred without another rate-limit result');
    const successfulManuals = Number(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.status='ok' AND r.manual`));
    await new Promise(resolve => setTimeout(resolve, 31_000));
    await agent.restart({ scenario: 'success' });
    await runManual(T, name, /Mbps ↓|Gbps ↓|kbps ↓/);
    assert.equal(Number(T.db(`SELECT count(*) FROM results r JOIN nodes n ON n.id=r.node_id WHERE n.name='${name}' AND r.status='ok' AND r.manual`)), successfulManuals + 1, 'the real agent resumes successful measurements after the deadline');
  } finally {
    T.db("UPDATE alert_rules SET for_tests=2 WHERE metric='failed'");
  }
}

export function nodesSuite(T, ctx) {
  const { page, assert } = T;
  const { stack, startAgent, fetchAgentConfig } = ctx;
  async function pinOoklaAndCheckFallback() {
    await T.press('Refresh server list');
    await T.row('Gateway Central').waitFor({ timeout: 90000 });
    await page.getByRole('columnheader', { name: 'Distance' }).waitFor();
    await assertPickerTableFits(page.locator('.setting').filter({ hasText: 'Search Ookla servers' }).first(), 'Ookla');
    const search = 'Search Ookla servers';
    await T.fill(search, 'Gateway');
    const server = T.row('Gateway Central');
    await server.waitFor();
    await T.fill(search, 'Hanoi');
    await server.waitFor();
    await T.fill(search, 'Ookla ISP');
    await server.waitFor();
    await server.getByRole('button', { name: 'Pin' }).click();
    await T.toast('Ookla server selection saved').waitFor();
    await T.press('Use automatic selection');
    await T.toast('Ookla server selection saved').waitFor();
    await server.getByRole('button', { name: 'Pin' }).click();
    await T.toast('Ookla server selection saved').waitFor();
    assert.equal(T.db("select pinned_server_id from node_engine_servers c join nodes n on n.id=c.node_id where n.name='E2E-1' and c.engine='ookla'"), '54321');
    await T.press('Run test');
    const run = T.dialog('Running speed test');
    await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
    await T.press('Close', run.locator('.modal-foot'));
    const saved = T.db("select r.server_id::text||':'||r.warnings::text from results r join nodes n on n.id=r.node_id where n.name='E2E-1' order by r.ts desc limit 1");
    assert.match(saved, /^12345:.*Pinned server was unreachable/);
    await T.open('#/history', 'Results');
    await T.fill('Search results', 'E2E-1');
    const resultRow = page.getByRole('row').filter({ hasText: 'Different from pinned' }).first();
    await resultRow.waitFor();
    await resultRow.click();
    await page.getByText(/instead of pinned server #54321/).waitFor();
    await page.getByText(/Pinned server was unreachable/).waitFor();
    await page.locator('dd').filter({ hasText: /Singapore Gateway, Singapore \(#12345\)/ }).waitFor();
  }

  async function pinLibreSpeedAndCheckPicker() {
    await T.open('#/nodes', 'Nodes');
    await T.row('E2E-1').click();
    await T.tab('Settings');
    const libre = page.locator('.setting').filter({ hasText: 'LibreSpeed' }).first();
    await libre.getByRole('button', { name: 'Refresh server list' }).click();
    const libreRows = libre.locator('tbody tr');
    await libreRows.nth(1).waitFor({ timeout: 90000 });
    await assertPickerTableFits(libre, 'LibreSpeed');
    assert.match(await libreRows.nth(0).innerText(), /Hanoi/);
    assert.match(await libreRows.nth(1).innerText(), /Singapore/);
    assert.equal(await libre.getByRole('columnheader', { name: 'Distance' }).count(), 0, 'LibreSpeed has no invented distance column');
    await libre.getByRole('row').filter({ hasText: 'Singapore' }).getByRole('button', { name: 'Pin' }).click();
    await T.toast('LibreSpeed server selection saved').waitFor();
    await T.toast('LibreSpeed server selection saved').waitFor({ state: 'hidden' });
    assert.equal(T.db("select pinned_server_id from node_engine_servers c join nodes n on n.id=c.node_id where n.name='E2E-1' and c.engine='librespeed'"), '52');
    const enabled = page.getByRole('checkbox', { name: 'LibreSpeed enabled' });
    if (!(await enabled.isChecked())) await enabled.click();
    await T.press('Save engine schedules');
    await T.toast('Engine schedules saved').waitFor();
    assert.equal(T.db("select s.enabled from engine_schedules s join nodes n on n.id=s.node_id where n.name='E2E-1' and s.engine='librespeed'"), 't');
    return libre;
  }

  async function runLibreSpeedFallbackAndCheckHistory() {
    T.db("update nodes set primary_engine='librespeed',server_id=52,config_revision=config_revision+1 where name='E2E-1'");
    await T.press('Run test');
    const run = T.dialog('Running speed test');
    await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
    await T.press('Close', run.locator('.modal-foot'));
    const saved = T.db("select r.server_id::text||':'||r.pinned_server_id_at_test::text||':'||coalesce(r.server->>'host','')||':'||coalesce(r.server->>'endpoint','')||':'||r.server_selection_known::text||':'||r.warnings::text from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.engine='librespeed' and r.manual order by r.ts desc limit 1");
    assert.match(saved, /^51:52:hn\.example\.test:https:\/\/hn\.example\.test\/backend:true:.*Pinned server was unreachable/);
    await T.open('#/history', 'Results');
    await T.fill('Search results', 'E2E-1');
    const resultRow = page.getByRole('row').filter({ hasText: 'Different from pinned' }).first();
    await resultRow.waitFor();
    await resultRow.click();
    await page.getByText(/instead of pinned server #52/).waitFor();
    await page.getByText(/Pinned server was unreachable/).waitFor();
    await page.locator('dd').filter({ hasText: /Hanoi.*#51/ }).waitFor();
  }

  async function checkServerPickerResponsive(libre) {
    const original = page.viewportSize();
    for (const theme of ['dark', 'light']) {
      await page.evaluate(value => document.documentElement.setAttribute('data-theme', value), theme);
      for (const width of [390, 768, 1440]) {
        await page.setViewportSize({ width, height: 900 });
        await libre.scrollIntoViewIfNeeded();
        await page.evaluate(() => window.scrollTo(0, 0));
        await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
        const overflow = await page.evaluate(() => {
          const wide = [];
          for (const el of document.querySelectorAll('.content *')) {
            const box = el.getBoundingClientRect();
            if (box.width && box.right > document.documentElement.clientWidth + 1 && !el.closest('.table-wrap, .tabs, .seg, pre, .legend')) wide.push(`${el.tagName}.${el.className} ${Math.round(box.left)}-${Math.round(box.right)} width=${Math.round(box.width)} min=${getComputedStyle(el).minWidth}`);
          }
          return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, x: scrollX, wide: wide.slice(0, 5) };
        });
        assert.ok(overflow.page <= 1, `node server settings overflow at ${width}px (${theme}): ${JSON.stringify(overflow)}`);
        assert.deepEqual(overflow.wide, [], `node server settings elements overflow at ${width}px (${theme})`);
        if (width >= 768) await assertPickerTableFits(libre, 'LibreSpeed');
        if (width <= 900) {
          const menu = await page.locator('.menu-btn').boundingBox();
          assert.ok(menu && menu.x >= 0 && menu.x + menu.width <= width, `mobile menu is outside the viewport at ${width}px (${theme}): ${JSON.stringify(menu)}`);
        }
        if (process.env.E2E_SHOTS) await page.screenshot({ path: `${process.env.E2E_SHOTS}/server-selection-${theme}-${width}.png`, fullPage: true });
      }
    }
    await page.setViewportSize(original);
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
  }

  async function assertPickerTableFits(section, label) {
    const table = section.locator('.table-wrap').first();
    const size = await table.evaluate(el => ({ client: el.clientWidth, scroll: el.scrollWidth }));
    assert.ok(size.client > 0 && size.scroll <= size.client + 1, `${label} server table clips at desktop width: ${JSON.stringify(size)}`);
  }
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
      const agent = await startAgent(stack, { name: 'E2E-1', token, backoffInitial: '30s', backoffMax: '30s' });
      await T.see('E2E-1 is online', dialog, 90000);
      await dialog.getByText(/First result: .* down, .* up, .* ping\./).waitFor({ timeout: 90000 });
      const schedules = T.db("select s.interval_minutes||':'||s.offset_minutes||':'||e.interval_minutes||':'||e.offset_minutes from schedules s join nodes n on n.id=s.node_id join engine_schedules e on e.node_id=n.id and e.engine='ookla' where n.name='E2E-1'");
      assert.equal(schedules, '60:0:60:0', 'both center schedule tables start at 60 minutes and offset 0');
      const agentConfig = await fetchAgentConfig(stack, 'E2E-1');
      assert.equal(agentConfig.schedule.interval, 60, 'the enrolled agent receives the 60-minute schedule');
      assert.equal(agentConfig.schedule.offset, 0, 'the enrolled agent receives offset 0');
      await T.press('Open node', dialog);
      await page.getByRole('heading', { name: /E2E-1/, level: 1 }).waitFor();

      await T.press('Run test');
      const run = T.dialog('Running speed test');
      await run.waitFor();
      await run.getByText(/Mbps ↓|Gbps ↓|kbps ↓/).waitFor({ timeout: 90000 });
      await T.press('Close', run.locator('.modal-foot'));
      const stored = T.db("select count(*) from results r join nodes n on n.id=r.node_id where n.name='E2E-1' and r.manual");
      assert.ok(Number(stored) >= 1, 'the manual result is stored on the center: ' + stored);
      await T.open('#/history', 'Results');
      await T.selectWith('Any status').selectOption('all');
      await T.fill('Search results', 'E2E-1');
      await page.getByRole('row').nth(1).waitFor();
      // The same line cannot be enrolled twice with the same one-time key.
      const again = await page.request.post(stack.browserOrigin + '/api/agent/enroll', { data: { token, name: 'E2E-copy', agent: 'x' } });
      assert.ok(again.status() >= 400, 'an enrolment key works once, got ' + again.status());
      await verifyRateLimitedAgent(T, agent, 'E2E-1');
    },

    async discoverPinAndFallback() {
      await T.open('#/nodes', 'Nodes');
      await T.row('E2E-1').click();
      await T.tab('Settings');
      await pinOoklaAndCheckFallback();
      await T.dismiss();
      const libre = await pinLibreSpeedAndCheckPicker();
      await checkServerPickerResponsive(libre);
      await runLibreSpeedFallbackAndCheckHistory();
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
