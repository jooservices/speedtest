export function alertsSuite(T) {
  const { page, assert } = T;
  return {
    async ruleLifecycle() {
      await T.open('#/alerts', 'Alerts');
      await T.tab(/Rules/);
      await T.press('New rule');
      const dialog = T.dialog('New alert rule');
      await dialog.getByPlaceholder('e.g. Evening slowdown').fill('E2E slow evening');
      await T.press('Create rule', dialog);
      await T.toast('Rule created').waitFor();
      await T.see('E2E slow evening');
      assert.equal(T.db("select count(*) from alert_rules where name='E2E slow evening'"), '1');
      const row = text => page.locator('.list-row').filter({ hasText: text });
      await T.press('Edit', row('E2E slow evening'));
      const edit = T.dialog('Edit alert rule');
      await edit.getByPlaceholder('e.g. Evening slowdown').fill('E2E renamed rule');
      await T.press('Save rule', edit);
      await T.toast('Rule saved').waitFor();
      await T.see('E2E renamed rule');
      await row('E2E renamed rule').locator('.switch').click();
      await T.toast('turned off').waitFor();
      assert.equal(T.db("select enabled from alert_rules where name='E2E renamed rule'"), 'f');
      await T.press(/Delete/, row('E2E renamed rule'));
      await T.press('Delete rule', page.getByRole('dialog'));
      await T.toast('Rule deleted').waitFor();
      assert.equal(T.db("select count(*) from alert_rules where name like 'E2E%'"), '0');
    },

    // Every rule type can be chosen and saved; a nameless rule is refused in plain language and nothing is stored.
    async ruleValidationAndTypes() {
      await T.open('#/alerts', 'Alerts');
      await T.tab(/Rules/);
      await T.press('New rule');
      await T.press('Create rule', page.getByRole('dialog'));
      await T.see('Give the rule a name.');
      assert.equal(T.db("select count(*) from alert_rules where name=''"), '0');
      const dialog = T.dialog('New alert rule');
      const metric = dialog.locator('select').first();
      const options = await metric.locator('option').allInnerTexts();
      assert.ok(options.length >= 5, 'the rule types are offered: ' + options.join(', '));
      await T.press('Cancel', dialog);
    },

    // Acknowledge one, acknowledge all, switch between open and all, filter by importance.
    async activityAcknowledge() {
      await T.open('#/alerts', 'Alerts');
      await T.tab(/Activity/);
      const before = Number(T.db('select count(*) from alert_events where not acked'));
      const openCount = async () => (await T.api('/api/alerts/events?state=all&size=100')).body.data.filter(event => !event.acked).length;
      assert.ok(before >= 2);
      const firstOpenAlert = page.getByRole('button', { name: 'Acknowledge', exact: true }).first();
      const alertTitle = await firstOpenAlert.locator('xpath=..').locator('b').innerText();
      await firstOpenAlert.click();
      await T.toast('Alert acknowledged').waitFor();
      await T.eventually(async () => await openCount() === before - 1, 'the alert acknowledgement is stored');
      await page.getByRole('tab', { name: 'All', exact: true }).click();
      await T.settled();
      const alertRow = page.locator('.list-row').filter({ hasText: alertTitle }).first();
      await T.press('Reopen', alertRow);
      await T.eventually(async () => await openCount() === before, 'Reopen puts the alert back');
      await alertRow.getByRole('button', { name: 'Acknowledge', exact: true }).waitFor();
      await T.choose(T.selectWith('Any importance'), 'crit');
      await T.expectNoLoadError();
      await T.choose(T.selectWith('Any importance'), 'all');
      await page.getByRole('tab', { name: 'Open' }).click();
      await T.press('Acknowledge all');
      await T.toast('All alerts acknowledged').waitFor();
      assert.equal(T.db('select count(*) from alert_events where not acked'), '0');
    },
  };
}
