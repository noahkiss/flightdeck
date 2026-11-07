// Test doubles: a fake SDK (no model call, no credential read) and a fake
// gff-hab engine route on a temporary Unix socket.
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { promises as fs } from 'node:fs';
import type { Options, SDKMessage, SDKUserMessage } from '@anthropic-ai/claude-agent-sdk';
import { Engine, type QueryHandle } from '../src/engine.js';

export type Msg = { type: string; seq?: number; [k: string]: any };

// ---- waiting ----

export async function until<T>(f: () => T | undefined | false, ms = 5000, what = 'condition'): Promise<T> {
  const end = Date.now() + ms;
  for (;;) {
    const v = f();
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 5));
  }
}

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

// ---- the fake SDK ----

export interface TurnCtx {
  readonly consumed: SDKUserMessage[];
  readonly aborted: boolean;
  say(text: string): void;
  tool(name: string, input: Record<string, unknown>, response?: string): Promise<'allow' | 'deny'>;
  fold(): void;
  until(f: () => boolean): Promise<void>;
  authFail(): void;
  // compact runs the PreCompact hooks, then emits compact_boundary, as an SDK compaction does.
  compact(): Promise<void>;
  options: Options;
}

export type Script = (ctx: TurnCtx) => Promise<void>;

const echo: Script = async (ctx) => {
  ctx.say(`echo ${ctx.consumed.length}`);
};

export class FakeSdk {
  calls: Options[] = [];
  queries: FakeQuery[] = [];
  initTools = ['WebFetch', 'WebSearch', 'mcp__flightdeck__hosts'];
  initServers: { name: string; status: string }[] = [{ name: 'flightdeck', status: 'connected' }];
  script: Script = echo;
  // The summary calls (persistSession false), kept apart from the session queries.
  summaryCalls: { options: Options; prompt: string }[] = [];
  summarize: (prompt: string) => Promise<string> | string = (prompt) => `summary: ${prompt.split('\n').filter((l) => l.includes(': ')).length} lines`;
  query = (p: { prompt: AsyncIterable<SDKUserMessage>; options: Options }): QueryHandle => {
    if (p.options.persistSession === false) return new FakeSummary(this, p.prompt, p.options);
    this.calls.push(p.options);
    const q = new FakeQuery(this, p.prompt, p.options);
    this.queries.push(q);
    return q;
  };
  get last(): FakeQuery {
    return this.queries[this.queries.length - 1]!;
  }
}

let uuidN = 0;
const fakeUuid = () => `00000000-0000-4000-8000-${String(++uuidN).padStart(12, '0')}`;

export class FakeQuery implements QueryHandle {
  // Every user message the SDK got, the probe included.
  received: SDKUserMessage[] = [];
  private out: SDKMessage[] = [];
  private wake?: () => void;
  private closed = false;
  private queue: SDKUserMessage[] = [];
  private busy = false;
  private aborted = false;
  private initSent = false;
  private cost = 0;
  private abort = new AbortController();
  interrupts = 0;
  readonly sessionId: string;

  constructor(private sdk: FakeSdk, prompt: AsyncIterable<SDKUserMessage>, readonly options: Options) {
    this.sessionId = options.resume ?? fakeUuid();
    void this.read(prompt);
  }

  private emit(m: any): void {
    this.out.push({ session_id: this.sessionId, uuid: fakeUuid(), ...m });
    this.wake?.();
  }

  private emitInit(): void {
    if (this.initSent) return;
    this.initSent = true;
    this.emit({ type: 'system', subtype: 'init', tools: this.sdk.initTools, mcp_servers: this.sdk.initServers, cwd: this.options.cwd });
  }

  private replay(m: SDKUserMessage): void {
    this.emit({ type: 'user', message: m.message, parent_tool_use_id: null, uuid: m.uuid, isReplay: true });
  }

  private async read(prompt: AsyncIterable<SDKUserMessage>): Promise<void> {
    for await (const m of prompt) {
      if (this.closed) return;
      this.received.push(m);
      if (m.shouldQuery === false) {
        this.emitInit();
        this.replay(m);
        this.emit({ type: 'result', subtype: 'success', is_error: false, num_turns: 0, total_cost_usd: this.cost, usage: {}, modelUsage: {} });
        continue;
      }
      this.queue.push(m);
      this.kick();
    }
  }

  private kick(): void {
    if (this.busy || this.closed || !this.queue.length) return;
    this.busy = true;
    void this.turn();
  }

  private async turn(): Promise<void> {
    const first = this.queue.shift()!;
    const consumed = [first];
    this.emitInit();
    this.replay(first);
    let auth = false;
    this.abort = new AbortController();
    const self = this;
    const assistant = (content: any[], extra: any = {}) =>
      this.emit({
        type: 'assistant',
        parent_tool_use_id: null,
        message: { role: 'assistant', content, usage: { input_tokens: 300, output_tokens: 20 } },
        user_message_uuids: consumed.map((c) => c.uuid),
        ...extra,
      });
    const ctx: TurnCtx = {
      consumed,
      get aborted() {
        return self.aborted;
      },
      options: this.options,
      say: (text) => assistant([{ type: 'text', text }]),
      fold: () => {
        while (this.queue.length) {
          const m = this.queue.shift()!;
          consumed.push(m);
          this.replay(m);
        }
      },
      until: async (f) => {
        while (!f() && !this.closed) await sleep(5);
      },
      authFail: () => {
        auth = true;
        assistant([{ type: 'text', text: 'Not logged in' }], { error: 'authentication_failed' });
      },
      tool: (name, input, response) => this.runTool(name, input, response ?? '', assistant),
      compact: async () => {
        for (const m of this.options.hooks?.PreCompact ?? []) {
          for (const h of m.hooks) await h({ hook_event_name: 'PreCompact', trigger: 'auto', custom_instructions: null } as any, undefined, { signal: this.abort.signal });
        }
        this.emit({ type: 'system', subtype: 'compact_boundary', compact_metadata: { trigger: 'auto', pre_tokens: 900 } });
      },
    };
    try {
      await this.sdk.script(ctx);
    } catch {
      // a script that throws ends its turn
    }
    if (this.closed) return;
    this.cost += 0.01;
    this.emit({
      type: 'result',
      subtype: this.aborted ? 'error_during_execution' : 'success',
      is_error: this.aborted || auth,
      api_error_status: auth ? 401 : undefined,
      num_turns: 1,
      total_cost_usd: this.cost,
      usage: { input_tokens: 300, output_tokens: 20 },
      modelUsage: { fake: { contextWindow: 1000 } },
      user_message_uuids: consumed.map((c) => c.uuid),
    });
    this.aborted = false;
    this.busy = false;
    this.kick();
  }

  private async runTool(name: string, input: Record<string, unknown>, response: string, assistant: (c: any[]) => void): Promise<'allow' | 'deny'> {
    const id = `toolu_${fakeUuid()}`;
    assistant([{ type: 'tool_use', id, name, input }]);
    const o = this.options;
    let decision: string | undefined;
    for (const m of o.hooks?.PreToolUse ?? []) {
      if (m.matcher && m.matcher !== name) continue;
      for (const h of m.hooks) {
        const r: any = await h({ hook_event_name: 'PreToolUse', tool_name: name, tool_input: input, tool_use_id: id } as any, id, { signal: this.abort.signal });
        decision = r?.hookSpecificOutput?.permissionDecision ?? decision;
      }
    }
    let verdict: 'allow' | 'deny';
    if (decision === 'allow') verdict = 'allow';
    else if (decision === 'deny') verdict = 'deny';
    else if (decision !== 'ask' && (o.allowedTools ?? []).some((a) => a === name || (a.endsWith('*') && name.startsWith(a.slice(0, -1))))) verdict = 'allow';
    else if (o.canUseTool) {
      const r = await o.canUseTool(name, input, { signal: this.abort.signal, toolUseID: id } as any);
      verdict = r?.behavior === 'allow' ? 'allow' : 'deny';
    } else verdict = 'deny';
    if (verdict === 'allow') {
      for (const m of o.hooks?.PostToolUse ?? []) {
        if (m.matcher && m.matcher !== name) continue;
        for (const h of m.hooks) await h({ hook_event_name: 'PostToolUse', tool_name: name, tool_input: input, tool_response: response, tool_use_id: id } as any, id, { signal: this.abort.signal });
      }
    }
    this.emit({ type: 'user', parent_tool_use_id: null, message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: id, content: verdict === 'allow' ? response : 'denied' }] } });
    return verdict;
  }

  async interrupt(): Promise<unknown> {
    this.interrupts++;
    this.aborted = true;
    this.abort.abort();
    return {};
  }

  close(): void {
    this.closed = true;
    this.wake?.();
  }

  // Make the SDK stream fail, as a crash of the binary does.
  fail(err: Error): void {
    this.failure = err;
    this.wake?.();
  }
  private failure?: Error;

  async *[Symbol.asyncIterator](): AsyncIterator<SDKMessage> {
    for (;;) {
      while (this.out.length) yield this.out.shift()!;
      if (this.failure) throw this.failure;
      if (this.closed) return;
      await new Promise<void>((r) => (this.wake = r));
      this.wake = undefined;
    }
  }

  // The text the model got, per real (non-probe) message.
  texts(): string[] {
    return this.received
      .filter((m) => m.shouldQuery !== false)
      .map((m) => {
        const c = m.message.content as any;
        return typeof c === 'string' ? c : c.filter((b: any) => b.type === 'text').map((b: any) => b.text).join('\n');
      });
  }
}

// The text of the first message a turn took.
export function firstText(ctx: TurnCtx): string {
  const c = ctx.consumed[0]?.message.content as any;
  return typeof c === 'string' ? c : (c?.[0]?.text ?? '');
}

// A summary call: one prompt in, the text of sdk.summarize out, then a result.
class FakeSummary implements QueryHandle {
  private out: SDKMessage[] = [];
  private wake?: () => void;
  private closed = false;
  constructor(private sdk: FakeSdk, prompt: AsyncIterable<SDKUserMessage>, private options: Options) {
    void this.run(prompt);
  }
  private async run(prompt: AsyncIterable<SDKUserMessage>): Promise<void> {
    for await (const m of prompt) {
      const c = m.message.content as any;
      const text = typeof c === 'string' ? c : c.map((b: any) => b.text ?? '').join('\n');
      this.sdk.summaryCalls.push({ options: this.options, prompt: text });
      let got: string;
      try {
        got = await this.sdk.summarize(text);
      } catch {
        this.push({ type: 'result', subtype: 'error_during_execution', is_error: true });
        return;
      }
      this.push({ type: 'assistant', parent_tool_use_id: null, message: { role: 'assistant', content: [{ type: 'text', text: got }] } });
      this.push({ type: 'result', subtype: 'success', is_error: false, num_turns: 1, total_cost_usd: 0, usage: {}, modelUsage: {} });
      return;
    }
  }
  private push(m: any): void {
    this.out.push({ session_id: 'summary', uuid: fakeUuid(), ...m });
    this.wake?.();
  }
  async interrupt(): Promise<unknown> {
    return {};
  }
  close(): void {
    this.closed = true;
    this.wake?.();
  }
  async *[Symbol.asyncIterator](): AsyncIterator<SDKMessage> {
    for (;;) {
      while (this.out.length) yield this.out.shift()!;
      if (this.closed) return;
      await new Promise<void>((r) => (this.wake = r));
      this.wake = undefined;
    }
  }
}

// ---- the fake gff-hab route ----

export class FakeHab {
  readonly token = 'ab'.repeat(16); // the engine route's
  readonly paneToken = 'cd'.repeat(16); // GFF_HAB's, for the MCP child
  socket = '';
  received: (Msg & { conn: number })[] = [];
  ackEngine = true;
  hello: Record<string, unknown> = {};
  // The answer to each context.request; null leaves it unanswered.
  context: ((m: Msg) => Record<string, unknown>) | null = () => ({ summary: '', through: '', recent: [], seed: {} });
  connections = 0;
  refused = 0;
  private server?: http.Server;
  private sock?: net.Socket;
  private seq = 0;

  async start(dir: string): Promise<void> {
    this.socket = path.join(dir, 'h.sock');
    this.server = http.createServer((_req, res) => {
      res.writeHead(400);
      res.end();
    });
    this.server.on('upgrade', (req, socket: net.Socket) => {
      if (req.url !== `/capcom/${this.token}` || req.headers.upgrade !== 'gff-capcom/1') {
        this.refused++;
        socket.end('HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n');
        return;
      }
      socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: gff-capcom/1\r\nConnection: Upgrade\r\n\r\n');
      this.sock?.destroy();
      this.sock = socket;
      this.seq = 0;
      const conn = ++this.connections;
      let buf = '';
      socket.on('data', (c) => {
        buf += c.toString('utf8');
        let i;
        while ((i = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, i);
          buf = buf.slice(i + 1);
          if (!line) continue;
          const m = JSON.parse(line) as Msg;
          this.received.push({ ...m, conn });
          if (m.type !== 'ack' && this.ackEngine && socket === this.sock) socket.write(JSON.stringify({ type: 'ack', ack: m.seq }) + '\n');
          if (m.type === 'context.request' && this.context && socket === this.sock) {
            this.send('context', { request: m.request, for: m.for, ...this.context(m) });
          }
        }
      });
      socket.on('error', () => {});
      this.send('hello', this.hello);
    });
    await new Promise<void>((r) => this.server!.listen(this.socket, r));
  }

  get addr(): string {
    return `http+unix://${this.socket}/hooks/${this.paneToken}`;
  }

  send(type: string, fields: Record<string, unknown> = {}): number {
    const seq = ++this.seq;
    this.sock?.write(JSON.stringify({ seq, type, ...fields }) + '\n');
    return seq;
  }

  input(id: string, text: string, extra: Record<string, unknown> = {}): void {
    this.send('input', { item: { id, kind: 'message', author: 'user', time: '2026-01-01T10:00:00Z', tz: 'UTC', text, images: [], context: [], ...extra } });
  }

  ack(n: number): void {
    this.sock?.write(JSON.stringify({ type: 'ack', ack: n }) + '\n');
  }

  drop(): void {
    this.sock?.destroy();
    this.sock = undefined;
  }

  of(type: string): (Msg & { conn: number })[] {
    return this.received.filter((m) => m.type === type);
  }

  wait(type: string, pred: (m: Msg) => boolean = () => true, ms = 5000): Promise<Msg & { conn: number }> {
    return until(() => this.received.find((m) => m.type === type && pred(m)), ms, `${type} message`);
  }

  async close(): Promise<void> {
    this.sock?.destroy();
    await new Promise<void>((r) => this.server?.close(() => r()) ?? r());
  }
}

// ---- one engine with its fakes and temp folder ----

export const PLANTED = 'planted-credential-value-0123456789';

export interface Rig {
  dir: string;
  cfg: string;
  cwd: string;
  hab: FakeHab;
  sdk: FakeSdk;
  engine: Engine;
  logs: string[];
  exitCode: () => number | undefined;
  stop: () => Promise<void>;
}

export async function rig(opts: { hello?: Record<string, unknown>; setup?: (r: { cfg: string; cwd: string; sdk: FakeSdk }) => Promise<void> | void } = {}): Promise<Rig> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-'));
  const cfg = path.join(dir, 'cfg');
  const cwd = path.join(dir, 'engine');
  await fs.mkdir(cfg);
  await fs.mkdir(cwd);
  const hab = new FakeHab();
  hab.hello = {
    credential: { kind: 'oauth_token', value: PLANTED },
    device: 'dev1',
    last_item: '',
    sdk_session: '',
    settings: { model: 'fake-model', small_model: 'fake-small', max_turns: 7, max_turn_usd: 1.5, wake_retry_ms: 10000 },
    ...opts.hello,
  };
  const sdk = new FakeSdk();
  await opts.setup?.({ cfg, cwd, sdk });
  await hab.start(dir);
  const logs: string[] = [];
  let code: number | undefined;
  const engine = new Engine({
    query: sdk.query,
    env: { PATH: '/usr/bin:/bin', GFF_HAB: hab.addr, GFF_HAB_CAPCOM_TOKEN: hab.token, CLAUDE_CONFIG_DIR: cfg, CLAUDE_CODE_OAUTH_TOKEN: 'inherited-should-go', ANTHROPIC_API_KEY: 'inherited-should-go' },
    cwd,
    exit: (c) => {
      code = c;
    },
    log: (l) => logs.push(l),
    backoffStartMs: 50,
    flushTimeoutMs: 1000,
  });
  engine.start();
  return {
    dir,
    cfg,
    cwd,
    hab,
    sdk,
    engine,
    logs,
    exitCode: () => code,
    stop: async () => {
      for (const q of sdk.queries) q.close();
      (engine as any).route?.close();
      await hab.close();
      await fs.rm(dir, { recursive: true, force: true });
    },
  };
}

// Wait until the engine passed its start check and can take input.
export async function started(r: Rig): Promise<void> {
  await r.hab.wait('ready');
  await r.hab.wait('session.started');
  await until(() => (r.engine as any).ready, 5000, 'engine ready');
}
