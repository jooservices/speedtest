const routePattern = '**/api/notifications/deliveries**';
const prefix = 'd1260000-0000-4000-8000-000000000';
const fixtures = [
  ['slack', 'pending', 0], ['telegram', 'sent', 1], ['email', 'sent', 1], ['webhook', 'failed', 2],
  ['slack', 'sent', 1], ['telegram', 'failed', 3], ['email', 'failed', 1], ['webhook', 'pending', 0],
  ['slack', 'failed', 2], ['telegram', 'sent', 1], ['email', 'failed', 2], ['webhook', 'failed', 3]
];

function values() {
  return fixtures.map(([channel, status, attempts], index) => {
    const number = index + 1;
    const id = prefix + String(number).padStart(3, '0');
    const time = `now() + interval '1 hour' + interval '${number} minutes'`;
    const error = status === 'failed' ? `'safe failure detail ${number}'` : 'NULL';
    const lastAttempt = status === 'pending' ? 'NULL' : time;
    return `('${id}',NULL,'${channel}','${status}',${attempts},${time},${lastAttempt},${error})`;
  }).join(',');
}

export function deliveryHistorySteps(T) {
  const { page } = T;
  return {
    seed() {
      T.db(`DELETE FROM notification_deliveries WHERE id::text LIKE '${prefix}%'; INSERT INTO notification_deliveries(id,event_id,channel,status,attempts,created_at,last_attempt,error) VALUES ${values()}`);
    },
    cleanup() { T.db(`DELETE FROM notification_deliveries WHERE id::text LIKE '${prefix}%';`); },
    async open() {
      await T.open('#/settings', 'Settings');
      await T.tab('Delivery history');
      await page.getByTestId('delivery-history').waitFor();
    },
    loadingState() { return page.getByTestId('delivery-history-results').getByText('Loading delivery history…'); },
    results() { return page.getByTestId('delivery-history-results'); },
    rows() { return page.getByTestId('delivery-history-results').getByRole('row'); },
    pageStatus() { return page.getByRole('status').filter({ hasText: /^Page / }); },
    async waitForPage(number) { await page.getByRole('status').filter({ hasText: new RegExp(`^Page ${number} of `) }).waitFor(); },
    async status(value) { await page.getByLabel('Status', { exact: true }).selectOption(value); },
    async statusDisabled() { return page.getByLabel('Status', { exact: true }).getAttribute('aria-disabled'); },
    async channel(value) { await page.getByLabel('Channel', { exact: true }).selectOption(value); },
    async previous() { await page.getByRole('button', { name: 'Previous delivery history page' }).click(); },
    async clearFilters() { await page.getByRole('button', { name: 'Clear filters' }).click(); },
    async retry() { await page.getByRole('button', { name: 'Retry' }).click(); },
    async focusStatus() { await page.getByLabel('Status', { exact: true }).focus(); },
    async tabKey() { await page.keyboard.press('Tab'); },
    async enterKey() { await page.keyboard.press('Enter'); },
    async focusNext() { await page.getByRole('button', { name: 'Next delivery history page' }).focus(); },
    async activePaginationName() {
      return page.evaluate(() => document.activeElement.getAttribute('aria-label'));
    },
    async retryFocused() {
      return page.getByRole('button', { name: 'Retry' }).evaluate(element => element === document.activeElement);
    },
    async activeName() {
      return page.evaluate(() => {
        const active = document.activeElement;
        return active.getAttribute('aria-label') || (active.labels && active.labels[0] && active.labels[0].textContent.trim()) || active.textContent.trim();
      });
    },
    async blockNextList() {
      let started;
      let release;
      let begin;
      let unblock;
      const requestStarted = new Promise(resolve => { begin = resolve; });
      const gate = new Promise(resolve => { unblock = resolve; });
      let pending = true;
      const handler = async route => {
        if (!pending) return route.continue();
        pending = false;
        begin();
        const response = await route.fetch();
        await gate;
        await route.fulfill({ response });
      };
      await page.route(routePattern, handler);
      started = requestStarted;
      release = () => unblock();
      return {
        started,
        release,
        async remove() { release(); await page.unroute(routePattern, handler); }
      };
    },
    async failNextList() {
      const requests = [];
      const handler = route => {
        requests.push(new URL(route.request().url()));
        return route.fulfill({
          status: 500,
          contentType: 'application/problem+json',
          body: JSON.stringify({ type: 'about:blank', title: 'Failure', status: 500,
            detail: 'provider response canary; message body canary; destination-secret-canary' })
        });
      };
      await page.route(routePattern, handler);
      const remove = () => page.unroute(routePattern, handler);
      remove.requests = requests;
      return remove;
    },
    async injectMarkupIntoNextFailure() {
      const markup = '<img src=x data-delivery-injected="true">';
      const handler = async route => {
        const response = await route.fetch();
        const body = await response.json();
        if (body.data.length) body.data[0].error = markup;
        await route.fulfill({ response, body: JSON.stringify(body) });
      };
      await page.route(routePattern, handler);
      return { markup, remove: () => page.unroute(routePattern, handler) };
    },
    async setViewport(width) {
      const current = page.viewportSize();
      await page.setViewportSize({ width, height: current.height });
    },
    async restoreViewport() {
      await page.setViewportSize({ width: 1440, height: 900 });
    },
    async layout() {
      return page.evaluate(() => {
        const width = document.documentElement.clientWidth;
        const overflow = [...document.querySelectorAll('#view *')].filter(element => {
          const box = element.getBoundingClientRect();
          return box.width && box.right > width + 1 && !element.closest('.tabs, .table-wrap, pre');
        }).map(element => `${element.tagName}.${element.className}`);
        return {
          pageOverflow: document.documentElement.scrollWidth - width,
          overflow,
          tableDisplay: getComputedStyle(document.querySelector('table.data')).display
        };
      });
    }
  };
}
