// Test doubles for the Codex engine: a fake Codex SDK that runs no CLI and makes no model call,
// writes each thread's rollout under a throwaway CODEX_HOME, and for the start check posts a
// Responses request with its tool list to the engine's loopback endpoint, as the CLI does.
import { randomUUID } from 'node:crypto';
import { promises as fs } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { CodexOptions, Input, ThreadEvent, ThreadItem, ThreadOptions, TurnOptions } from '@openai/codex-sdk';
import { CodexEngine, type CodexLike, type CodexThreadLike } from '../src/codex.js';
import { SUMMARY_SYSTEM } from '../src/context.js';
import { FakeHab, PLANTED, until } from './helpers.js';

// The tools of a gpt-5.5 request under the engine's settings (probed with the pinned CLI).
export const GOOD_TOOLS: unknown[] = [
  { type: 'function', name: 'list_mcp_resources' },
  { type: 'function', name: 'list_mcp_resource_templates' },
  { type: 'function', name: 'read_mcp_resource' },
  { type: 'function', name: 'request_user_input' },
  { type: 'custom', name: 'apply_patch' },
  { type: 'function', name: 'tool_search' },
  { type: 'web_search' },
  { type: 'namespace', name: 'mcp__flightdeck', tools: [{ type: 'function', name: 'hosts' }] },
];

export interface CodexTurnCtx {
  readonly input: Input;
  readonly text: string; // the input's text
  readonly images: string[];
  readonly thread: FakeThread;
  readonly signal?: AbortSignal;
  say(text: string): void;
  mcp(tool: string, args: Record<string, unknown>, server?: string): void;
  search(query: string): void;
  item(item: Partial<ThreadItem> & { type: string }): void;
  fail(message: string): void; // turn.failed, then the CLI exits 1
  context(used: number): Promise<void>; // a token_count at this share of the window
  until(f: () => boolean): Promise<void>;
}
export type CodexScript = (ctx: CodexTurnCtx) => Promise<void>;

export interface CodexTurnRec {
  thread: FakeThread;
  input: Input;
  text: string;
}

const echo: CodexScript = async (ctx) => {
  ctx.say(`echo ${ctx.text.split('\n').filter((l) => l.startsWith('[flightdeck item ')).length}`);
};

export class FakeCodex {
  options: CodexOptions[] = [];
  threads: FakeThread[] = [];
  turns: CodexTurnRec[] = [];
  summaries: { options: CodexOptions; thread: ThreadOptions; prompt: string }[] = [];
  probes = 0;
  offered: unknown[] = GOOD_TOOLS;
  script: CodexScript = echo;
  summaryText: string | undefined = 'fake summary';
  summaryDelayMs = 0;
  startTurn = true; // false: the CLI exits before turn.started
  constructor(readonly home: string) {}

  factory = (o: CodexOptions): CodexLike => {
    this.options.push(o);
    return {
      startThread: (t?: ThreadOptions) => this.thread(o, t ?? {}, null),
      resumeThread: (id: string, t?: ThreadOptions) => this.thread(o, t ?? {}, id),
    };
  };

  private thread(o: CodexOptions, t: ThreadOptions, id: string | null): FakeThread {
    const th = new FakeThread(this, o, t, id);
    this.threads.push(th);
    return th;
  }

  // The rollout file of a thread, as the CLI writes it.
  rolloutPath(id: string): string {
    return path.join(this.home, 'sessions', '2026', '11', '07', `rollout-2026-11-07T00-00-00-${id}.jsonl`);
  }

  async appendRollout(id: string, recs: unknown[]): Promise<void> {
    const p = this.rolloutPath(id);
    await fs.mkdir(path.dirname(p), { recursive: true });
    await fs.appendFile(p, recs.map((r) => JSON.stringify(r)).join('\n') + '\n');
  }

  texts(): string[] {
    return this.turns.map((t) => t.text);
  }
}

function textOf(input: Input): { text: string; images: string[] } {
  if (typeof input === 'string') return { text: input, images: [] };
  const text: string[] = [];
  const images: string[] = [];
  for (const p of input) {
    if (p.type === 'text') text.push(p.text);
    else images.push(p.path);
  }
  return { text: text.join('\n\n'), images };
}

class AbortErr extends Error {
  name = 'AbortError';
}

export class FakeThread implements CodexThreadLike {
  private _id: string | null;
  constructor(
    private fake: FakeCodex,
    readonly codexOptions: CodexOptions,
    readonly threadOptions: ThreadOptions,
    id: string | null,
  ) {
    this._id = id;
  }
  get id(): string | null {
    return this._id;
  }

  async runStreamed(input: Input, opts: TurnOptions = {}): Promise<{ events: AsyncGenerator<ThreadEvent> }> {
    return { events: this.run(input, opts) };
  }

  private async *run(input: Input, opts: TurnOptions): AsyncGenerator<ThreadEvent> {
    const o = this.codexOptions;
    const { text, images } = textOf(input);
    if (o.baseUrl?.startsWith('http://127.0.0.1')) {
      // The start check: post the request the CLI would, with the tools it offers.
      this.fake.probes++;
      const res = await fetch(`${o.baseUrl}/responses`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: `Bearer ${o.apiKey}` },
        body: JSON.stringify({ model: this.threadOptions.model, tools: this.fake.offered, input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text }] }] }),
      });
      await res.text();
      yield { type: 'thread.started', thread_id: randomUUID() };
      yield { type: 'turn.started' };
      yield { type: 'item.completed', item: { id: 'i0', type: 'agent_message', text: 'ok' } };
      yield { type: 'turn.completed', usage: zeroUsage() };
      return;
    }
    if ((o.config as any)?.developer_instructions === SUMMARY_SYSTEM) {
      this.fake.summaries.push({ options: o, thread: this.threadOptions, prompt: text });
      if (this.fake.summaryDelayMs) await abortable(sleepMs(this.fake.summaryDelayMs), opts.signal);
      yield { type: 'thread.started', thread_id: randomUUID() };
      yield { type: 'turn.started' };
      if (this.fake.summaryText === undefined) {
        yield { type: 'turn.failed', error: { message: 'summary failed' } };
        throw new Error('Codex Exec exited with code 1');
      }
      yield { type: 'item.completed', item: { id: 's0', type: 'agent_message', text: this.fake.summaryText } };
      yield { type: 'turn.completed', usage: zeroUsage() };
      return;
    }
    const fresh = this._id === null;
    if (fresh) this._id = randomUUID();
    const id = this._id!;
    this.fake.turns.push({ thread: this, input, text });
    yield { type: 'thread.started', thread_id: id };
    if (!this.fake.startTurn) throw new Error('Codex Exec exited with code 1: could not start');
    const recs: unknown[] = [];
    if (fresh) recs.push({ type: 'session_meta', payload: { id, cwd: this.threadOptions.workingDirectory } });
    recs.push({ type: 'response_item', payload: { type: 'message', role: 'user', content: [{ type: 'input_text', text }] } });
    await this.fake.appendRollout(id, recs);
    yield { type: 'turn.started' };
    const queue: ThreadEvent[] = [];
    let wake: (() => void) | undefined;
    let done = false;
    let failed: string | undefined;
    let n = 0;
    const push = (e: ThreadEvent) => {
      queue.push(e);
      wake?.();
    };
    const ctx: CodexTurnCtx = {
      input,
      text,
      images,
      thread: this,
      signal: opts.signal,
      say: (t) => push({ type: 'item.completed', item: { id: `m${++n}`, type: 'agent_message', text: t } }),
      mcp: (tool, args, server = 'flightdeck') => {
        const it = { id: `c${++n}`, type: 'mcp_tool_call' as const, server, tool, arguments: args, status: 'in_progress' as const };
        push({ type: 'item.started', item: it });
        push({ type: 'item.completed', item: { ...it, status: 'completed' } });
      },
      search: (query) => {
        push({ type: 'item.started', item: { id: `w${++n}`, type: 'web_search', query: '' } });
        push({ type: 'item.completed', item: { id: `w${n}`, type: 'web_search', query } });
      },
      item: (it) => push({ type: 'item.started', item: { id: `x${++n}`, ...it } as ThreadItem }),
      fail: (message) => {
        failed = message;
        push({ type: 'turn.failed', error: { message } });
      },
      context: (used) =>
        this.fake.appendRollout(id, [
          { type: 'event_msg', payload: { type: 'token_count', info: { last_token_usage: { input_tokens: Math.round(used * 100000), output_tokens: 0 }, model_context_window: 100000 } } },
        ]),
      until: async (f) => {
        while (!f()) {
          if (opts.signal?.aborted) throw new AbortErr('aborted');
          await sleepMs(5);
        }
      },
    };
    const script = this.fake.script(ctx).then(
      () => {
        done = true;
        wake?.();
      },
      (e) => {
        done = true;
        if (!(e instanceof AbortErr)) failed = String(e);
        wake?.();
      },
    );
    for (;;) {
      if (opts.signal?.aborted) throw new AbortErr('The operation was aborted');
      while (queue.length) yield queue.shift()!;
      if (done) break;
      await new Promise<void>((r) => {
        wake = r;
        const onAbort = () => r();
        opts.signal?.addEventListener('abort', onAbort, { once: true });
        setTimeout(r, 20);
      });
      wake = undefined;
    }
    await script;
    if (opts.signal?.aborted) throw new AbortErr('The operation was aborted');
    while (queue.length) yield queue.shift()!;
    if (failed !== undefined) throw new Error(`Codex Exec exited with code 1: ${failed}`);
    yield { type: 'turn.completed', usage: { ...zeroUsage(), input_tokens: 120, output_tokens: 7 } };
  }
}

function zeroUsage() {
  return { input_tokens: 0, cached_input_tokens: 0, cache_write_input_tokens: 0, output_tokens: 0, reasoning_output_tokens: 0 };
}

function sleepMs(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

function abortable<T>(p: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return p;
  return new Promise((resolve, reject) => {
    signal.addEventListener('abort', () => reject(new AbortErr('aborted')), { once: true });
    p.then(resolve, reject);
  });
}

export interface CodexRig {
  dir: string;
  home: string; // the throwaway CODEX_HOME
  cwd: string;
  hab: FakeHab;
  codex: FakeCodex;
  engine: CodexEngine;
  logs: string[];
  exitCode: () => number | undefined;
  stop: () => Promise<void>;
}

export const MCP_JSON = { mcpServers: { flightdeck: { type: 'stdio', command: '/opt/gff/gff-hab', args: ['mcp'] } } };

export async function codexRig(
  opts: { hello?: Record<string, unknown>; setup?: (r: { home: string; cwd: string; codex: FakeCodex }) => Promise<void> | void; mcp?: unknown } = {},
): Promise<CodexRig> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-codex-'));
  const home = path.join(dir, 'codex-home');
  const cwd = path.join(dir, 'engine');
  await fs.mkdir(home);
  await fs.mkdir(cwd);
  await fs.writeFile(path.join(cwd, '.mcp.json'), JSON.stringify(opts.mcp ?? MCP_JSON));
  const hab = new FakeHab();
  hab.hello = {
    credential: { kind: 'api_key', value: PLANTED },
    device: 'dev1',
    last_item: '',
    sdk_session: '',
    settings: { model: 'fake-codex-model', small_model: 'fake-codex-small', max_turns: 7, max_turn_usd: 1.5, wake_retry_ms: 10000 },
    ...opts.hello,
  };
  const codex = new FakeCodex(home);
  await opts.setup?.({ home, cwd, codex });
  await hab.start(dir);
  const logs: string[] = [];
  let code: number | undefined;
  const engine = new CodexEngine({
    codex: codex.factory,
    env: {
      PATH: '/usr/bin:/bin',
      GFF_HAB: hab.addr,
      GFF_HAB_CAPCOM_TOKEN: hab.token,
      CODEX_HOME: home,
      OPENAI_API_KEY: 'inherited-should-go',
      CODEX_API_KEY: 'inherited-should-go',
    },
    cwd,
    exit: (c) => {
      code = c;
    },
    log: (l) => logs.push(l),
    backoffStartMs: 50,
    flushTimeoutMs: 1000,
    startCheckTimeoutMs: 5000,
  });
  engine.start();
  return {
    dir,
    home,
    cwd,
    hab,
    codex,
    engine,
    logs,
    exitCode: () => code,
    stop: async () => {
      (engine as any).turn?.ac.abort();
      (engine as any).route?.close();
      await hab.close();
      await fs.rm(dir, { recursive: true, force: true });
    },
  };
}

// codexStarted waits until the engine passed its start check and can take input.
export async function codexStarted(r: CodexRig): Promise<void> {
  await r.hab.wait('ready');
  await until(() => (r.engine as any).ready, 5000, 'engine ready');
}
