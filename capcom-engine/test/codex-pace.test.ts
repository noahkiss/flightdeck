// The governor's pace in the Codex engine (capcom-service.md 10.1), through the shared rules of
// contract.ts: wait holds the turn, small runs it on the small model on the same thread, a
// send-now message runs with the items before it and not those after it, and an event turn
// waits at its own step. A fake Codex SDK; no CLI, no model call.
import assert from 'node:assert/strict';
import test from 'node:test';
import { codexRig, codexStarted, type CodexRig } from './codex-helpers.js';
import { sleep, until } from './helpers.js';

async function withRig(f: (r: CodexRig) => Promise<void>): Promise<void> {
  const r = await codexRig({});
  try {
    await f(r);
  } finally {
    await r.stop();
  }
}

const ended = (r: CodexRig, n: number) => until(() => r.hab.of('turn.ended').length >= n, 5000, `${n} turns ended`);
const models = (r: CodexRig) => r.codex.turns.map((t) => t.thread.threadOptions.model);

test('Codex: wait holds the turn, small runs the small model on the same thread', async () => {
  await withRig(async (r) => {
    await codexStarted(r);
    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    r.hab.input('i1', 'first');
    await ended(r, 1);
    const id = r.codex.turns[0]!.thread.id;

    r.hab.send('pace', { pace: 'wait', until: '2026-11-08T00:00:00Z', event_pace: 'wait' });
    r.hab.input('i2', 'second');
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 1, 'a turn began while the pace said wait');

    r.hab.send('pace', { pace: 'small', event_pace: 'wait' });
    await ended(r, 2);
    assert.deepEqual(models(r), ['fake-codex-model', 'fake-codex-small']);
    assert.equal(r.codex.turns[1]!.thread.id, id, 'the small turn left the thread');
    assert.deepEqual(r.hab.of('turn.started')[1]!.items, ['i2']);

    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    r.hab.input('i3', 'third');
    await ended(r, 3);
    assert.deepEqual(models(r), ['fake-codex-model', 'fake-codex-small', 'fake-codex-model']);
    assert.equal(r.codex.turns[2]!.thread.id, id);
  });
});

test('Codex: send now during a wait runs the items before it, not those after it', async () => {
  await withRig(async (r) => {
    await codexStarted(r);
    r.hab.send('pace', { pace: 'wait', until: '2026-11-08T00:00:00Z', event_pace: 'wait' });
    r.hab.input('i1', 'waits');
    await sleep(100);
    assert.equal(r.hab.of('turn.started').length, 0);
    // All three arrive before the engine looks: i3 and the report come after the send-now one.
    r.hab.input('i2', 'now', { send_now: true });
    r.hab.input('i3', 'after');
    r.hab.input('a1', 'a report', { kind: 'agent', author: 'bee/bob' });
    await ended(r, 1);
    await sleep(150);
    const t = r.hab.of('turn.started');
    assert.equal(t.length, 1, 'an item after the send-now one began a turn');
    assert.deepEqual(t[0]!.items.slice(0, 1), ['i1']);
    assert.ok(!t[0]!.items.includes('i3') && !t[0]!.items.includes('a1'), `the send-now turn took ${t[0]!.items}`);
    assert.deepEqual(models(r), ['fake-codex-small']);

    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    await until(() => r.hab.of('turn.started').flatMap((x) => x.items).includes('a1'), 5000, 'the held items ran');
  });
});

test('Codex: an event turn waits at its own step, and a user message does not wait behind it', async () => {
  await withRig(async (r) => {
    await codexStarted(r);
    r.hab.send('pace', { pace: 'ok', event_pace: 'wait', event_until: '2026-11-08T00:00:00Z' });
    r.hab.input('e1', 'an event', { kind: 'event' });
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 0, 'an event turn began past event_stop');
    r.hab.input('u1', 'a question');
    await ended(r, 1);
    const t = r.hab.of('turn.started');
    assert.equal(t[0]!.origin, 'user');
    assert.deepEqual(t[0]!.items, ['e1', 'u1']);
  });
});

test('contract: admit and handCount over a held queue', async () => {
  const { admit, handCount } = await import('../src/contract.js');
  const wait = { user: 'wait', event: 'wait' } as const;
  const q = [
    { id: 'i1', kind: 'message' },
    { id: 'i2', kind: 'message', send_now: true },
    { id: 'i3', kind: 'message' },
    { id: 'a1', kind: 'agent' },
  ];
  assert.deepEqual(admit(q, wait), { origin: 'user', small: true });
  assert.equal(handCount(q, wait), 2, 'only up to the last send-now item');
  assert.equal(admit(q.slice(2), wait), null);
  assert.equal(handCount(q.slice(2), { user: 'wait', event: 'ok' }), 0, 'a user message heads the queue');
  assert.equal(handCount(q.slice(3), { user: 'wait', event: 'ok' }), 1);
  assert.equal(handCount(q, { user: 'small', event: 'wait' }), 4);
});
