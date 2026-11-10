import { readFileSync } from 'node:fs';

const ROUTES = [['#/', 'Dashboard'], ['#/nodes', 'Nodes'], ['#/history', 'Results'], ['#/stats', 'Statistics'], ['#/network', 'IP & location'], ['#/schedules', 'Schedules'], ['#/alerts', 'Alerts'], ['#/settings', 'Settings']];

export function qualitySuite(T) {
  const { page, assert } = T;
  return {
    async everyPageDraws() {
      for (const [hash, title] of ROUTES.slice(1)) {
        await T.open(hash, title);
        await page.waitForLoadState('networkidle');
        await T.expectNoLoadError();
      }
    },

    // Every page, both themes, phone/tablet/desktop width: nothing overflows the viewport or leaves a console error.
    async layoutHolds() {
      const original = page.viewportSize();
      for (const theme of ['dark', 'light']) {
        await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);
        for (const width of [390, 768, 1440]) {
          await page.setViewportSize({ width, height: 900 });
          for (const [hash, title] of ROUTES) {
            await T.open(hash, title);
            await page.evaluate(t => document.documentElement.setAttribute('data-theme', t), theme);
            await page.evaluate(() => new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done))));
            if (process.env.E2E_SHOTS) { await page.evaluate(() => new Promise(r => setTimeout(r, 700))); await page.screenshot({ path: `${process.env.E2E_SHOTS}/${theme}-${width}-${title.replaceAll(/\W+/g, '_')}.png`, fullPage: false }); }
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

    // On a phone the top bar stays one row, keeps "Run all tests" and the page does not scroll sideways;
    // download buttons carry the download icon, not the info icon.
    async phoneTopBar() {
      const original = page.viewportSize();
      try {
        await page.setViewportSize({ width: 390, height: 844 });
        await T.open('#/', 'Dashboard');
        const box = await page.locator('#topbar').boundingBox();
        assert.ok(box.height <= 64, `the top bar must fit on one row on a phone (was ${box.height}px)`);
        assert.ok(await page.getByRole('button', { name: 'Run all tests' }).isVisible(), 'Run all tests must stay visible on a phone');
        const sideways = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert.ok(sideways <= 1, `the page scrolls sideways by ${sideways}px on a phone`);
        const signOut = await page.getByRole('button', { name: 'Sign out' }).boundingBox();
        assert.ok(signOut && signOut.width >= 20, 'Sign out must stay visible (as an icon) on a phone');
        await T.open('#/history', 'Results');
        const path = await page.getByRole('button', { name: 'Export CSV' }).locator('svg path').first().getAttribute('d');
        assert.ok(path.startsWith('M12 3v12'), `Export CSV must use the download icon (path was ${path})`);
      } finally {
        await page.setViewportSize(original);
      }
    },

    // The top-bar button flips the theme and the choice survives a reload (stored on the center).
    async themeSwitch() {
      await T.open('#/', 'Dashboard');
      const theme = () => page.evaluate(() => document.documentElement.getAttribute('data-theme'));
      const start = await theme();
      await T.press('Toggle theme');
      await page.waitForFunction(t => document.documentElement.getAttribute('data-theme') !== t, start);
      const flipped = await theme();
      assert.notEqual(flipped, start);
      await T.settled();
      await page.reload();
      await T.heading('Dashboard').waitFor();
      assert.equal(await theme(), flipped, 'the theme survives a reload');
      await T.press('Toggle theme');
      await page.waitForFunction(t => document.documentElement.getAttribute('data-theme') !== t, flipped);
      await T.settled();
    },

    // Keyboard and screen-reader basics: controls have names, the keyboard reaches them, exactly one h1.
    async accessibility() {
      for (const [hash, title] of ROUTES) {
        await T.open(hash, title);
        await page.waitForLoadState('networkidle');
        await page.addScriptTag({ url: new URL('/__e2e_axe.js', T.url(hash)).href });
        const violations = await page.evaluate(async () => {
          const result = await window.axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'] } });
          return result.violations.map(rule => ({ id: rule.id, help: rule.help, targets: rule.nodes.map(node => ({ target: node.target, summary: node.failureSummary })) }));
        });
        assert.deepEqual(violations, [], `${title}: axe WCAG 2.2 AA violations`);
        const found = await page.evaluate(() => {
          const out = [];
          const name = el => (el.getAttribute('aria-label') || el.getAttribute('aria-labelledby') || el.getAttribute('title') || el.textContent || '').trim()
            || (el.labels && el.labels.length ? 'label' : '') || (el.querySelector && el.querySelector('img[alt]:not([alt=""])') ? 'img' : '');
          for (const el of document.querySelectorAll('button, a[href], input:not([type=hidden]), select, textarea, [role=button], [role=tab]')) {
            if (el.closest('.leaflet-control-container, [hidden]')) continue;
            if (!name(el)) out.push(`${el.tagName.toLowerCase()}.${el.className} has no accessible name`);
          }
          if (document.querySelectorAll('h1').length !== 1) out.push(`${document.querySelectorAll('h1').length} h1 elements`);
          if (!document.documentElement.lang) out.push('html has no lang');
          return out;
        });
        assert.deepEqual(found, [], `${title}: ${found.join('; ')}`);
        await page.keyboard.press('Tab'); await page.keyboard.press('Tab'); await page.keyboard.press('Tab');
        const focused = await page.evaluate(() => ({ tag: document.activeElement && document.activeElement.tagName,
          visible: !!document.activeElement && document.activeElement.matches(':focus-visible'), outline: getComputedStyle(document.activeElement).outlineStyle }));
        assert.ok(focused.tag && focused.tag !== 'BODY', `${title}: Tab does not move focus into the page`);
        assert.ok(focused.visible && focused.outline !== 'none', `${title}: keyboard focus is not visible`);
      }
    },
  };
}
