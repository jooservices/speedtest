export function nodesSuite(T, ctx) {
  const { page, assert } = T;
  const { stack, startAgent } = ctx;
  return {
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
      await startAgent(stack, { name: 'E2E-1', token });
      await T.see('E2E-1 is online', dialog, 90000);
      await dialog.getByText(/First result: .* down, .* up, .* ping\./).waitFor({ timeout: 90000 });
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
      await T.fill('Search results', 'E2E-1');
      await page.getByRole('row').nth(1).waitFor();
      // The same line cannot be enrolled twice with the same one-time key.
      const again = await page.request.post(stack.origin + '/api/agent/enroll', { data: { token, name: 'E2E-copy', agent: 'x' } });
      assert.ok(again.status() >= 400, 'an enrolment key works once, got ' + again.status());
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
      assert.equal(T.db("select enabled from nodes where name='VM4'"), 'f');
      await T.eventually(async () => await page.getByRole('button', { name: 'Run test' }).isDisabled(), 'a paused node cannot be tested');
      await T.press('Resume');
      await T.toast('resumed').waitFor();
      await T.settled();
      assert.equal(T.db("select enabled from nodes where name='VM4'"), 't');
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
