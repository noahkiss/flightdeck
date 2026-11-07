import { test } from 'node:test';
import assert from 'node:assert/strict';
import { promises as fs, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseHabAddr } from '../src/addr.js';
import { CAPABILITIES, START_CHECK_TEXT } from '../src/engine.js';
import { ALLOW_TOOLS, DENY_TOOLS, urlKey } from '../src/policy.js';
import { encodeCwd, transcriptPath } from '../src/transcript.js';
import { SDK_VERSION } from '../src/version.js';
import { firstText, PLANTED, rig, sleep, started, until, type Rig } from './helpers.js';

const here = path.dirname(fileURLToPath(import.meta.url));

async function withRig(opts: Parameters<typeof rig>[0], f: (r: Rig) => Promise<void>): Promise<void> {
  const r = await rig(opts);
  try {
    await f(r);
  } finally {
    await r.stop();
  }
}

test('GFF_HAB parses as the hook package does', () => {
  const a = parseHabAddr(`http+unix:///run/x/hook.sock/hooks/${'0f'.repeat(10)}`);
  assert.equal(a.socket, '/run/x/hook.sock');
  assert.equal(a.token, '0f'.repeat(10));
  assert.throws(() => parseHabAddr(undefined));
  assert.throws(() => parseHabAddr('http://x/hooks/abcdefabcdefabcdef'));
  assert.throws(() => parseHabAddr('http+unix://rel/hooks/abcdefabcdefabcdef'));
  assert.throws(() => parseHabAddr('http+unix:///s/hooks/XYZ'));
});

test('the transcript path follows the CLI folder key', () => {
  assert.equal(encodeCwd('/state/capcom/engine'), '-state-capcom-engine');
  assert.equal(encodeCwd('/a_b/c.d'), '-a-b-c-d');
  assert.equal(transcriptPath('/cfg', '/state/x', 'S1'), '/cfg/projects/-state-x/S1.jsonl');
});

test('the SDK version constant matches the pin', () => {
  const pkg = JSON.parse(readFileSync(path.join(here, '..', 'package.json'), 'utf8'));
  assert.equal(pkg.dependencies['@anthropic-ai/claude-agent-sdk'], SDK_VERSION);
  for (const deps of [pkg.dependencies, pkg.devDependencies]) {
    for (const v of Object.values(deps)) assert.match(String(v), /^\d+\.\d+\.\d+$/, 'every dependency pinned exactly');
  }
});

test('hello gets ready, and the SDK gets exactly the 5.1 surface', async () => {
  await withRig({}, async (r) => {
    await started(r);
    const ready = r.hab.of('ready')[0]!;
    assert.equal(ready.engine, 'claude');
    assert.equal(ready.sdk_version, SDK_VERSION);
    assert.deepEqual(ready.capabilities, CAPABILITIES);
    assert.equal(ready.seq, 1);
    const s = r.hab.of('session.started')[0]!;
    assert.equal(s.why, 'first');
    assert.equal(s.session, r.sdk.last.sessionId);

    const o = r.sdk.calls[0]!;
    assert.deepEqual(o.disallowedTools, ['Bash', 'Read', 'Edit', 'Write', 'Glob', 'Grep', 'Agent', 'Skill', 'ToolSearch', 'AskUserQuestion', 'TaskCreate', 'TaskUpdate', 'NotebookEdit']);
    assert.deepEqual(o.disallowedTools, [...DENY_TOOLS]);
    assert.deepEqual(o.allowedTools, ['mcp__flightdeck__*', 'WebSearch', 'ListMcpResourcesTool', 'ReadMcpResourceTool']);
    assert.deepEqual(o.allowedTools, [...ALLOW_TOOLS]);
    assert.ok(!o.allowedTools!.includes('WebFetch'));
    assert.deepEqual(o.tools, ['WebSearch', 'WebFetch']);
    assert.deepEqual(o.settingSources, ['project']);
    assert.equal(o.permissionMode, 'default');
    assert.equal(o.cwd, r.cwd);
    assert.equal(o.model, 'fake-model');
    assert.equal(o.maxTurns, 7);
    assert.equal(o.maxBudgetUsd, 1.5);
    assert.equal(o.resume, undefined);
    assert.equal(typeof o.canUseTool, 'function');
    assert.equal(o.hooks?.PreToolUse?.[0]?.matcher, 'WebFetch');
    assert.ok('replay-user-messages' in (o.extraArgs ?? {}));
    assert.equal(o.env?.GFF_HAB, r.hab.addr);
    assert.equal(o.env?.CLAUDE_CONFIG_DIR, r.cfg);
    // The probe is the only thing the SDK got before any input.
    assert.equal(r.sdk.last.received.length, 1);
    assert.equal(r.sdk.last.received[0]!.shouldQuery, false);
    assert.equal(r.sdk.last.received[0]!.message.content, START_CHECK_TEXT);
  });
});

test('a turn: started, taken, reply, tool line, ended with cost and context use', async () => {
  await withRig({}, async (r) => {
    r.sdk.script = async (ctx) => {
      ctx.say('first part');
      await ctx.tool('mcp__flightdeck__hosts', { query: 'all hosts' }, 'two hosts');
      ctx.say('second part');
    };
    await started(r);
    r.hab.input('a1', 'hello');
    const ended = await r.hab.wait('turn.ended');
    const ts = r.hab.of('turn.started')[0]!;
    assert.deepEqual(ts.items, ['a1']);
    assert.equal(ts.origin, 'user');
    assert.equal(r.hab.of('input.taken')[0]!.item, 'a1');
    const replies = r.hab.of('reply');
    assert.deepEqual(replies.map((m) => [m.part, m.text]), [[0, 'first part'], [1, 'second part']]);
    assert.ok(replies.every((m) => m.turn === ts.turn));
    assert.equal(r.hab.of('tool')[0]!.summary, 'hosts: all hosts');
    assert.equal(ended.turn, ts.turn);
    assert.equal(ended.result, 'success');
    assert.equal(ended.input_tokens, 300);
    assert.equal(ended.output_tokens, 20);
    assert.ok(Math.abs(ended.cost_usd - 0.01) < 1e-9);
    assert.ok(Math.abs(ended.context_used - 0.32) < 1e-9);
    const text = r.sdk.last.texts()[0]!;
    assert.ok(text.startsWith('[flightdeck item a1]\n'));
    assert.match(text, /2026-01-01T10:00:00Z \(UTC\)/);
    assert.ok(text.endsWith('hello'));
  });
});

test('two inputs during a turn go in in order, each taken', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.sdk.script = async (ctx) => {
      await ctx.until(() => release);
      ctx.fold();
      ctx.say('all three');
    };
    await started(r);
    r.hab.input('a', 'one');
    await r.hab.wait('turn.started');
    r.hab.input('b', 'two');
    r.hab.input('c', 'three');
    await until(() => r.sdk.last.texts().length === 3, 5000, 'three inputs at the SDK');
    release = true;
    await r.hab.wait('turn.ended');
    assert.deepEqual(r.hab.of('input.taken').map((m) => m.item), ['a', 'b', 'c']);
    assert.deepEqual(r.sdk.last.texts().map((t) => t.split('\n')[0]), ['[flightdeck item a]', '[flightdeck item b]', '[flightdeck item c]']);
    assert.equal(r.hab.of('turn.started').length, 1);
    // A repeat of a handed item is skipped.
    r.hab.input('b', 'two again');
    await sleep(100);
    assert.equal(r.sdk.last.texts().length, 3);
  });
});

test('an input the SDK queues past the result opens the next turn', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.sdk.script = async (ctx) => {
      if (firstText(ctx).includes('item a]')) await ctx.until(() => release);
      ctx.say('done');
    };
    await started(r);
    r.hab.input('a', 'one');
    await r.hab.wait('turn.started');
    r.hab.input('b', 'two');
    await until(() => r.sdk.last.texts().length === 2);
    release = true;
    await until(() => r.hab.of('turn.ended').length === 2);
    const starts = r.hab.of('turn.started');
    assert.deepEqual(starts.map((s) => s.items), [['a'], ['b']]);
    assert.deepEqual(r.hab.of('input.taken').map((m) => m.item), ['a', 'b']);
  });
});

test('interrupt ends the turn interrupted and the next input runs', async () => {
  await withRig({}, async (r) => {
    r.sdk.script = async (ctx) => {
      if (firstText(ctx).includes('item slow]')) {
        await ctx.until(() => ctx.aborted);
        return;
      }
      ctx.say('quick');
    };
    await started(r);
    r.hab.input('slow', 'take your time');
    await r.hab.wait('turn.started');
    r.hab.send('interrupt');
    const e1 = await r.hab.wait('turn.ended');
    assert.equal(e1.result, 'interrupted');
    assert.equal(r.sdk.last.interrupts, 1);
    r.hab.input('next', 'go');
    const e2 = await r.hab.wait('turn.ended', (m) => m.turn !== e1.turn);
    assert.equal(e2.result, 'success');
    assert.equal(r.hab.of('reply').at(-1)!.text, 'quick');
  });
});

test('shutdown waits for the turn end, then exits 0', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.sdk.script = async (ctx) => {
      await ctx.until(() => release);
      ctx.say('finished');
    };
    await started(r);
    r.hab.input('a', 'long job');
    await r.hab.wait('turn.started');
    r.hab.send('shutdown');
    await sleep(150);
    assert.equal(r.exitCode(), undefined);
    r.hab.input('late', 'after shutdown');
    await sleep(50);
    release = true;
    await until(() => r.exitCode() !== undefined, 5000, 'exit');
    assert.equal(r.exitCode(), 0);
    assert.equal(r.hab.of('turn.ended')[0]!.result, 'success');
    assert.ok(!r.sdk.last.texts().some((t) => t.includes('item late]')), 'nothing new goes in after shutdown');
  });
});

test('shutdown while idle exits 0 at once', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.send('shutdown');
    await until(() => r.exitCode() !== undefined);
    assert.equal(r.exitCode(), 0);
  });
});

test('a reconnect resends unacked replies, in order, with new seq numbers', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.ackEngine = false;
    r.sdk.script = async (ctx) => {
      ctx.say('kept until acked');
      ctx.say('second');
    };
    r.hab.input('a', 'hi');
    await r.hab.wait('turn.ended');
    const first = r.hab.of('reply');
    assert.equal(first.length, 2);
    r.hab.drop();
    r.hab.ackEngine = true;
    await r.hab.wait('ready', (m) => m.conn === 2);
    const again = await until(() => {
      const xs = r.hab.received.filter((m) => m.conn === 2 && m.type === 'reply');
      return xs.length === 2 ? xs : undefined;
    });
    assert.deepEqual(again.map((m) => [m.turn, m.part, m.text]), first.map((m) => [m.turn, m.part, m.text]));
    const conn2 = r.hab.received.filter((m) => m.conn === 2 && m.type !== 'ack');
    assert.equal(conn2[0]!.type, 'ready');
    assert.deepEqual(conn2.map((m) => m.seq), conn2.map((_, i) => i + 1), 'seq starts at 1 on each connection');
    // turn.started, input.taken and turn.ended were unacked too.
    assert.deepEqual(conn2.map((m) => m.type), ['ready', 'turn.started', 'input.taken', 'reply', 'reply', 'turn.ended']);
    // Messages acked on the second connection are not sent a third time.
    await until(() => (r.engine as any).route.pending === 0);
    r.hab.drop();
    await r.hab.wait('ready', (m) => m.conn === 3);
    await sleep(100);
    assert.equal(r.hab.received.filter((m) => m.conn === 3 && m.type !== 'ack').length, 1);
    // The SDK session went on: no new query.
    assert.equal(r.sdk.calls.length, 1);
  });
});

test('resume rebuilds the handed set from the transcript markers', async () => {
  const sid = '5e550000-0000-4000-8000-000000000001';
  await withRig(
    {
      hello: { sdk_session: sid },
      setup: async ({ cfg, cwd }) => {
        const file = transcriptPath(cfg, cwd, sid);
        await fs.mkdir(path.dirname(file), { recursive: true });
        const text = await fs.readFile(path.join(here, 'fixtures', 'resumed.jsonl'), 'utf8');
        await fs.writeFile(file, text.replaceAll('SESSION', sid));
      },
    },
    async (r) => {
      await started(r);
      assert.equal(r.sdk.calls[0]!.resume, sid);
      assert.equal(r.hab.of('session.started')[0]!.why, 'restart');
      // gff-hab sends every item past the last input.taken: i1 was taken,
      // i2 was handed and never echoed, i3 is new.
      r.hab.input('i2', 'handed, never echoed');
      r.hab.input('i3', 'new');
      r.hab.input('i9', 'a marker quoted in a tool result does not count');
      await until(() => r.sdk.last.texts().length === 2);
      await sleep(100);
      assert.deepEqual(r.sdk.last.texts().map((t) => t.split('\n')[0]), ['[flightdeck item i3]', '[flightdeck item i9]']);
      // A URL from a user message of the thread before the restart is seen.
      r.sdk.script = async (ctx) => {
        await ctx.tool('WebFetch', { url: 'https://example.net/notes', prompt: 'read' });
      };
      r.hab.input('i4', 'fetch it');
      await until(() => r.hab.of('turn.ended').length >= 2);
      assert.equal(r.hab.of('approval.request').length, 0);
    },
  );
});

test('the start check refuses an init that lists Bash', async () => {
  await withRig({ setup: ({ sdk }) => void (sdk.initTools = ['WebFetch', 'WebSearch', 'Bash', 'mcp__flightdeck__hosts']) }, async (r) => {
    await r.hab.wait('ready');
    const err = await r.hab.wait('error');
    assert.equal(err.kind, 'start');
    assert.equal(err.exiting, true);
    assert.match(err.text, /Bash/);
    await until(() => r.exitCode() !== undefined);
    assert.notEqual(r.exitCode(), 0);
    r.hab.input('a', 'never goes in');
    await sleep(100);
    assert.equal(r.sdk.last.texts().length, 0);
    assert.equal(r.hab.of('session.started').length, 0);
  });
});

test('the start check accepts the MCP resource tools while flightdeck is the only MCP server', async () => {
  const tools = ['WebFetch', 'WebSearch', 'mcp__flightdeck__hosts', 'ListMcpResourcesTool', 'ReadMcpResourceTool'];
  await withRig({ setup: ({ sdk }) => void (sdk.initTools = tools) }, async (r) => {
    await started(r);
    assert.equal(r.hab.of('error').length, 0);
    assert.equal(r.hab.of('session.started').length, 1);
  });
});

test('the start check refuses the MCP resource tools when another MCP server is listed', async () => {
  const tools = ['WebFetch', 'WebSearch', 'mcp__flightdeck__hosts', 'ListMcpResourcesTool'];
  const servers = [{ name: 'flightdeck', status: 'connected' }, { name: 'other', status: 'connected' }];
  await withRig({ setup: ({ sdk }) => void ((sdk.initTools = tools), (sdk.initServers = servers)) }, async (r) => {
    await r.hab.wait('ready');
    const err = await r.hab.wait('error');
    assert.equal(err.kind, 'start');
    assert.match(err.text, /ListMcpResourcesTool/);
    await until(() => r.exitCode() !== undefined);
  });
});

test('WebFetch of an unseen URL raises an approval card; allow lets it run', async () => {
  await withRig({}, async (r) => {
    let verdict = '';
    r.sdk.script = async (ctx) => {
      verdict = await ctx.tool('WebFetch', { url: 'https://unseen.example.com/secret?q=1', prompt: 'read' });
      // Once allowed, the same URL needs no second card this turn.
      verdict += ',' + (await ctx.tool('WebFetch', { url: 'https://unseen.example.com/secret?q=1', prompt: 'again' }));
    };
    await started(r);
    r.hab.input('a', 'look something up');
    const req = await r.hab.wait('approval.request');
    assert.equal(req.tool, 'WebFetch');
    assert.equal(req.input.url, 'https://unseen.example.com/secret?q=1');
    assert.equal(req.reason, 'Capcom wants to open unseen.example.com/secret');
    r.hab.send('approval.answer', { request: req.request, allow: true, message: '' });
    await r.hab.wait('turn.ended');
    assert.equal(verdict, 'allow,allow');
    assert.equal(r.hab.of('approval.request').length, 1);
  });
});

test('WebFetch: a denied or expired card denies with the hab message', async () => {
  await withRig({}, async (r) => {
    let verdict = '';
    r.sdk.script = async (ctx) => {
      verdict = await ctx.tool('WebFetch', { url: 'https://unseen.example.com/x', prompt: 'read' });
    };
    await started(r);
    r.hab.input('a', 'look it up');
    const req = await r.hab.wait('approval.request');
    r.hab.send('approval.answer', { request: req.request, allow: false, message: 'the user did not answer' });
    await r.hab.wait('turn.ended');
    assert.equal(verdict, 'deny');
  });
});

test('WebFetch of a URL from a user message or a tool result raises no card', async () => {
  await withRig({}, async (r) => {
    const verdicts: string[] = [];
    r.sdk.script = async (ctx) => {
      verdicts.push(await ctx.tool('WebFetch', { url: 'https://example.org/page/', prompt: 'read' }));
      await ctx.tool('WebSearch', { query: 'weather' }, 'result: https://search.example.com/r1 and more');
      verdicts.push(await ctx.tool('WebFetch', { url: 'https://search.example.com/r1#top', prompt: 'read' }));
    };
    await started(r);
    r.hab.input('a', 'please read https://example.org/page.');
    await r.hab.wait('turn.ended');
    assert.deepEqual(verdicts, ['allow', 'allow']);
    assert.equal(r.hab.of('approval.request').length, 0);
    // A search result counts for its own turn only.
    r.sdk.script = async (ctx) => {
      void ctx.tool('WebFetch', { url: 'https://search.example.com/r1', prompt: 'read' });
      await ctx.until(() => false);
    };
    r.hab.input('b', 'again');
    await r.hab.wait('approval.request');
  });
});

test('a tool outside WebFetch that reaches canUseTool is denied', async () => {
  await withRig({}, async (r) => {
    await started(r);
    const can = r.sdk.calls[0]!.canUseTool!;
    const res = await can('Bash', { command: 'ls' }, { signal: new AbortController().signal } as any);
    assert.equal(res?.behavior, 'deny');
  });
});

test('urlKey drops the fragment and a trailing slash', () => {
  assert.equal(urlKey('https://Example.com/a/#x'), 'https://example.com/a');
  assert.equal(urlKey('ftp://example.com/'), undefined);
});

for (const kind of ['oauth_token', 'api_key'] as const) {
  test(`the ${kind} credential reaches the SDK env under its name and nowhere else`, async () => {
    await withRig({ hello: { credential: { kind, value: PLANTED } } }, async (r) => {
      await started(r);
      const env = r.sdk.calls[0]!.env!;
      const name = kind === 'oauth_token' ? 'CLAUDE_CODE_OAUTH_TOKEN' : 'ANTHROPIC_API_KEY';
      const other = kind === 'oauth_token' ? 'ANTHROPIC_API_KEY' : 'CLAUDE_CODE_OAUTH_TOKEN';
      assert.equal(env[name], PLANTED);
      assert.equal(env[other], undefined, 'an inherited credential never reaches the binary');
      // Leak paths: the SDK's stderr, a tool input, and an SDK crash text.
      r.sdk.calls[0]!.stderr!(`debug: auth ${PLANTED}\n`);
      r.sdk.script = async (ctx) => {
        await ctx.tool('mcp__flightdeck__hosts', { query: `x ${PLANTED}` });
        ctx.say('ok');
      };
      r.hab.input('a', 'hi');
      await r.hab.wait('turn.ended');
      r.sdk.last.fail(new Error(`boom ${PLANTED}`));
      await r.hab.wait('error');
      await until(() => r.exitCode() !== undefined);
      const captured = JSON.stringify(r.hab.received) + '\n' + r.logs.join('\n');
      assert.ok(!captured.includes(PLANTED), 'the credential appears in no message or log line');
      assert.ok(r.logs.some((l) => l.includes('[redacted]')));
      const files = await fs.readdir(r.dir, { recursive: true });
      for (const f of files) {
        const p = path.join(r.dir, String(f));
        if ((await fs.stat(p)).isFile() && !p.endsWith('.sock')) assert.ok(!(await fs.readFile(p, 'utf8')).includes(PLANTED));
      }
    });
  });
}

test('a refused credential gives error auth and a non-zero exit', async () => {
  await withRig({}, async (r) => {
    r.sdk.script = async (ctx) => ctx.authFail();
    await started(r);
    r.hab.input('a', 'hi');
    const ended = await r.hab.wait('turn.ended');
    assert.equal(ended.result, 'error');
    const err = await r.hab.wait('error');
    assert.equal(err.kind, 'auth');
    assert.equal(err.exiting, true);
    await until(() => r.exitCode() !== undefined);
    assert.notEqual(r.exitCode(), 0);
  });
});

test('reseed ends the SDK session and starts a fresh one', async () => {
  await withRig({}, async (r) => {
    await started(r);
    r.hab.input('a', 'hi');
    await r.hab.wait('turn.ended');
    r.hab.send('reseed', { reason: 'new' });
    const s2 = await r.hab.wait('session.started', (m) => m.why === 'new');
    assert.equal(r.sdk.calls.length, 2);
    assert.equal(r.sdk.calls[1]!.resume, undefined);
    assert.notEqual(s2.session, r.hab.of('session.started')[0]!.session);
    // A handed id from the old session goes in again in the new one.
    r.hab.input('a', 'hi again');
    await until(() => r.sdk.last.texts().length === 1);
    r.hab.send('reseed', { reason: 'limit' });
    await r.hab.wait('session.started', (m) => m.why === 'reseed');
  });
});

test('reseed during a turn waits for the turn end', async () => {
  await withRig({}, async (r) => {
    let release = false;
    r.sdk.script = async (ctx) => {
      await ctx.until(() => release);
      ctx.say('done');
    };
    await started(r);
    r.hab.input('a', 'hi');
    await r.hab.wait('turn.started');
    r.hab.send('reseed', { reason: 'quiet' });
    await sleep(100);
    assert.equal(r.sdk.calls.length, 1);
    release = true;
    await r.hab.wait('session.started', (m) => m.why === 'reseed');
    assert.equal(r.hab.of('turn.ended')[0]!.result, 'success');
  });
});

test('images go in as base64 image blocks; context goes before the text', async () => {
  await withRig({}, async (r) => {
    await started(r);
    const png = path.join(r.dir, 'shot.png');
    await fs.writeFile(png, Buffer.from([0x89, 0x50, 0x4e, 0x47]));
    r.hab.input('a', 'what is this', {
      images: [png, path.join(r.dir, 'missing.jpg')],
      context: [{ kind: 'notice', title: 'Build failed', text: 'exit 1' }],
    });
    await r.hab.wait('turn.ended');
    const content = r.sdk.last.received[1]!.message.content as any[];
    assert.equal(content[0].type, 'text');
    const text: string = content[0].text;
    assert.ok(text.indexOf('Build failed') < text.indexOf('what is this'));
    assert.match(text, /missing\.jpg: could not be read/);
    assert.equal(content[1].type, 'image');
    assert.equal(content[1].source.media_type, 'image/png');
    assert.equal(content[1].source.data, Buffer.from([0x89, 0x50, 0x4e, 0x47]).toString('base64'));
  });
});

test('compaction and rate-limit events reach gff-hab', async () => {
  await withRig({}, async (r) => {
    r.sdk.script = async (ctx) => {
      (r.sdk.last as any).emit({ type: 'system', subtype: 'compact_boundary', compact_metadata: { trigger: 'auto', pre_tokens: 9 } });
      (r.sdk.last as any).emit({ type: 'rate_limit_event', rate_limit_info: { status: 'allowed', utilization: 0.4 } });
      ctx.say('ok');
    };
    await started(r);
    r.hab.input('a', 'hi');
    await r.hab.wait('turn.ended');
    assert.equal(r.hab.of('compacted').length, 1);
    assert.deepEqual(r.hab.of('usage')[0]!.reading, { status: 'allowed', utilization: 0.4 });
  });
});

test('a route that refuses the token is retried with backoff', async () => {
  await withRig({}, async (r) => {
    await started(r);
    (r.hab as any).token = 'cd'.repeat(16);
    r.hab.drop();
    await until(() => r.hab.refused >= 2, 5000, 'two refused connects');
    (r.hab as any).token = 'ab'.repeat(16);
    await r.hab.wait('ready', (m) => m.conn === 2);
  });
});

test('after a reconnect, ready names the running turn and open cards are asked again', async () => {
  await withRig({ hello: { settings: { route_backoff_max_ms: 2000 } } }, async (r) => {
    let verdict = '';
    r.sdk.script = async (ctx) => {
      verdict = await ctx.tool('WebFetch', { url: 'https://unseen.example.com/y', prompt: 'read' });
    };
    await started(r);
    assert.equal((r.engine as any).route.backoffMax, 2000);
    assert.equal(r.hab.of('ready')[0]!.turn, undefined);
    r.hab.input('a', 'look it up');
    const req = await r.hab.wait('approval.request');
    const ts = r.hab.of('turn.started')[0]!;
    await until(() => (r.engine as any).route.pending === 0);
    r.hab.drop();
    const ready = await r.hab.wait('ready', (m) => m.conn === 2);
    assert.deepEqual(ready.turn, { turn: ts.turn, origin: 'user' });
    const again = await r.hab.wait('approval.request', (m) => m.conn === 2);
    assert.deepEqual({ ...again, seq: 0, conn: 0 }, { ...req, seq: 0, conn: 0 });
    r.hab.send('approval.answer', { request: req.request, allow: true, message: '' });
    await r.hab.wait('turn.ended');
    assert.equal(verdict, 'allow');
  });
});
