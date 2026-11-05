export function schedulesSuite(T) {
  const { page, assert } = T;
  return {
    async intervalStaggerQuietHours() {
      await T.open('#/schedules', 'Schedules');
      await T.choose(T.row('VM1').locator('select'), '60');
      await T.toast('hour').waitFor();
      await page.reload();
      await T.heading('Schedules').waitFor();
      assert.equal(await T.row('VM1').locator('select').inputValue(), '60', 'the interval is stored on the center');
      assert.equal(T.db("select interval_minutes from schedules s join nodes n on n.id=s.node_id where n.name='VM1'"), '60');
      await T.press('Spread out evenly');
      await T.toast('Start times spread out').waitFor();
      assert.ok(Number(T.db('select count(distinct offset_minutes) from schedules')) > 1, 'staggering gives different start minutes');
      await T.settled(); // the spread-out request redraws the page; type only after that has settled
      await T.fillAndLeave('Pause from', '23:00');
      await T.fillAndLeave('Pause until', '05:00');
      await T.toast('Saved').waitFor();
      assert.equal(T.db("select value #>> '{}' from settings where key='quietStart'"), '23:00');
      // A half-typed pause window is not saved: both ends or neither.
      await T.fillAndLeave('Pause until', '');
      await T.settled();
      assert.equal(T.db("select value #>> '{}' from settings where key='quietEnd'"), '05:00', 'one empty end changes nothing');
    },

    // Start minute, the Active switch, the overlap rule and the concurrency limit, per node.
    async perNodeControls() {
      await T.open('#/schedules', 'Schedules');
      await T.settled();
      const row = T.row('VM2');
      const offset = row.getByRole('spinbutton').first();
      await offset.fill('7');
      await offset.blur();
      await T.toast('starts at minute 7').waitFor();
      assert.equal(T.db("select offset_minutes from schedules s join nodes n on n.id=s.node_id where n.name='VM2'"), '7');
      await T.settled();
      await T.row('VM2').locator('.switch').click();
      await T.toast('paused').waitFor();
      await T.settled();
      assert.equal(await T.row('VM2').getByRole('button', { name: 'Run now' }).isDisabled(), true, 'a paused node cannot run now');
      await T.row('VM2').locator('.switch').click();
      await T.toast('resumed').waitFor();
      await T.settled();
      await T.press('Run now', T.row('VM3'));
      await T.dialog('Running speed test').waitFor();
      await T.dismiss();
      const rule = page.locator('.setting').filter({ hasText: 'Keep tests from overlapping' }).locator('.switch');
      await rule.click();
      await T.toast('Tests may overlap').waitFor();
      await T.settled();
      await T.choose(page.locator('.setting').filter({ hasText: 'Maximum tests at the same time' }).locator('select'), '3');
      await T.settled();
      await T.eventually(() => T.db("select value #>> '{}' from settings where key='maxConcurrent'") === '3', 'the concurrency limit is stored')
      await page.locator('.setting').filter({ hasText: 'Keep tests from overlapping' }).locator('.switch').click();
      await T.toast('spread out').waitFor();
      await T.see('Coming up');
      await T.see('Data used by tests');
    },
  };
}
