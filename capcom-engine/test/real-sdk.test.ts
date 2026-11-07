// One run of the real SDK and its bundled binary against a fake Messages API
// on 127.0.0.1, with a dummy token and a temporary config folder. No real
// Anthropic call is made. Skipped when the platform binary is missing.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, promises as fs } from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Engine } from '../src/engine.js';
import { findTranscript, handedIds, transcriptPath } from '../src/transcript.js';
import { FakeHab, until } from './helpers.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const bin = path.join(here, '..', 'node_modules', '@anthropic-ai', `claude-agent-sdk-${process.platform}-${process.arch}`, 'claude');
const DUMMY = 'dummy-token-for-a-fake-server';

function fakeApi(status: number): http.Server {
  return http.createServer((req, res) => {
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      if (!req.url?.startsWith('/v1/messages') || req.url.includes('count_tokens')) {
        res.writeHead(404, { 'content-type': 'application/json' });
        res.end('{"type":"error","error":{"type":"not_found_error","message":"no"}}');
        return;
      }
      if (status !== 200) {
        res.writeHead(status, { 'content-type': 'application/json' });
        res.end('{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}');
        return;
      }
      let j: any = {};
      try {
        j = JSON.parse(body);
      } catch {
        // keep the default
      }
      const msg = { id: 'msg_1', type: 'message', role: 'assistant', model: j.model || 'm', content: [] as any[], stop_reason: null, stop_sequence: null, usage: { input_tokens: 10, output_tokens: 1 } };
      if (!j.stream) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ ...msg, content: [{ type: 'text', text: 'ok' }], stop_reason: 'end_turn' }));
        return;
      }
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      const ev = (t: string, d: unknown) => res.write(`event: ${t}\ndata: ${JSON.stringify(d)}\n\n`);
      ev('message_start', { type: 'message_start', message: msg });
      ev('content_block_start', { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } });
      ev('content_block_delta', { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'hello from the fake' } });
      ev('content_block_stop', { type: 'content_block_stop', index: 0 });
      ev('message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn', stop_sequence: null }, usage: { output_tokens: 5 } });
      ev('message_stop', { type: 'message_stop' });
      res.end();
    });
  });
}

async function run(status: number, f: (h: { hab: FakeHab; code: () => number | undefined; cfg: string; cwd: string; logs: string[] }) => Promise<void>): Promise<void> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-real-'));
  const cfg = path.join(dir, 'cfg');
  const cwd = path.join(dir, 'engine');
  await fs.mkdir(cfg);
  await fs.mkdir(cwd);
  const api = fakeApi(status);
  await new Promise<void>((r) => api.listen(0, '127.0.0.1', r));
  const port = (api.address() as any).port;
  const hab = new FakeHab();
  hab.hello = { credential: { kind: 'oauth_token', value: DUMMY }, device: 'd', last_item: '', sdk_session: '', settings: {} };
  await hab.start(dir);
  let code: number | undefined;
  const logs: string[] = [];
  const engine = new Engine({
    query: (p) => query(p),
    env: {
      PATH: process.env.PATH,
      HOME: dir,
      CLAUDE_CONFIG_DIR: cfg,
      GFF_HAB: hab.addr,
      GFF_HAB_CAPCOM_TOKEN: hab.token,
      ANTHROPIC_BASE_URL: `http://127.0.0.1:${port}`,
      DISABLE_TELEMETRY: '1',
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
    },
    cwd,
    exit: (c) => {
      code = c;
    },
    log: (l) => logs.push(l),
    backoffStartMs: 50,
  });
  engine.start();
  try {
    await f({ hab, code: () => code, cfg, cwd, logs });
  } finally {
    hab.send('shutdown');
    await until(() => code !== undefined, 10000).catch(() => undefined);
    (engine as any).route?.close();
    await hab.close();
    api.close();
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('the real SDK binary: start check, a turn, the transcript markers', { skip: !existsSync(bin) && 'no SDK binary for this platform', timeout: 60000 }, async () => {
  await run(200, async ({ hab, cfg, cwd, logs }) => {
    const s = await hab.wait('session.started', undefined, 30000);
    hab.input('r1', 'hi');
    const ended = await hab.wait('turn.ended', undefined, 30000);
    assert.equal(ended.result, 'success');
    assert.equal(hab.of('input.taken')[0]?.item, 'r1');
    assert.equal(hab.of('reply')[0]?.text, 'hello from the fake');
    assert.equal(hab.of('error').length, 0, logs.join('\n'));
    // The transcript lives where transcriptPath says for the real path of
    // the folder, with the marker.
    const file = transcriptPath(cfg, await fs.realpath(cwd), s.session);
    assert.equal(await findTranscript(cfg, cwd, s.session), file);
    assert.deepEqual([...(await handedIds(file))], ['r1']);
    const captured = JSON.stringify(hab.received) + logs.join('\n') + (await fs.readFile(file, 'utf8'));
    assert.ok(!captured.includes(DUMMY));
  });
});

test('the real SDK binary: a 401 gives error auth and a non-zero exit', { skip: !existsSync(bin) && 'no SDK binary for this platform', timeout: 60000 }, async () => {
  await run(401, async ({ hab, code }) => {
    await hab.wait('session.started', undefined, 30000);
    hab.input('r1', 'hi');
    const err = await hab.wait('error', undefined, 30000);
    assert.equal(err.kind, 'auth');
    await until(() => code() !== undefined, 10000);
    assert.notEqual(code(), 0);
  });
});
