// The Codex engine against a fake Codex SDK and a fake gff-hab route: the A2 and A4 engine
// cases (capcom-service.md 18), each capability Codex lacks taking its fallback (4.1). No test
// runs the Codex CLI, calls a model or reads a real Codex login: CODEX_HOME is a throwaway
// folder.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promises as fs } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { promisify } from 'node:util';
import { CODEX_CAPABILITIES, CODEX_DEFAULT_MODEL, EPHEMERAL_VAR } from '../src/codex.js';
import { findCodexBinary, wrapperScript, writeWrapper } from '../src/codexbin.js';
import { CODEX_FEATURES_OFF, offeredTools, onCodexSurface } from '../src/codexpolicy.js';
import { SEED_MARKER } from '../src/context.js';
import { findRollout, rolloutContext, rolloutHandedIds } from '../src/rollout.js';
import { START_CHECK_KEY } from '../src/startcheck.js';
import { CODEX_SDK_VERSION } from '../src/version.js';
import { codexRig, codexStarted, type CodexRig } from './codex-helpers.js';
import { PLANTED, sleep, until } from './helpers.js';

const run = promisify(execFile);

async function withRig(opts: Parameters<typeof codexRig>[0], f: (r: CodexRig) => Promise<void>): Promise<void> {
  const r = await codexRig(opts);
  try {
    await f(r);
  } finally {
    await r.stop();
  }
}

test('usage: the rollout rate limits go to gff-hab before the turn end, once per change', async () => {
  const codexLimits = (used: number) => ({
    limit_id: 'codex',
    limit_name: null,
    primary: { used_percent: 12.5, window_minutes: 300, resets_at: 1767268800 },
    secondary: { used_percent: used, window_minutes: 10080, resets_at: 1767744000 },
    credits: null,
    individual_limit: null,
    spend_control_reached: null,
    plan_type: 'plus',
    rate_limit_reached_type: null,
  });
  const modelY = { limit_id: 'model-y', limit_name: null, primary: null, secondary: { used_percent: 100, window_minutes: 10080, resets_at: 1767744000 } };
  await withRig({}, async (r) => {
    let turn = 0;
    r.codex.script = async (ctx) => {
      turn++;
      if (turn === 1) {
        await ctx.limits(codexLimits(30), '2026-01-01T10:00:00.000Z');
        await ctx.limits(modelY, '2026-01-01T10:00:01.000Z');
        await ctx.limits(codexLimits(40), '2026-01-01T10:00:02.000Z');
      }
      if (turn === 3) await ctx.limits(null); // a response with no headers: nothing new
      if (turn === 4) await ctx.limits(codexLimits(41), '2026-01-01T11:00:00.000Z');
      ctx.say('ok');
    };
    await codexStarted(r);
    const order = () => r.hab.received.filter((m) => m.type === 'usage' || m.type === 'turn.ended').map((m) => m.type);
    r.hab.input('it1', 'one');
    await r.hab.wait('turn.ended');
    assert.deepEqual(order(), ['usage', 'turn.ended']);
    const first = r.hab.of('usage')[0]!.reading.rollout;
    // The newest record of each limit id, oldest first, as the CLI wrote them.
    assert.deepEqual(
      first.map((rec: any) => [rec.timestamp, rec.payload.rate_limits.limit_id]),
      [
        ['2026-01-01T10:00:01.000Z', 'model-y'],
        ['2026-01-01T10:00:02.000Z', 'codex'],
      ],
    );
    assert.equal(first[1].payload.rate_limits.secondary.used_percent, 40);
    assert.equal(first[1].type, 'event_msg');
    for (const [it, n] of [['it2', 2], ['it3', 3], ['it4', 4]] as const) {
      r.hab.input(it, 'again');
      await r.hab.wait('turn.ended', () => r.hab.of('turn.ended').length === n);
    }
    const usage = r.hab.of('usage');
    assert.equal(usage.length, 2); // turns 2 and 3 changed nothing
    assert.equal(usage[1]!.reading.rollout.at(-1).payload.rate_limits.secondary.used_percent, 41);
  });
});

test('the Codex SDK version constant matches the pin', async () => {
  const pkg = JSON.parse(await fs.readFile(new URL('../package.json', import.meta.url), 'utf8'));
  assert.equal(pkg.dependencies['@openai/codex-sdk'], CODEX_SDK_VERSION);
});

test('ready declares what Codex has; the start check sees the 5.1 surface', async () => {
  await withRig({}, async (r) => {
    const ready = await r.hab.wait('ready');
    assert.equal(ready.engine, 'codex');
    assert.equal(ready.sdk_version, CODEX_SDK_VERSION);
    assert.deepEqual(ready.capabilities, ['images', 'interrupt', 'resume_by_id', 'usage_events']);
    for (const c of ['mid_turn_input', 'approval_callback', 'compaction_events']) assert.ok(!CODEX_CAPABILITIES.includes(c));
    await codexStarted(r);
    assert.equal(r.codex.probes, 1);
    // The probe ran against the loopback endpoint under a throwaway home with a placeholder key.
    const probe = r.codex.options.find((o) => o.baseUrl?.startsWith('http://127.0.0.1'))!;
    assert.equal(probe.apiKey, START_CHECK_KEY);
    assert.equal(probe.env!.CODEX_HOME, path.join(r.cwd, '.start-check'));
    assert.equal(probe.env![EPHEMERAL_VAR], '1');
    // Every option set carries the features that take built-ins off, and the one MCP server with
    // GFF_HAB passed through by name.
    const main = r.codex.options[r.codex.options.length - 1]!;
    assert.deepEqual((main.config as any).features, CODEX_FEATURES_OFF);
    assert.deepEqual((main.config as any).mcp_servers, { flightdeck: { command: '/opt/gff/gff-hab', args: ['mcp'], env_vars: ['GFF_HAB'] } });
    assert.equal(main.env!.GFF_HAB, r.hab.addr);
    assert.equal(main.env!.GFF_HAB_CAPCOM_TOKEN, undefined, 'the engine route token stays in the engine');
    assert.equal(main.env![EPHEMERAL_VAR], undefined);
    const th = r.codex.threads[r.codex.threads.length - 1]!;
    assert.equal(th.threadOptions.sandboxMode, 'read-only');
    assert.equal(th.threadOptions.approvalPolicy, 'never');
    assert.equal(th.threadOptions.networkAccessEnabled, false);
    assert.equal(th.threadOptions.model, 'fake-codex-model');
    assert.equal(th.threadOptions.workingDirectory, r.cwd);
    // No first session is reported before its first turn gives it an id.
    assert.equal(r.hab.of('session.started').length, 0);
    await assert.rejects(fs.access(path.join(r.cwd, '.start-check')));
  });
});

test('the model defaults to one whose request fits 5.1', async () => {
  await withRig({ hello: { settings: {} } }, async (r) => {
    await codexStarted(r);
    assert.equal(r.codex.threads[r.codex.threads.length - 1]!.threadOptions.model, CODEX_DEFAULT_MODEL);
  });
});

for (const [what, tools] of [
  ['a shell', [{ type: 'function', name: 'shell' }]],
  ['code mode and sub-agents', [{ type: 'namespace', name: 'functions', tools: [{ type: 'custom', name: 'exec' }] }, { type: 'namespace', name: 'collaboration', tools: [{ name: 'spawn_agent' }] }]],
  ['another MCP server', [{ type: 'namespace', name: 'mcp__other', tools: [{ name: 'x' }] }]],
] as const) {
  test(`the start check refuses a request that offers ${what}`, async () => {
    await withRig({ setup: ({ codex }) => void (codex.offered = [...(tools as unknown as unknown[])]) }, async (r) => {
      const err = await r.hab.wait('error');
      assert.equal(err.kind, 'start');
      assert.equal(err.exiting, true);
      assert.match(err.text, /tool outside the Capcom surface/);
      await until(() => r.exitCode() === 1, 3000, 'exit 1');
      assert.equal(r.codex.turns.length, 0, 'no turn ran');
    });
  });
}

test('offeredTools reads top-level, namespaced and additional tools', () => {
  const body = {
    tools: [{ type: 'web_search' }, { type: 'function', name: 'tool_search' }],
    input: [{ type: 'additional_tools', tools: [{ type: 'namespace', name: 'functions', tools: [{ name: 'exec' }, { name: 'wait' }] }] }],
  };
  assert.deepEqual(offeredTools(body), ['web_search', 'tool_search', 'functions.exec', 'functions.wait']);
  assert.ok(onCodexSurface('mcp__flightdeck.hosts') && onCodexSurface('mcp__flightdeck__hosts'));
  assert.ok(!onCodexSurface('functions.exec') && !onCodexSurface('shell') && !onCodexSurface('collaboration.spawn_agent'));
});

test('a turn: started, taken, reply, tool lines, ended with tokens and context use', async () => {
  await withRig({}, async (r) => {
    r.codex.script = async (ctx) => {
      ctx.mcp('hosts', { query: 'online' });
      ctx.search('weather tomorrow');
      await ctx.context(0.42);
      ctx.say('hello back');
    };
    await codexStarted(r);
    r.hab.input('it1', 'hello');
    const ended = await r.hab.wait('turn.ended');
    const started = r.hab.of('turn.started')[0]!;
    assert.deepEqual(started.items, ['it1']);
    assert.equal(started.origin, 'user');
    assert.deepEqual(r.hab.of('input.taken').map((m) => m.item), ['it1']);
    const ss = r.hab.of('session.started');
    assert.equal(ss.length, 1);
    assert.equal(ss[0]!.why, 'first');
    assert.equal(ss[0]!.session, r.codex.turns[0]!.thread.id);
    assert.deepEqual(
      r.hab.of('tool').map((m) => [m.name, m.summary]),
      [
        ['mcp__flightdeck__hosts', 'hosts: online'],
        ['WebSearch', 'weather tomorrow'],
      ],
    );
    assert.equal(r.hab.of('reply')[0]!.text, 'hello back');
    assert.equal(ended.result, 'success');
    assert.equal(ended.input_tokens, 120);
    assert.equal(ended.output_tokens, 7);
    assert.ok(Math.abs(ended.context_used - 0.42) < 0.001);
    assert.match(r.codex.turns[0]!.text, /^\[flightdeck item it1\]\nSent by user at /);
  });
});

test('no mid-turn input: a message during a turn waits for the next turn, in order, each taken', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.codex.script = async (ctx) => {
      if (ctx.text.includes('first')) await ctx.until(() => release);
      ctx.say('done');
    };
    await codexStarted(r);
    r.hab.input('it1', 'first');
    await r.hab.wait('turn.started');
    r.hab.input('it2', 'second');
    r.hab.input('it3', 'third');
    await sleep(100);
    assert.equal(r.codex.turns.length, 1, 'nothing goes in during the turn');
    release = true;
    await until(() => r.hab.of('turn.ended').length === 2, 5000, 'two turns');
    assert.deepEqual(r.hab.of('turn.started').map((m) => m.items), [['it1'], ['it2', 'it3']]);
    assert.deepEqual(r.hab.of('input.taken').map((m) => m.item), ['it1', 'it2', 'it3']);
    const second = r.codex.turns[1]!.text;
    assert.ok(second.indexOf('[flightdeck item it2]') < second.indexOf('[flightdeck item it3]'));
    // The same thread throughout.
    assert.equal(r.codex.turns[0]!.thread, r.codex.turns[1]!.thread);
  });
});

test('interrupt ends the turn interrupted and the next input runs', async () => {
  await withRig({}, async (r) => {
    r.codex.script = async (ctx) => {
      if (ctx.text.includes('long')) await ctx.until(() => false);
      ctx.say('next ran');
    };
    await codexStarted(r);
    r.hab.input('it1', 'long');
    await r.hab.wait('input.taken');
    r.hab.send('interrupt');
    const ended = await r.hab.wait('turn.ended');
    assert.equal(ended.result, 'interrupted');
    r.hab.input('it2', 'after');
    await r.hab.wait('reply', (m) => m.text === 'next ran');
    assert.equal(r.exitCode(), undefined);
  });
});

test('shutdown waits for the turn end, then exits 0', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.codex.script = async (ctx) => {
      await ctx.until(() => release);
      ctx.say('finished');
    };
    await codexStarted(r);
    r.hab.input('it1', 'work');
    await r.hab.wait('turn.started');
    r.hab.send('shutdown');
    await sleep(80);
    assert.equal(r.exitCode(), undefined);
    release = true;
    await r.hab.wait('turn.ended');
    await until(() => r.exitCode() === 0, 3000, 'exit 0');
  });
});

test('resume rebuilds the handed set from the rollout markers and resumes the thread by id', async () => {
  const id = '01a1b56d-8402-7dc1-9bc0-d60094de8351';
  await withRig(
    {
      hello: { sdk_session: id },
      setup: async ({ codex }) => {
        await codex.appendRollout(id, [
          { type: 'session_meta', payload: { id } },
          { type: 'response_item', payload: { type: 'message', role: 'user', content: [{ type: 'input_text', text: '[flightdeck item it1]\nold one' }] } },
          // a marker that is not at a line start is no marker
          { type: 'response_item', payload: { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'see [flightdeck item it9]' }] } },
        ]);
      },
    },
    async (r) => {
      await codexStarted(r);
      const ss = await r.hab.wait('session.started');
      assert.deepEqual([ss.session, ss.why], [id, 'restart']);
      r.hab.input('it1', 'old one'); // handed before the crash: never twice
      r.hab.input('it2', 'new one');
      await r.hab.wait('turn.ended');
      assert.equal(r.codex.turns.length, 1);
      assert.equal(r.codex.turns[0]!.thread.id, id);
      assert.ok(!r.codex.turns[0]!.text.includes('[flightdeck item it1]'));
      assert.ok(r.codex.turns[0]!.text.includes('[flightdeck item it2]'));
      assert.deepEqual([...(await rolloutHandedIds(await findRollout(r.home, id)))].sort(), ['it1', 'it2']);
    },
  );
});

test('a thread that will not resume is replaced by a re-seed', async () => {
  await withRig({ hello: { sdk_session: '0000aaaa-0000-0000-0000-000000000000' } }, async (r) => {
    const err = await r.hab.wait('error');
    assert.equal(err.exiting, false);
    assert.match(err.text, /resume of .* failed/);
    await r.hab.wait('context.request', (m) => m.for === 'reseed');
    const ss = await r.hab.wait('session.started');
    assert.deepEqual([ss.session, ss.why], ['', 'reseed']);
  });
});

test('a rollout that is there but will not load is replaced by a re-seed, not a crash loop', async () => {
  const id = '02b2c67e-9513-8ed2-0f1f-e70005ef9462';
  await withRig(
    {
      hello: { sdk_session: id },
      setup: async ({ codex }) => {
        codex.resumeFails = true;
        await codex.appendRollout(id, [{ type: 'session_meta', payload: { id } }]);
      },
    },
    async (r) => {
      await codexStarted(r);
      await r.hab.wait('session.started', (m) => m.session === id);
      r.hab.input('it1', 'after the restart');
      const err = await r.hab.wait('error');
      assert.equal(err.exiting, false);
      assert.match(err.text, /resume of .* failed/);
      await r.hab.wait('context.request', (m) => m.for === 'reseed');
      await r.hab.wait('session.started', (m) => m.why === 'reseed');
      // The untaken item goes into the fresh session, once.
      await r.hab.wait('input.taken', (m) => m.item === 'it1');
      const fresh = r.codex.turns.filter((t) => !t.thread.resumed);
      assert.equal(fresh.length, 1);
      assert.equal(fresh[0]!.text.split('[flightdeck item it1]').length, 2);
      assert.equal(r.exitCode(), undefined);
    },
  );
});

test('a tool outside 5.1 that runs anyway ends the turn and the engine', async () => {
  await withRig({}, async (r) => {
    r.codex.script = async (ctx) => {
      ctx.item({ type: 'command_execution', command: 'cat /etc/hosts', aggregated_output: '', status: 'in_progress' });
      await ctx.until(() => false);
    };
    await codexStarted(r);
    r.hab.input('it1', 'go');
    const ended = await r.hab.wait('turn.ended');
    assert.equal(ended.result, 'error');
    const err = await r.hab.wait('error');
    assert.equal(err.kind, 'start');
    assert.match(err.text, /command_execution/);
    await until(() => r.exitCode() === 1, 3000, 'exit 1');
  });
});

test('the API key reaches the SDK as apiKey and nowhere else; inherited keys never pass', async () => {
  await withRig({}, async (r) => {
    await codexStarted(r);
    r.hab.input('it1', 'hi');
    await r.hab.wait('turn.ended');
    const main = r.codex.turns[0]!.thread.codexOptions;
    assert.equal(main.apiKey, PLANTED);
    for (const o of r.codex.options) {
      assert.equal(o.env!.OPENAI_API_KEY, undefined);
      assert.equal(o.env!.CODEX_API_KEY, undefined);
      assert.ok(!JSON.stringify(o.env).includes(PLANTED));
    }
    assert.ok(!r.logs.join('\n').includes(PLANTED));
  });
});

test('the user\'s own Codex login: no key passes, CODEX_HOME does', async () => {
  await withRig({ hello: { credential: { kind: 'login', value: 'ignored' } } }, async (r) => {
    await codexStarted(r);
    r.hab.input('it1', 'hi');
    await r.hab.wait('turn.ended');
    const main = r.codex.turns[0]!.thread.codexOptions;
    assert.equal(main.apiKey, undefined);
    assert.equal(main.env!.CODEX_HOME, r.home);
  });
});

test('hello with no credential, or an OAuth token, gives error auth', async () => {
  await withRig({ hello: { credential: { kind: 'oauth_token', value: 'x'.repeat(20) } } }, async (r) => {
    const err = await r.hab.wait('error');
    assert.deepEqual([err.kind, err.exiting], ['auth', true]);
    await until(() => r.exitCode() === 1, 3000, 'exit 1');
  });
});

test('a refused credential gives turn.ended error, then error auth and exit 1', async () => {
  await withRig({}, async (r) => {
    r.codex.script = async (ctx) => {
      ctx.fail(`unexpected status 401 Unauthorized: bad key ${PLANTED}`);
    };
    await codexStarted(r);
    r.hab.input('it1', 'hi');
    const ended = await r.hab.wait('turn.ended');
    assert.equal(ended.result, 'error');
    const err = await r.hab.wait('error');
    assert.deepEqual([err.kind, err.exiting], ['auth', true]);
    await until(() => r.exitCode() === 1, 3000, 'exit 1');
    assert.ok(!r.logs.join('\n').includes(PLANTED), 'the key is redacted');
  });
});

test('a turn the CLI never started exits so gff-hab restarts the engine', async () => {
  await withRig({ setup: ({ codex }) => void (codex.startTurn = false) }, async (r) => {
    await codexStarted(r);
    r.hab.input('it1', 'hi');
    await r.hab.wait('turn.ended');
    assert.equal(r.hab.of('input.taken').length, 0);
    const err = await r.hab.wait('error');
    assert.equal(err.exiting, true);
    await until(() => r.exitCode() === 1, 3000, 'exit 1');
  });
});

test('images go in as local_image parts; an unsupported one as a note', async () => {
  await withRig({}, async (r) => {
    const png = path.join(r.dir, 'flyer.png');
    await fs.writeFile(png, Buffer.from([0x89, 0x50, 0x4e, 0x47]));
    await codexStarted(r);
    r.hab.input('it1', 'look', { images: [png, path.join(r.dir, 'doc.pdf')], context: [{ kind: 'notification', title: 'A notice', text: 'body' }] });
    await r.hab.wait('turn.ended');
    const input = r.codex.turns[0]!.input;
    assert.ok(Array.isArray(input));
    assert.deepEqual((input as any[]).filter((p) => p.type === 'local_image'), [{ type: 'local_image', path: png }]);
    const text = r.codex.turns[0]!.text;
    assert.match(text, /\[image doc\.pdf: not a supported image type\]/);
    assert.ok(text.indexOf('[context: notification: A notice]') < text.indexOf('look'));
  });
});

// ---- context and memory (A4 on Codex) ----

test('a re-seed: the summary call, the session ends at once, the seed joins the first message', async () => {
  await withRig({}, async (r) => {
    r.hab.context = () => ({
      summary: 'old summary',
      through: 'it1',
      recent: [{ id: 'it1', kind: 'message', author: 'user', time: '2026-01-01T10:00:00Z', text: 'my sister is Ana' }],
      seed: { logbook: 'Guides:\n- prefers window seats', messages: [{ id: 'it1', kind: 'message', author: 'user', text: 'my sister is Ana' }], open: [] },
    });
    await codexStarted(r);
    r.hab.input('it1', 'hello');
    await r.hab.wait('turn.ended');
    const first = r.codex.turns[0]!.thread;
    r.hab.send('reseed', { reason: 'limit' });
    const summary = await r.hab.wait('summary');
    assert.deepEqual([summary.text, summary.through], ['fake summary', 'it1']);
    const pend = await r.hab.wait('session.started', (m) => m.why === 'reseed');
    assert.equal(pend.session, '', 'the fresh thread has no id before its first turn');
    // The summary call ran on the small model, saved nothing, offered no tools or MCP server.
    const s = r.codex.summaries[0]!;
    assert.equal(s.thread.model, 'fake-codex-small');
    assert.equal(s.thread.webSearchMode, 'disabled');
    assert.equal(s.options.env![EPHEMERAL_VAR], '1');
    assert.equal((s.options.config as any).mcp_servers, undefined);
    assert.notEqual(s.thread.workingDirectory, r.cwd);
    assert.match(s.prompt, /old summary/);
    r.hab.input('it2', 'who is my sister?');
    await until(() => r.hab.of('turn.ended').length === 2, 5000, 'the second turn');
    const second = r.codex.turns[1]!;
    assert.notEqual(second.thread, first, 'a fresh thread');
    assert.ok(second.text.startsWith(SEED_MARKER), 'the seed opens the first message');
    assert.match(second.text, /fake summary/);
    assert.match(second.text, /prefers window seats/);
    assert.match(second.text, /my sister is Ana/);
    assert.ok(second.text.indexOf(SEED_MARKER) < second.text.indexOf('[flightdeck item it2]'));
    const real = r.hab.of('session.started').filter((m) => m.why === 'reseed' && m.session);
    assert.equal(real.length, 1);
    assert.equal(real[0]!.session, second.thread.id);
    // The third turn carries no seed again.
    r.hab.input('it3', 'thanks');
    await until(() => r.hab.of('turn.ended').length === 3, 5000, 'the third turn');
    assert.ok(!r.codex.turns[2]!.text.includes(SEED_MARKER));
  });
});

test('a re-seed during a turn waits for the turn end', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.codex.script = async (ctx) => {
      await ctx.until(() => release);
      ctx.say('ok');
    };
    await codexStarted(r);
    r.hab.input('it1', 'long');
    await r.hab.wait('turn.started');
    r.hab.send('reseed', { reason: 'limit' });
    await sleep(100);
    assert.equal(r.hab.of('context.request').length, 0, 'never mid-turn');
    release = true;
    await r.hab.wait('turn.ended');
    await r.hab.wait('session.started', (m) => m.why === 'reseed');
  });
});

test('a new conversation starts clean: no summary call, an empty summary in the seed', async () => {
  await withRig({}, async (r) => {
    r.hab.context = () => ({ summary: 'should not be used', through: 'it1', recent: [{ id: 'it1', text: 'x' }], seed: {} });
    await codexStarted(r);
    r.hab.send('reseed', { reason: 'new' });
    await r.hab.wait('session.started', (m) => m.why === 'new');
    assert.equal(r.codex.summaries.length, 0);
    r.hab.input('it2', 'fresh');
    await r.hab.wait('turn.ended');
    assert.ok(!r.codex.turns[0]!.text.includes('should not be used'));
  });
});

test('hello with reseed switch opens with a re-seed, not an empty session', async () => {
  await withRig({ hello: { reseed: 'switch' } }, async (r) => {
    r.hab.context = () => ({ summary: 'what Claude knew', through: 'it5', recent: [], seed: { logbook: 'Guides:\n- likes tea' } });
    await r.hab.wait('context.request', (m) => m.for === 'reseed');
    await r.hab.wait('session.started', (m) => m.why === 'reseed');
    r.hab.input('it6', 'hello again');
    await r.hab.wait('turn.ended');
    assert.match(r.codex.turns[0]!.text, /what Claude knew/);
    assert.match(r.codex.turns[0]!.text, /likes tea/);
    assert.equal(r.hab.of('session.started').filter((m) => m.why === 'first').length, 0);
  });
});

test('a quiet summary update runs between turns', async () => {
  await withRig({}, async (r) => {
    await codexStarted(r);
    r.hab.send('context', { for: 'quiet', summary: '', through: 'it3', recent: [{ id: 'it3', kind: 'message', author: 'user', text: 'a fact' }] });
    const s = await r.hab.wait('summary');
    assert.equal(s.through, 'it3');
  });
});

test('a summary call past its bound leaves the old summary', async () => {
  await withRig({ hello: { settings: { precompact_timeout_ms: 100 } } }, async (r) => {
    r.codex.summaryDelayMs = 2000;
    r.hab.context = () => ({ summary: 'kept', through: 'it1', recent: [{ id: 'it1', text: 'x' }], seed: {} });
    await codexStarted(r);
    r.hab.send('reseed', { reason: 'quiet' });
    await r.hab.wait('session.started', (m) => m.why === 'reseed');
    assert.equal(r.hab.of('summary').length, 0);
    r.hab.input('it2', 'go');
    await r.hab.wait('turn.ended');
    assert.match(r.codex.turns[0]!.text, /kept/);
  });
});

// ---- the rollout and the wrapper ----

test('rolloutContext reads the last token_count', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rollout-'));
  try {
    const f = path.join(dir, 'r.jsonl');
    await fs.writeFile(
      f,
      [
        JSON.stringify({ type: 'event_msg', payload: { type: 'token_count', info: { last_token_usage: { input_tokens: 1000, output_tokens: 0 }, model_context_window: 10000 } } }),
        JSON.stringify({ type: 'event_msg', payload: { type: 'token_count', info: { last_token_usage: { input_tokens: 5000, output_tokens: 1000 }, model_context_window: 10000 } } }),
        '{"half":',
      ].join('\n'),
    );
    assert.equal(await rolloutContext(f), 0.6);
    assert.equal(await rolloutContext(undefined), 0);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('the wrapper adds the flags after exec, and --ephemeral on request', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'wrap-'));
  try {
    const real = path.join(dir, "fake codex's bin");
    await fs.writeFile(real, '#!/bin/sh\nfor a in "$@"; do printf "%s\\n" "$a"; done\n', { mode: 0o700 });
    const w = await writeWrapper(dir, real);
    assert.equal((await fs.stat(w)).mode & 0o777, 0o700);
    const plain = (await run(w, ['exec', '--experimental-json', 'resume', 'abc'], { env: { PATH: '/usr/bin:/bin' } })).stdout.trim().split('\n');
    assert.deepEqual(plain, ['exec', '--ignore-user-config', '--ignore-rules', '--experimental-json', 'resume', 'abc']);
    const eph = (await run(w, ['exec', '--model', 'm'], { env: { PATH: '/usr/bin:/bin', [EPHEMERAL_VAR]: '1' } })).stdout.trim().split('\n');
    assert.deepEqual(eph, ['exec', '--ignore-user-config', '--ignore-rules', '--ephemeral', '--model', 'm']);
    assert.match(wrapperScript(real), /^#!\/bin\/sh\n/);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('the pinned Codex CLI is found beside the SDK', { skip: !['darwin', 'linux'].includes(process.platform) && 'no build' }, async () => {
  const bin = findCodexBinary(new URL('../src/codexbin.ts', import.meta.url).href);
  assert.ok((await fs.stat(bin)).isFile());
});
