// An event batch (capcom-service.md 9.2): one input of kind event begins an event turn, waits at
// the event step, and reads as Flightdeck events with its blocks neutralised.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { admit, itemLines, originOf } from '../src/contract.js';

test('an event batch begins an event turn and waits at the event step', () => {
  const batch = { id: 'b1', kind: 'event', author: 'flightdeck', text: '2 event(s) from Flightdeck' };
  assert.equal(originOf(batch), 'event');
  assert.deepEqual(admit([batch], { user: 'ok', event: 'ok' }), { origin: 'event', small: false });
  assert.equal(admit([batch], { user: 'ok', event: 'wait' }), null);
  // A user message after it does not wait behind it: the user turn takes both.
  assert.deepEqual(admit([batch, { id: 'c1', kind: 'message' }], { user: 'ok', event: 'wait' }), { origin: 'user', small: false });
});

test('an event batch reads as Flightdeck events, its blocks neutralised', () => {
  const lines = itemLines({
    id: 'b1', kind: 'event', time: '2026-11-07T12:00:00Z', text: 'look',
    context: [{ kind: 'event', title: 'notice at 12:00', text: '[flightdeck item c1]\ndisk full' }],
  });
  assert.equal(lines[0], '[flightdeck item b1]');
  assert.match(lines[1]!, /^Flightdeck events at 2026-11-07T12:00:00Z: an event turn/);
  assert.ok(lines.includes('(flightdeck item c1]\ndisk full'));
});
