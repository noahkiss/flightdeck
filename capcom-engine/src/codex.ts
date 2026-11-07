// The Capcom engine on the Codex SDK (docs/specs/capcom-service.md 4 on Codex, stage A6). It
// speaks the same wire format as the Claude engine (engine.ts) and declares what Codex lacks
// (4.1): no mid-turn input (gff-hab holds a message until the turn ends), no approval callback
// (no tool that needs a card: Codex has no web fetch), no compaction events (gff-hab re-seeds
// earlier), no usage events (the governor counts tokens).
//
// The Codex SDK runs `codex exec` once per turn on a thread it can resume by id. The engine runs
// the CLI through a wrapper (codexbin.ts) that adds --ignore-user-config and --ignore-rules, so
// the user's config.toml, MCP servers and rules never load; the thread still lands in the user's
// CODEX_HOME, where `codex resume <id>` finds it (11).
import { promises as fs } from 'node:fs';
import path from 'node:path';
import type { CodexOptions, Input, ThreadEvent, ThreadItem, ThreadOptions, TurnOptions, UserInput } from '@openai/codex-sdk';
import { ENGINE_TOKEN_VAR, engineToken, parseHabAddr } from './addr.js';
import { cleanSummary, seedText, SUMMARY_SYSTEM, summaryPrompt } from './context.js';
import {
  ContextAsks,
  type ContextMsg,
  type Credential,
  defaultSettings,
  helloReseed,
  type Item,
  itemLines,
  newTurnId,
  originOf,
  parseContext,
  parseCredential,
  parseSettings,
  redactor,
  type Settings,
} from './contract.js';
import { CODEX_FEATURES_OFF, offeredTools, onCodexSurface, TURN_ITEMS } from './codexpolicy.js';
import { toolSummary } from './policy.js';
import { codexHome, findRollout, rolloutContext, rolloutHandedIds } from './rollout.js';
import { Route, type Msg } from './route.js';
import { START_CHECK_KEY, startProbe } from './startcheck.js';
import { CODEX_SDK_VERSION, ENGINE_VERSION } from './version.js';

// What the Codex engine has (4.1). It lacks mid_turn_input, approval_callback,
// compaction_events and usage_events.
export const CODEX_CAPABILITIES = ['images', 'interrupt', 'resume_by_id'];

// The model when [capcom] model is unset. The pinned CLI's newer models are code-mode only:
// their requests carry a JavaScript `exec` tool and sub-agent tools, which the start check
// refuses. This is the newest bundled model whose request fits 5.1.
export const CODEX_DEFAULT_MODEL = 'gpt-5.5';

// The text of the start check's one turn (startcheck.ts).
export const CODEX_START_CHECK_TEXT = '[flightdeck start check]';

// The wrapper adds --ephemeral when this is set: the start check and the summary save no thread.
export const EPHEMERAL_VAR = 'GFF_CODEX_EPHEMERAL';

// The part of the SDK the engine uses.
export interface CodexThreadLike {
  readonly id: string | null;
  runStreamed(input: Input, options?: TurnOptions): Promise<{ events: AsyncGenerator<ThreadEvent> }>;
}
export interface CodexLike {
  startThread(options?: ThreadOptions): CodexThreadLike;
  resumeThread(id: string, options?: ThreadOptions): CodexThreadLike;
}
export type CodexFactory = (options: CodexOptions) => CodexLike;

export interface CodexDeps {
  codex: CodexFactory;
  codexPath?: string; // the wrapper; unset in tests, whose fake SDK runs no CLI
  env: NodeJS.ProcessEnv;
  cwd: string;
  exit: (code: number) => void;
  log?: (line: string) => void;
  backoffStartMs?: number;
  flushTimeoutMs?: number;
  startCheckTimeoutMs?: number;
}

const IMAGE_EXT = new Set(['.png', '.jpg', '.jpeg', '.gif', '.webp']);

// Variables the CLI never gets from the engine's environment: the engine route's token (3.3)
// and any OpenAI key not from hello.
const DROP_ENV = new Set([ENGINE_TOKEN_VAR, 'OPENAI_API_KEY', 'CODEX_API_KEY', EPHEMERAL_VAR, 'GFF_HAB_CAPCOM_KIND']);

interface Turn {
  id: string;
  origin: 'user' | 'event';
  items: string[];
  part: number;
  interrupted: boolean;
  ac: AbortController;
  authFailed: boolean;
  violation: string;
  taken: boolean;
  tools: Set<string>; // the item ids already shown as a tool line
}

function isAuthFailure(text: string): boolean {
  return /\b401\b|unauthori[sz]ed|invalid_api_key|not logged in|log ?in again/i.test(text);
}

export class CodexEngine {
  private d: CodexDeps;
  private route!: Route;
  private credential?: Credential;
  private settings: Settings = defaultSettings();
  private started = false;
  private mcp?: { command: string; args: string[] };

  private gen = 0;
  private thread?: CodexThreadLike;
  private sessionId = '';
  private sessionWhy = 'first';
  private rollout?: string;
  private resumeUnproven = false; // a resumed thread whose CLI has not yet started a turn
  private seed = ''; // a fresh session's seed: it joins the first message (6.2)
  private ready = false;
  private checked = false;
  private handed = new Set<string>();
  private pending: Item[] = [];

  private turn?: Turn;
  private shuttingDown = false;
  private reseedPending?: string;
  private reseeding = false;
  private summarizing?: Promise<void>;
  private asks = new ContextAsks();
  private exiting = false;

  constructor(d: CodexDeps) {
    this.d = d;
  }

  start(): void {
    const addr = parseHabAddr(this.d.env.GFF_HAB);
    this.route = new Route({
      socket: addr.socket,
      token: engineToken(this.d.env[ENGINE_TOKEN_VAR]),
      backoffStartMs: this.d.backoffStartMs,
      onHello: (m) => this.onHello(m),
      onMessage: (m) => this.onMessage(m),
      log: (l) => this.log(l),
    });
    this.route.start();
  }

  // ---- logging ----

  private redact = redactor(() => this.credential?.value);

  private log(line: string): void {
    (this.d.log ?? ((l: string) => process.stderr.write(l + '\n')))(this.redact(line));
  }

  private send(type: string, fields: Record<string, unknown> = {}): void {
    this.route.send(type, fields);
  }

  private sendError(text: string, kind: '' | 'auth' | 'start', exiting: boolean): void {
    this.send('error', { text: this.redact(text), kind, exiting });
  }

  // ---- gff-hab messages ----

  private onHello(m: Msg): Record<string, unknown> {
    const s = (m.settings ?? {}) as Record<string, unknown>;
    if (typeof s.route_backoff_max_ms === 'number') this.route.setBackoffMax(s.route_backoff_max_ms);
    this.settings = parseSettings(s);
    if (!this.started) {
      this.started = true;
      this.credential = parseCredential(m.credential, ['login', 'api_key']);
      const resume = typeof m.sdk_session === 'string' ? m.sdk_session : '';
      const opening = helloReseed(m);
      queueMicrotask(() => {
        if (!this.credential) {
          this.sendError('hello carried no Codex login or API key', 'auth', true);
          void this.exit(1);
          return;
        }
        if (opening) {
          this.reseedPending = opening;
          void this.doReseed();
          return;
        }
        void this.startSession(resume ? 'restart' : 'first', resume);
      });
    }
    const ready: Record<string, unknown> = { engine: 'codex', version: ENGINE_VERSION, sdk_version: CODEX_SDK_VERSION, capabilities: CODEX_CAPABILITIES };
    if (this.turn) ready.turn = { turn: this.turn.id, origin: this.turn.origin };
    return ready;
  }

  private onMessage(m: Msg): void {
    switch (m.type) {
      case 'input':
        this.onInput(m.item as Item);
        break;
      case 'interrupt':
        this.interrupt();
        break;
      case 'reseed':
        this.reseed(typeof m.reason === 'string' ? m.reason : '');
        break;
      case 'shutdown':
        this.shuttingDown = true;
        if (!this.turn) void this.exit(0);
        break;
      case 'context':
        this.onContext(m);
        break;
      default:
        // approval.answer: the engine opens no card. pace: the governor's stage. Unknown
        // types are acked and ignored.
        break;
    }
  }

  private onInput(item: Item): void {
    if (!item || typeof item.id !== 'string' || !item.id) return;
    if (this.shuttingDown || this.exiting) return;
    if (this.handed.has(item.id) || this.pending.some((p) => p.id === item.id)) return;
    this.pending.push(item);
    this.pump();
  }

  // pump runs the next turn when the engine is idle: every item that waits goes into it, in
  // arrival order. Codex takes no input during a turn; gff-hab holds new items until the turn
  // ends (4.1), and any that arrive anyway wait here.
  private pump(): void {
    if (!this.ready || this.turn || this.reseedPending || this.reseeding || this.shuttingDown || this.exiting) return;
    const items = this.pending.filter((i) => !this.handed.has(i.id));
    this.pending = [];
    if (items.length) void this.runTurn(items);
  }

  private interrupt(): void {
    if (!this.turn) return;
    this.turn.interrupted = true;
    this.turn.ac.abort();
  }

  private reseed(reason: string): void {
    if (this.reseeding && reason !== 'new') return;
    this.reseedPending = reason || 'reseed';
    this.maybeReseed();
  }

  private maybeReseed(): void {
    if (this.reseedPending && !this.reseeding && !this.turn) void this.doReseed();
  }

  // ---- the context manager's half (6) ----

  private onContext(m: Msg): void {
    if (this.asks.answer(m)) return;
    const c = parseContext(m);
    if (m.for === 'quiet' && !this.turn && !this.reseeding && !this.summarizing && c.recent.length) {
      void this.updateSummary(c, this.settings.precompact_timeout_ms);
    }
  }

  private async updateSummary(c: ContextMsg, ms: number): Promise<string> {
    if (!c.recent.length) return c.summary;
    if (this.summarizing) await this.summarizing;
    let text = c.summary;
    const run = (async () => {
      const got = await this.summaryCall(c, ms);
      if (got === undefined) return;
      text = cleanSummary(got, this.settings.summary_max_chars);
      if (text) this.send('summary', { text, through: c.through });
      else text = c.summary;
    })();
    this.summarizing = run;
    try {
      await run;
    } finally {
      if (this.summarizing === run) this.summarizing = undefined;
    }
    return text;
  }

  // The summary call: its own thread on the small model, in an empty folder, with no MCP server,
  // no web search, nothing saved (--ephemeral), the summary rules as developer instructions.
  private async summaryCall(c: ContextMsg, ms: number): Promise<string | undefined> {
    const dir = path.join(this.d.cwd, '.summary');
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), Math.max(1, ms));
    try {
      await fs.mkdir(dir, { recursive: true, mode: 0o700 });
      const codex = this.d.codex(this.codexOptions({ mcp: false, ephemeral: true, developer: SUMMARY_SYSTEM }));
      const th = codex.startThread({
        ...this.threadOptions(this.settings.small_model ?? this.settings.model ?? CODEX_DEFAULT_MODEL),
        workingDirectory: dir,
        webSearchMode: 'disabled',
      });
      const { events } = await th.runStreamed(summaryPrompt(c.summary, c.recent, this.settings.summary_max_chars), { signal: ac.signal });
      let out: string | undefined;
      let done = false;
      for await (const ev of events) {
        if (ev.type === 'item.completed' && ev.item.type === 'agent_message') out = ev.item.text;
        if (ev.type === 'turn.completed') done = true;
        if (ev.type === 'turn.failed') break;
      }
      if (done && out !== undefined) return out;
    } catch (e) {
      if (!ac.signal.aborted) this.log(`summary: ${(e as Error).message}`);
    } finally {
      clearTimeout(timer);
    }
    this.log('summary: no new summary; the old one stays');
    return undefined;
  }

  // A re-seed (6.2): the material, the summary (none for a new conversation), then a fresh
  // thread whose first message carries the seed.
  private async doReseed(): Promise<void> {
    if (this.reseeding) return;
    this.reseeding = true;
    try {
      for (;;) {
        const reason = this.reseedPending ?? 'reseed';
        const ms = this.settings.precompact_timeout_ms;
        const end = Date.now() + ms;
        const c = await this.asks.ask((t, f) => this.send(t, f), 'reseed', ms);
        if (this.exiting) return;
        let summary = '';
        if (reason !== 'new' && c) summary = await this.updateSummary(c, end - Date.now());
        if (this.exiting) return;
        if (this.reseedPending === 'new' && reason !== 'new') continue;
        if (this.summarizing) await this.summarizing;
        if (this.exiting) return;
        this.reseedPending = undefined;
        await this.startSession(reason === 'new' ? 'new' : 'reseed', '', seedText(reason, summary, c?.seed));
        break;
      }
    } finally {
      this.reseeding = false;
    }
    if (!this.exiting) {
      this.maybeReseed();
      this.pump();
    }
  }

  // ---- the Codex thread ----

  // The flightdeck MCP server, from the .mcp.json gff-hab writes in the engine folder (3.4).
  private async loadMcp(): Promise<void> {
    if (this.mcp) return;
    const raw = await fs.readFile(path.join(this.d.cwd, '.mcp.json'), 'utf8');
    const f = JSON.parse(raw)?.mcpServers?.flightdeck;
    if (!f || typeof f.command !== 'string' || !f.command) throw new Error('.mcp.json names no flightdeck server');
    this.mcp = { command: f.command, args: Array.isArray(f.args) ? f.args.map(String) : [] };
  }

  codexOptions(o: { mcp: boolean; ephemeral?: boolean; developer?: string; baseUrl?: string; apiKey?: string; home?: string }): CodexOptions {
    const env: Record<string, string> = {};
    for (const [k, v] of Object.entries(this.d.env)) if (v !== undefined && !DROP_ENV.has(k)) env[k] = v;
    if (o.home) env.CODEX_HOME = o.home;
    if (o.ephemeral) env[EPHEMERAL_VAR] = '1';
    const config: Record<string, any> = { features: { ...CODEX_FEATURES_OFF } };
    if (o.mcp && this.mcp) {
      // Codex hands an MCP server only a short list of variables; GFF_HAB must reach gff-hab mcp
      // (3.3), and nothing names it on the command line.
      config.mcp_servers = { flightdeck: { command: this.mcp.command, args: this.mcp.args, env_vars: ['GFF_HAB'] } };
    }
    if (o.developer) config.developer_instructions = o.developer;
    const out: CodexOptions = { env, config };
    if (this.d.codexPath) out.codexPathOverride = this.d.codexPath;
    if (o.baseUrl) out.baseUrl = o.baseUrl;
    const key = o.apiKey ?? (this.credential?.kind === 'api_key' ? this.credential.value : undefined);
    if (key) out.apiKey = key;
    return out;
  }

  threadOptions(model = this.settings.model ?? CODEX_DEFAULT_MODEL): ThreadOptions {
    return {
      model,
      workingDirectory: this.d.cwd,
      skipGitRepoCheck: true,
      sandboxMode: 'read-only',
      approvalPolicy: 'never',
      networkAccessEnabled: false,
      webSearchMode: 'live',
    };
  }

  // The start check (5.1 on Codex), once per engine process.
  private async startCheck(): Promise<boolean> {
    if (this.checked) return true;
    const fail = async (text: string): Promise<false> => {
      this.sendError(`start check: ${text}`, 'start', true);
      await this.exit(1);
      return false;
    };
    try {
      await this.loadMcp();
    } catch (e) {
      return fail((e as Error).message);
    }
    const probe = await startProbe();
    const home = path.join(this.d.cwd, '.start-check');
    try {
      await fs.rm(home, { recursive: true, force: true });
      await fs.mkdir(home, { recursive: true, mode: 0o700 });
      const codex = this.d.codex(this.codexOptions({ mcp: true, ephemeral: true, baseUrl: probe.url, apiKey: START_CHECK_KEY, home }));
      const th = codex.startThread(this.threadOptions());
      const ac = new AbortController();
      const timer = setTimeout(() => ac.abort(), this.d.startCheckTimeoutMs ?? 60_000);
      try {
        const { events } = await th.runStreamed(CODEX_START_CHECK_TEXT, { signal: ac.signal });
        for await (const _ev of events) {
          // the probe's one turn runs to its end
        }
      } catch (e) {
        if (probe.body() === undefined) this.log(`start check: ${(e as Error).message}`);
      } finally {
        clearTimeout(timer);
        ac.abort();
      }
      const body = probe.body();
      if (!body) return fail('the Codex CLI made no model request');
      const bad = offeredTools(body).filter((t) => !onCodexSurface(t));
      if (bad.length) return fail(`tool outside the Capcom surface: ${bad.join(', ')}`);
      this.checked = true;
      return true;
    } finally {
      await probe.close();
      await fs.rm(home, { recursive: true, force: true }).catch(() => {});
    }
  }

  private async startSession(why: 'first' | 'restart' | 'reseed' | 'new', resume = '', seed = ''): Promise<void> {
    const gen = ++this.gen;
    this.ready = false;
    if (!(await this.startCheck())) return;
    if (gen !== this.gen || this.exiting) return;
    this.handed = new Set();
    this.rollout = undefined;
    const codex = this.d.codex(this.codexOptions({ mcp: true }));
    if (resume) {
      const file = await findRollout(codexHome(this.d.env), resume);
      if (gen !== this.gen || this.exiting) return;
      if (!file) {
        // The thread will not resume: a re-seed replaces it (3.4).
        this.sendError(`resume of ${resume} failed: no rollout file under ${path.join(codexHome(this.d.env), 'sessions')}`, '', false);
        this.reseedPending = 'restart_failed';
        this.maybeReseed();
        return;
      }
      this.handed = await rolloutHandedIds(file);
      this.rollout = file;
      this.resumeUnproven = true;
      this.thread = codex.resumeThread(resume, this.threadOptions());
      this.sessionId = resume;
      this.sessionWhy = 'restart';
      this.seed = '';
      this.send('session.started', { session: resume, why: 'restart' });
    } else {
      // A Codex thread gets its id at its first turn. A re-seed or a new conversation says so at
      // once with no id: gff-hab ends the old session and adds the divider; the id follows at the
      // first turn (4.4).
      this.thread = codex.startThread(this.threadOptions());
      this.resumeUnproven = false;
      this.sessionId = '';
      this.sessionWhy = why;
      this.seed = seed;
      if (why === 'reseed' || why === 'new') this.send('session.started', { session: '', why });
    }
    this.ready = true;
    this.pump();
  }

  private async toInput(items: Item[]): Promise<Input> {
    const texts: string[] = [];
    if (this.seed) texts.push(this.seed);
    const images: UserInput[] = [];
    for (const it of items) {
      const lines = itemLines(it);
      for (const p of it.images ?? []) {
        if (!IMAGE_EXT.has(path.extname(p).toLowerCase())) {
          lines.push(`[image ${path.basename(p)}: not a supported image type]`);
          continue;
        }
        try {
          await fs.access(p);
          images.push({ type: 'local_image', path: p });
        } catch {
          lines.push(`[image ${path.basename(p)}: could not be read]`);
        }
      }
      texts.push(lines.join('\n'));
    }
    const text = texts.join('\n\n');
    return images.length ? [{ type: 'text', text }, ...images] : text;
  }

  private async runTurn(items: Item[]): Promise<void> {
    const thread = this.thread!;
    for (const it of items) this.handed.add(it.id);
    const t: Turn = {
      id: newTurnId(),
      origin: originOf(items[0]!),
      items: items.map((i) => i.id),
      part: 0,
      interrupted: false,
      ac: new AbortController(),
      authFailed: false,
      violation: '',
      taken: false,
      tools: new Set(),
    };
    this.turn = t;
    this.send('turn.started', { turn: t.id, origin: t.origin, items: t.items });
    let result = 'error';
    let usage: { input_tokens?: number; output_tokens?: number } | undefined;
    try {
      const input = await this.toInput(items);
      const { events } = await thread.runStreamed(input, { signal: t.ac.signal });
      for await (const ev of events) {
        if (this.exiting) break;
        switch (ev.type) {
          case 'thread.started':
            this.resumeUnproven = false;
            this.onThreadStarted(ev.thread_id);
            break;
          case 'turn.started':
            this.resumeUnproven = false;
            this.take(t);
            break;
          case 'item.started':
          case 'item.updated':
          case 'item.completed':
            this.onItem(t, ev.type, ev.item);
            break;
          case 'turn.completed':
            usage = ev.usage;
            result = 'success';
            break;
          case 'turn.failed':
            if (isAuthFailure(ev.error?.message ?? '')) t.authFailed = true;
            this.log(`codex: the turn failed: ${ev.error?.message ?? ''}`);
            break;
          case 'error':
            this.log(`codex: ${ev.message}`);
            break;
        }
        if (t.violation) {
          t.ac.abort();
          break;
        }
      }
    } catch (e) {
      const msg = (e as Error).message ?? String(e);
      if (!t.interrupted && !t.violation && !this.exiting) {
        if (isAuthFailure(msg)) t.authFailed = true;
        this.log(`codex: ${msg.split('\n')[0]}`);
        if (result === 'success') result = 'error';
      }
    }
    if (t.interrupted) result = 'interrupted';
    else if (t.violation) result = 'error';
    if (!this.sessionId && thread.id) this.onThreadStarted(thread.id);
    if (!this.rollout && this.sessionId) this.rollout = await findRollout(codexHome(this.d.env), this.sessionId);
    const used = await rolloutContext(this.rollout);
    this.send('turn.ended', {
      turn: t.id,
      result,
      input_tokens: usage?.input_tokens ?? 0,
      output_tokens: usage?.output_tokens ?? 0,
      cost_usd: 0,
      context_used: used,
    });
    this.turn = undefined;
    if (t.violation) {
      this.sendError(`start check: ${t.violation}`, 'start', true);
      await this.exit(1);
      return;
    }
    if (t.authFailed) {
      this.sendError('the model provider refused the Codex login or API key', 'auth', true);
      await this.exit(1);
      return;
    }
    if (this.shuttingDown) {
      await this.exit(0);
      return;
    }
    if (!t.taken && !t.interrupted && this.resumeUnproven) {
      // The resumed thread's rollout is there but will not load (empty, cut short, another
      // format): the CLI fails before any event. A re-seed replaces it (3.4) instead of a crash
      // loop, and the untaken items go into the fresh session.
      this.resumeUnproven = false;
      for (const it of items) this.handed.delete(it.id);
      this.pending = [...items, ...this.pending];
      this.sendError(`resume of ${this.sessionId} failed: the Codex CLI did not start the turn`, '', false);
      this.reseedPending = 'restart_failed';
      this.maybeReseed();
      return;
    }
    if (!t.taken && !t.interrupted) {
      // The CLI never started the turn (it did not run, or exited at once): exit, so gff-hab
      // starts the engine again after restart_delay and sends the untaken items again (3.2,
      // 4.3), and its crash count stops a loop.
      this.sendError('the Codex CLI did not start the turn', '', true);
      await this.exit(1);
      return;
    }
    if (this.reseedPending) {
      this.maybeReseed();
      return;
    }
    this.pump();
  }

  private onThreadStarted(id: string): void {
    if (this.sessionId || !id) return;
    this.sessionId = id;
    this.send('session.started', { session: id, why: this.sessionWhy });
  }

  // The model has the turn's input once Codex starts the turn: each item is read (7.3), and a
  // seed is in the thread.
  private take(t: Turn): void {
    if (t.taken) return;
    t.taken = true;
    this.seed = '';
    for (const id of t.items) this.send('input.taken', { item: id });
  }

  private onItem(t: Turn, phase: string, item: ThreadItem): void {
    if (!TURN_ITEMS.has(item.type)) {
      t.violation = `tool outside the Capcom surface ran: ${item.type}`;
      return;
    }
    switch (item.type) {
      case 'mcp_tool_call':
        if (item.server !== 'flightdeck') {
          t.violation = `tool outside the Capcom surface ran: ${item.server}.${item.tool}`;
          return;
        }
        if (!t.tools.has(item.id)) {
          t.tools.add(item.id);
          const name = `mcp__flightdeck__${item.tool}`;
          this.send('tool', { turn: t.id, name, summary: this.redact(toolSummary(name, item.arguments)) });
        }
        break;
      case 'web_search':
        if (phase === 'item.completed' && !t.tools.has(item.id)) {
          t.tools.add(item.id);
          this.send('tool', { turn: t.id, name: 'WebSearch', summary: this.redact(item.query ?? '') });
        }
        break;
      case 'agent_message':
        if (phase === 'item.completed' && item.text.trim()) this.send('reply', { turn: t.id, part: t.part++, text: item.text });
        break;
      case 'error':
        if (phase === 'item.completed') this.log(`codex: ${item.message.split('\n')[0]}`);
        break;
      default:
        break;
    }
  }

  private async exit(code: number): Promise<void> {
    if (this.exiting) return;
    this.exiting = true;
    this.gen++;
    this.ready = false;
    this.turn?.ac.abort();
    this.asks.drain();
    await this.route.flush(this.d.flushTimeoutMs ?? 5000);
    this.route.close();
    this.d.exit(code);
  }
}
