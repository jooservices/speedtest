import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { runtime } from './runtime.mjs';

function deliveryView() {
  const page = runtime('http://localhost', { demo: true });
  page.ST.setSettings({ timezone: 'UTC' });
  vm.runInContext(readFileSync('ui/assets/js/pages/delivery-history.js', 'utf8'), page.context);
  return page.ST.deliveryHistory.rowView;
}

test('AC1 delivery history row view formats sent and failed metadata without unrelated fields', () => {
  const rowView = deliveryView();
  const failed = rowView({
    channel: 'telegram', status: 'failed', attempts: 2, createdAt: '2026-11-17T09:08:07Z', error: 'notification attempt failed',
    eventId: 'private-event', message: 'private message', providerResponse: 'private response', credential: 'private secret'
  });

  assert.match(failed.when, /2026/);
  assert.equal(failed.channel, 'Telegram');
  assert.equal(failed.statusLabel, 'Failed');
  assert.equal(failed.statusKind, 'crit');
  assert.equal(failed.attempts, '2');
  assert.equal(failed.detail, 'notification attempt failed');
  assert.deepEqual(Object.keys(failed).sort(), ['attempts', 'channel', 'detail', 'statusKind', 'statusLabel', 'when']);

  const sent = rowView({ channel: 'email', status: 'sent', attempts: 1, createdAt: '2026-11-17T10:00:00Z', error: 'message body is not a failure detail' });
  assert.equal(sent.statusLabel, 'Sent');
  assert.equal(sent.statusKind, 'good');
  assert.equal(sent.detail, '—');
});

test('AC2 delivery history row view handles unknown or malformed metadata and bounds failure text', () => {
  const rowView = deliveryView();
  const unknown = rowView({ channel: 'unrecognized', status: 'unexpected', attempts: -1, createdAt: 'invalid', error: 'ignored' });
  assert.deepEqual({ when: unknown.when, channel: unknown.channel, statusLabel: unknown.statusLabel, attempts: unknown.attempts, detail: unknown.detail }, {
    when: '—', channel: 'Other', statusLabel: 'Unknown', attempts: '—', detail: '—'
  });
  assert.equal(rowView(null).detail, '—');
  const failed = rowView({ status: 'failed', error: '  ', attempts: 0 });
  assert.equal(failed.detail, 'No safe failure detail available.');
  assert.equal(failed.attempts, '0');
  const long = rowView({ status: 'failed', error: '<img>' + 'x'.repeat(300) });
  assert.equal(long.detail.length, 240);
  assert.ok(long.detail.startsWith('<img>'));
});
