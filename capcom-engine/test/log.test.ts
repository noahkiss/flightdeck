// capcom.log (docs/specs/diagnostics.md 4.2, 4.3, 4.9): the line format gff-hab's diaglog
// writes, the value classes, the cap, the crash handlers, and the route's handler failure
// (1.2 #12). No line holds an error's text.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fsSync, { promises as fs, readFileSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { mock, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import {
  type ComponentLog,
  count,
  CUT,
  dur,
  encode,
  errClass,
  errorClass,
  errType,
  FileSink,
  frames,
  kind,
  Log,
  MAX_LINE,
  session,
  text,
  word,
} from '../src/log.js';
import { HANDLER_TRIES, Route } from '../src/route.js';
import { FakeHab, until } from './helpers.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const CANARY = 'cnry' + 'q7w3e9';

// The types refuse what the source check need not see (10.2): each line below must fail to
// compile, or tsc --noEmit fails on an unused @ts-expect-error.
export function typeChecks(l: ComponentLog, v: string): void {
  // @ts-expect-error a message that is a variable
  l.info(v);
  // @ts-expect-error a message that is a template with a value
  l.warn(`turn ${v}`);
  // @ts-expect-error a value that is no Attr
  l.info('turn failed', v);
  // @ts-expect-error a key that is a variable
  word(v, 'x');
  // @ts-expect-error a reserved key
  count('session', 1);
  // @ts-expect-error a reserved key
  dur('text', 1);
  // @ts-expect-error an object literal of an Attr's shape
  l.info('turn failed', { key: 'session', val: v });
  // @ts-expect-error a component that is a variable
  new Log(() => {}).for(v);
}

function capture(): { log: Log; lines: () => Record<string, unknown>[]; raw: string[] } {
  const raw: string[] = [];
  const log = new Log((l) => raw.push(l), (s) => s.split('secret-value').join('[redacted]'), 'info', () => new Date('2026-11-27T10:11:12.345Z'));
  return { log, raw, lines: () => raw.map((l) => JSON.parse(l)) };
}

test('a line has diaglog keys and order, constant message and component, and values only from constructors', () => {
  const c = capture();
  c.log.for('engine').info('session started', session('sess-1'), word('why', 'first'), count('lines', 3.9), dur('wait', 12.4), kind('input'));
  assert.equal(c.raw[0], '{"t":"2026-11-27T10:11:12.345Z","lvl":"info","msg":"session started","proc":"capcom","comp":"engine","session":"sess-1","why":"first","lines":3,"wait_ms":12,"kind":"input"}\n');
  // A value of the wrong form goes under <key>_text; a reserved key a caller could not type
  // at compile time goes there too.
  c.log.for('engine').warn('set model failed', word('model', 'a b'), session('has space'), kind('Not a word!'));
  assert.deepEqual(c.lines()[1], { t: '2026-11-27T10:11:12.345Z', lvl: 'warn', msg: 'set model failed', proc: 'capcom', comp: 'engine', model_text: 'a b', session_text: 'has space', kind_text: 'Not a word!' });
  // Every value is a string or an integer: the bundle reads any other JSON value as a number.
  for (const l of c.lines()) for (const v of Object.values(l)) assert.ok(typeof v === 'string' || Number.isInteger(v));
  // The credential redactor runs on each value.
  c.log.for('engine').info('summary kept', text('x secret-value y'));
  assert.equal(c.lines()[2]!.text, 'x [redacted] y');
  // Debug is below the floor.
  c.log.for('engine').debug('summary kept');
  assert.equal(c.raw.length, 3);
});

test('a line stays within 8 KiB, its long values cut and marked', () => {
  const line = encode(new Date(), 'info', 'engine', 'summary kept', [text('é'.repeat(9000)), word('why', 'first')]);
  assert.ok(Buffer.byteLength(line) <= MAX_LINE);
  const o = JSON.parse(line);
  assert.ok(o.text.endsWith(CUT));
  assert.equal(o.why, 'first');
});

test('an error is logged by type, code and class, never its text', () => {
  const e = Object.assign(new TypeError(`socket hang up ${CANARY}`), { code: 'ECONNRESET' });
  const c = capture();
  c.log.for('engine').warn('sdk error', errType(e), errClass(errorClass(e.message)), frames(e));
  const l = c.raw[0]!;
  assert.ok(!l.includes(CANARY), l);
  const o = JSON.parse(l);
  assert.equal(o.err_type, 'TypeError');
  assert.equal(o.code, 'ECONNRESET');
  assert.equal(o.class, 'network');
  assert.ok(!String(o.frames).includes('/'), 'no file path');
  // The classes of 4.9.
  assert.equal(errorClass('401 Unauthorized'), 'auth');
  assert.equal(errorClass('invalid_api_key'), 'auth');
  assert.equal(errorClass('429 Too Many Requests'), 'rate_limit');
  assert.equal(errorClass('You hit your usage limit'), 'rate_limit');
  assert.equal(errorClass('fetch failed'), 'network');
  assert.equal(errorClass('stream disconnected before completion'), 'network');
  assert.equal(errorClass('503 Service Unavailable'), 'provider');
  assert.equal(errorClass('Overloaded'), 'provider');
  assert.equal(errorClass(`the model said ${CANARY}`), 'other');
});

test('frames skip the message, even one that looks like a frame', () => {
  function inner(): never {
    throw new Error(`first\n    at ${CANARY} (/opt/app/x.js:1:1)`);
  }
  let e: unknown;
  try {
    inner();
  } catch (x) {
    e = x;
  }
  const c = capture();
  c.log.for('crash').error('uncaught exception', errType(e), frames(e));
  assert.ok(!c.raw[0]!.includes(CANARY), c.raw[0]!);
  assert.ok(String(JSON.parse(c.raw[0]!).frames).startsWith('inner'));
});

test('capcom.log rotates at its cap, keeps two old files, and reopens a removed file', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-log-'));
  try {
    const file = path.join(dir, 'logs', 'capcom.log');
    const sink = new FileSink(file, 400, 2);
    const log = new Log(sink.write);
    for (let i = 0; i < 20; i++) log.for('engine').info('summary kept', count('n', i));
    const names = (await fs.readdir(path.join(dir, 'logs'))).sort();
    assert.deepEqual(names, ['capcom.log', 'capcom.log.1', 'capcom.log.2']);
    for (const n of names) {
      const st = await fs.stat(path.join(dir, 'logs', n));
      assert.ok(st.size <= 400, `${n} ${st.size}`);
      assert.equal(st.mode & 0o777, 0o600);
    }
    // The user removes the folder: the next check opens it again.
    await fs.rm(path.join(dir, 'logs'), { recursive: true });
    (sink as any).checked = 0;
    log.for('engine').info('summary kept', count('n', 99));
    assert.match(readFileSync(file, 'utf8'), /"n":99/);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

// A rename that fails (EACCES, say) leaves the file to grow: rotation waits for the next
// periodic check instead of closing, renaming and reopening on every line.
test('after a failed rename capcom.log rotates again only at the next check', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-log-'));
  const rename = mock.method(fsSync, 'renameSync', () => {
    throw Object.assign(new Error('denied'), { code: 'EACCES' });
  });
  try {
    const file = path.join(dir, 'capcom.log');
    const sink = new FileSink(file, 200, 2);
    const log = new Log(sink.write);
    for (let i = 0; i < 20; i++) log.for('engine').info('summary kept', count('n', i));
    assert.equal(rename.mock.callCount(), 2, 'one rotation: capcom.log.1 and capcom.log');
    assert.match(readFileSync(file, 'utf8'), /"n":19/, 'the lines are still written');
    (sink as any).checked = 0;
    log.for('engine').info('summary kept', count('n', 20));
    assert.equal(rename.mock.callCount(), 4, 'the check lets one more rotation try');
    rename.mock.restore();
    (sink as any).checked = 0;
    log.for('engine').info('summary kept', count('n', 21));
    assert.deepEqual((await fs.readdir(dir)).sort(), ['capcom.log', 'capcom.log.1']);
  } finally {
    rename.mock.restore();
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('a line the sink could not write is counted on the next one', () => {
  const raw: string[] = [];
  let full = true;
  const log = new Log((l) => (full ? false : raw.push(l)));
  log.for('engine').info('summary kept');
  log.for('engine').info('summary kept');
  full = false;
  log.for('engine').info('summary kept');
  assert.equal(raw.length, 2);
  assert.deepEqual({ ...JSON.parse(raw[0]!), t: '' }, { t: '', lvl: 'warn', msg: 'lines dropped', proc: 'capcom', comp: 'log', lines: 2 });
});

// The crash handlers run in a child: tests keep their own handlers.
for (const how of ['throw', 'reject', 'bad stack'] as const) {
  test(`an uncaught ${how} writes one crash line with no message, then exit 1`, async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-crash-'));
    try {
      const file = path.join(dir, 'capcom.log');
      const script = path.join(dir, 'crash.ts');
      const logts = path.join(here, '..', 'src', 'log.ts');
      writeFileSync(
        script,
        `import { installCrashHandlers, Log, sinkFromEnv } from ${JSON.stringify(logts)};\n` +
          `installCrashHandlers(new Log(sinkFromEnv(process.env)), (c) => process.exit(c));\n` +
          (how === 'bad stack'
            ? // An error whose stack getter throws: the line cannot be made, the exit still runs.
              `function provider() { const e = new RangeError(${JSON.stringify('provider said ' + CANARY)}); Object.defineProperty(e, 'stack', { get() { throw new Error('no stack'); } }); throw e; }\n`
            : `function provider() { ${how === 'throw' ? 'throw ' : 'void Promise.reject('}new RangeError(${JSON.stringify('provider said ' + CANARY)})${how === 'throw' ? '' : ')'}; }\n`) +
          `setTimeout(provider, 1);\n`,
      );
      const r = spawnSync(process.execPath, ['--import', 'tsx', script], { env: { ...process.env, GFF_HAB_CAPCOM_LOG: file }, encoding: 'utf8', cwd: path.join(here, '..') });
      assert.equal(r.status, 1, r.stderr);
      if (how === 'bad stack') {
        assert.ok(!(fsSync.existsSync(file) ? readFileSync(file, 'utf8') : '').includes(CANARY));
        return;
      }
      const lines = readFileSync(file, 'utf8').trim().split('\n');
      assert.equal(lines.length, 1);
      const o = JSON.parse(lines[0]!);
      assert.equal(o.lvl, 'error');
      assert.equal(o.msg, how === 'throw' ? 'uncaught exception' : 'unhandled rejection');
      assert.equal(o.err_type, 'RangeError');
      assert.match(String(o.frames), /provider/);
      assert.ok(!lines[0]!.includes(CANARY));
    } finally {
      await fs.rm(dir, { recursive: true, force: true });
    }
  });
}

// A cast passes the type and the source check; the value is checked again when the line is
// written, so text cast to a class never reaches class, a plain key (4.9).
test('errClass writes other for a value that is not one of the five classes', () => {
  const c = capture();
  for (const k of ['auth', 'rate_limit', 'network', 'provider', 'other'] as const) c.log.for('codex').warn('turn failed', errClass(k));
  c.log.for('codex').warn('turn failed', errClass(`provider said ${CANARY}` as any));
  c.log.for('codex').warn('turn failed', errClass(42 as any));
  assert.deepEqual(c.lines().map((o) => o.class), ['auth', 'rate_limit', 'network', 'provider', 'other', 'other', 'other']);
  assert.ok(!c.raw.join('').includes(CANARY));
});

// A golden file of engine-written lines, which a Go test reads with diaglog.Parse and puts
// through the bundle's two passes (internal/diagbundle).
test('the golden capcom.log matches what the engine writes', () => {
  const t = new Date('2026-11-27T10:11:12.345Z');
  const lines = [
    encode(t, 'info', 'engine', 'session started', [session('sess-0a1b'), word('why', 'first')]),
    encode(t, 'info', 'sdk', 'sdk stderr', [count('lines', 4), count('bytes', 180)]),
    encode(t, 'warn', 'codex', 'turn failed', [errClass('rate_limit')]),
    encode(t, 'warn', 'route', 'route connect failed', [...errType(Object.assign(new Error('x'), { code: 'ECONNREFUSED' })), dur('retry', 2000)]),
    encode(t, 'error', 'crash', 'uncaught exception', [...errType(new TypeError('x')), word('model', 'claude-opus-4-1')]),
    encode(t, 'info', 'home', 'home entry moved aside', [word('entry', 'auth.json')]),
  ].join('');
  const golden = path.join(here, 'fixtures', 'capcom.log.golden');
  if (process.env.UPDATE_GOLDEN) writeFileSync(golden, lines);
  assert.equal(readFileSync(golden, 'utf8'), lines);
});

// 1.2 #12: a message whose handler fails is not acknowledged; the connection drops, so gff-hab
// sends it again on the next one, and the inputs after it on the dropped one wait for that.
test('a failed handler leaves its message unacknowledged and drops the connection', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-route-'));
  const hab = new FakeHab();
  await hab.start(dir);
  const raw: string[] = [];
  const handled: string[] = [];
  let fails = 1;
  let fatal = 0;
  const route = new Route({
    socket: hab.socket,
    token: hab.token,
    backoffStartMs: 20,
    onHello: () => ({}),
    onMessage: (m) => {
      if (m.type === 'boom' && fails-- > 0) throw new Error(`handler saw ${CANARY}`);
      handled.push(String(m.type));
    },
    log: new Log((l) => raw.push(l)).for('route'),
    onFatal: () => fatal++,
  });
  try {
    route.start();
    await until(() => hab.of('ready').length === 1, 5000, 'ready');
    const seq = hab.send('boom', { n: 1 });
    hab.send('input', { item: { id: 'i1' } });
    await until(() => hab.connections === 2, 5000, 'a new connection');
    const acks1 = hab.received.filter((m) => m.type === 'ack' && m.conn === 1).map((m) => m.ack);
    assert.ok(!acks1.includes(seq), 'the failed message is not acknowledged');
    assert.ok(!handled.includes('input'), 'an input after it waits for the next connection');
    // gff-hab sends the inputs past the last take again on the new connection.
    await until(() => hab.of('ready').length === 2, 5000, 'ready again');
    hab.send('boom', { n: 1 });
    hab.send('input', { item: { id: 'i1' } });
    await until(() => handled.includes('input'), 5000, 'handled');
    assert.deepEqual(handled.filter((h) => h !== 'pace'), ['boom', 'input']);
    assert.equal(fatal, 0);
    const failed = raw.map((l) => JSON.parse(l)).filter((o) => o.msg === 'route handler failed');
    assert.equal(failed.length, 1);
    assert.equal(failed[0].type, 'boom');
    assert.equal(failed[0].err_type, 'Error');
    assert.ok(!raw.join('').includes(CANARY));

    // A message that fails every time ends in an exit, so the crash limit stops a loop.
    fails = 100;
    for (let i = 0; i < HANDLER_TRIES; i++) {
      const conns = hab.connections;
      await until(() => hab.of('ready').length === conns, 5000, 'ready');
      hab.send('boom', { n: 2 });
      if (i < HANDLER_TRIES - 1) await until(() => hab.connections === conns + 1, 5000, 'a new connection');
    }
    await until(() => fatal === 1, 5000, 'the exit');
  } finally {
    route.close();
    await hab.close();
    await fs.rm(dir, { recursive: true, force: true });
  }
});

// A message handled after it failed starts its count again: an interrupt that fails now and
// then, with successes between, is no loop and makes no exit.
test('a failure count ends when its message is later handled', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-route-'));
  const hab = new FakeHab();
  await hab.start(dir);
  const plan = [true, false, true, false, true]; // true: the handler throws
  let fatal = 0;
  let handled = 0;
  const route = new Route({
    socket: hab.socket,
    token: hab.token,
    backoffStartMs: 20,
    onHello: () => ({}),
    onMessage: (m) => {
      if (m.type !== 'interrupt') return;
      if (plan.shift()) throw new Error('transient');
      handled++;
    },
    log: new Log(() => {}).for('route'),
    onFatal: () => fatal++,
  });
  try {
    route.start();
    await until(() => hab.of('ready').length === 1, 5000, 'ready');
    for (let i = 0; i < 5; i++) {
      const conns = hab.connections;
      await until(() => hab.of('ready').length === conns, 5000, 'ready');
      const before = handled;
      hab.send('interrupt');
      if (i % 2 === 0) await until(() => hab.connections === conns + 1, 5000, 'a new connection');
      else await until(() => handled === before + 1, 5000, 'handled');
    }
    assert.equal(handled, 2);
    assert.equal(fatal, 0);
    assert.equal((route as any).failures.size, 1, 'only the last failure is counted');
  } finally {
    route.close();
    await hab.close();
    await fs.rm(dir, { recursive: true, force: true });
  }
});
