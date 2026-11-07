// The governor's pace (capcom-service.md 10.1): the engine holds a new turn at wait, runs it on
// the small model at small, and runs a send-now message on the small model while user turns
// wait. A fake SDK; no model call.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { DEFAULT_SMALL_MODEL } from '../src/engine.js';
import { rig, sleep, started, until, type Rig } from './helpers.js';

async function withRig(opts: Parameters<typeof rig>[0], f: (r: Rig) => Promise<void>): Promise<void> {
  const r = await rig(opts);
  try {
    await f(r);
  } finally {
    await r.stop();
  }
}

const ended = (r: Rig, n: number) => until(() => r.hab.of('turn.ended').length >= n, 5000, `${n} turns ended`);

test('pace ok runs the main model, small the small model, wait holds the turn', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.send('pace', { pace: 'small', event_pace: 'wait' });
    r.hab.input('i1', 'first');
    await ended(r, 1);
    assert.deepEqual(r.sdk.last.turnModels, ['fake-small']);

    r.hab.send('pace', { pace: 'wait', until: '2026-11-08T00:00:00Z', event_pace: 'wait' });
    r.hab.input('i2', 'second');
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 1, 'a turn began while the pace said wait');
    assert.equal(r.sdk.last.texts().length, 1, 'the SDK got the message while the pace said wait');

    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    await ended(r, 2);
    assert.deepEqual(r.sdk.last.turnModels, ['fake-small', 'fake-model']);
    assert.deepEqual(r.hab.of('turn.started')[1]!.items, ['i2']);
  });
});

test('send now runs the waiting messages with it on the small model', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.send('pace', { pace: 'wait', until: '2026-11-08T00:00:00Z', event_pace: 'wait' });
    r.hab.input('i1', 'waits');
    await sleep(100);
    assert.equal(r.hab.of('turn.started').length, 0);
    r.hab.input('i2', 'now', { send_now: true });
    // The fake SDK takes one message a turn: the held one, then the send-now one, both small.
    await ended(r, 2);
    assert.deepEqual(r.sdk.last.turnModels, ['fake-small', 'fake-small']);
    assert.deepEqual(r.hab.of('turn.started')[0]!.items, ['i1']);
    assert.equal(r.hab.of('input.taken').length, 2);
    // The next message waits again.
    r.hab.input('i3', 'later');
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 2);
  });
});

test('an event turn waits at its own step, and a user message does not wait behind it', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.send('pace', { pace: 'ok', event_pace: 'wait', event_until: '2026-11-08T00:00:00Z' });
    r.hab.input('e1', 'an event', { kind: 'event' });
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 0, 'an event turn began past event_stop');
    r.hab.input('u1', 'a question');
    // One message a turn in the fake SDK: the event, then the question, both user turns.
    await ended(r, 2);
    const t = r.hab.of('turn.started');
    assert.deepEqual(t.map((x) => x.origin), ['user', 'user']);
    assert.deepEqual(t[0]!.items, ['e1']);

    // An event alone runs once the event pace is ok.
    r.hab.input('e2', 'another event', { kind: 'event' });
    await sleep(100);
    assert.equal(r.hab.of('turn.started').length, 2);
    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    await ended(r, 3);
    assert.equal(r.hab.of('turn.started')[2]!.origin, 'event');
    assert.deepEqual(r.sdk.last.turnModels, ['fake-model', 'fake-model', 'fake-model']);
  });
});

test('an agent report waits at the event step, as no user typed it', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.send('pace', { pace: 'ok', event_pace: 'wait', event_until: '2026-11-08T00:00:00Z' });
    r.hab.input('a1', 'a report', { kind: 'agent', author: 'bee/bob' });
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 0, 'an agent turn began past event_stop');
    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    await ended(r, 1);
    assert.equal(r.hab.of('turn.started')[0]!.origin, 'user');
  });
});

test('with no small model in hello the small model is the default alias', async () => {
  await withRig({ hello: { settings: { model: 'fake-model', max_turns: 7 } } }, async (r) => {
    await started(r);
    r.hab.send('pace', { pace: 'small', event_pace: 'wait' });
    r.hab.input('i1', 'hi');
    await ended(r, 1);
    assert.deepEqual(r.sdk.last.turnModels, [DEFAULT_SMALL_MODEL]);
  });
});

test('with no pace message every turn runs on the main model', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.input('i1', 'hi');
    await ended(r, 1);
    assert.deepEqual(r.sdk.last.turnModels, ['fake-model']);
  });
});

test('a send-now turn during a wait takes no message queued after it', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.send('pace', { pace: 'wait', until: '2026-11-08T00:00:00Z', event_pace: 'wait' });
    r.hab.input('i1', 'waits');
    await sleep(100);
    r.hab.input('i2', 'now', { send_now: true });
    r.hab.input('i3', 'after');
    r.hab.input('a1', 'a report', { kind: 'agent', author: 'bee/bob' });
    // The fake SDK takes one message a turn: i1, then i2 as the follow-on turn; i3 and a1 wait.
    await ended(r, 2);
    await sleep(150);
    assert.equal(r.hab.of('turn.started').length, 2, 'an item after the send-now one began a turn');
    assert.equal(r.sdk.last.texts().length, 2, 'the SDK got an item queued after the send-now one');
    // The pace lifts: the held items run, in order.
    r.hab.send('pace', { pace: 'ok', event_pace: 'ok' });
    await ended(r, 4);
    const t = r.hab.of('turn.started');
    assert.deepEqual(t.slice(2).map((x) => x.items), [['i3'], ['a1']]);
  });
});
