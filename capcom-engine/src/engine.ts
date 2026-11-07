// The Capcom engine: drives one Claude Agent SDK session for gff-hab
// (docs/specs/capcom-service.md 3, 4, 5).
import { randomUUID } from 'node:crypto';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import type {
  CanUseTool,
  HookCallback,
  Options,
  PermissionResult,
  SDKMessage,
  SDKUserMessage,
} from '@anthropic-ai/claude-agent-sdk';
import { ENGINE_TOKEN_VAR, engineToken, parseHabAddr } from './addr.js';
import { ALLOW_TOOLS, BASE_TOOLS, DENY_TOOLS, onSurface, textOf, toolSummary, UrlSet } from './policy.js';
import { Route, type Msg } from './route.js';
import { configDir, findTranscript, handedIds, MARKER_RE } from './transcript.js';
import { cleanSummary, seedText, SUMMARY_SYSTEM, summaryPrompt } from './context.js';
import { ContextAsks, type ContextMsg, type Credential, defaultSettings, helloReseed, type Item, itemLines, newTurnId, originOf, parseContext, parseCredential, parseSettings, redactor, type Settings } from './contract.js';
import { ENGINE_VERSION, SDK_VERSION } from './version.js';

export const CAPABILITIES = [
  'images',
  'interrupt',
  'mid_turn_input',
  'approval_callback',
  'compaction_events',
  'resume_by_id',
  'usage_events',
];

// The text of the no-query message that makes the SDK report its tools
// before any input goes in.
export const START_CHECK_TEXT = '[flightdeck start check]';

// The part of the SDK's Query the engine uses.
export interface QueryHandle extends AsyncIterable<SDKMessage> {
  interrupt(): Promise<unknown>;
  close(): void;
}
export type QueryFn = (params: { prompt: AsyncIterable<SDKUserMessage>; options: Options }) => QueryHandle;

export interface EngineDeps {
  query: QueryFn;
  env: NodeJS.ProcessEnv;
  cwd: string;
  exit: (code: number) => void;
  log?: (line: string) => void;
  backoffStartMs?: number;
  flushTimeoutMs?: number;
}

export type { Credential, Item } from './contract.js';

interface Turn {
  id: string;
  origin: 'user' | 'event';
  part: number;
  interrupted: boolean;
  urls: UrlSet; // URLs seen in tool and search results of this turn
  approved: UrlSet; // URLs the user allowed through a card this turn
  lastContext: number;
  authFailed: boolean;
}

interface Approval {
  resolve: (r: PermissionResult) => void;
  url?: string;
  body: Record<string, unknown>; // the approval.request fields
}

// An input stream that stays open until ended.
class InputStream implements AsyncIterable<SDKUserMessage> {
  private q: SDKUserMessage[] = [];
  private wake?: () => void;
  private ended = false;
  push(m: SDKUserMessage): void {
    this.q.push(m);
    this.wake?.();
  }
  end(): void {
    this.ended = true;
    this.wake?.();
  }
  async *[Symbol.asyncIterator](): AsyncIterator<SDKUserMessage> {
    for (;;) {
      while (this.q.length) yield this.q.shift()!;
      if (this.ended) return;
      await new Promise<void>((r) => (this.wake = r));
      this.wake = undefined;
    }
  }
}

const IMAGE_TYPES: Record<string, string> = {
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.gif': 'image/gif',
  '.webp': 'image/webp',
};

export class Engine {
  private d: EngineDeps;
  private route!: Route;
  private credential?: Credential;
  private settings: Settings = defaultSettings();
  private started = false;

  // The current SDK session.
  private gen = 0;
  private q?: QueryHandle;
  private input?: InputStream;
  private sessionId = '';
  private ready = false; // start check passed and the probe answered
  private handed = new Set<string>();
  private userUrls = new UrlSet();
  private uuidToItem = new Map<string, string>();
  private uuidItems = new Map<string, Item>();
  private untaken = new Set<string>(); // uuids handed, not yet taken
  private pending: Item[] = [];
  private totalCost = 0;

  private turn?: Turn;
  private approvals = new Map<string, Approval>();
  private shuttingDown = false;
  private reseedPending?: string;
  private reseeding = false; // a re-seed is under way: the context, the summary, the fresh session
  private summarizing?: Promise<void>; // the summary call that runs
  private asks = new ContextAsks();
  private exiting = false;

  constructor(d: EngineDeps) {
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
      afterReady: () => this.resendApprovals(),
      log: (l) => this.log(l),
    });
    this.route.start();
  }

  // ---- logging and redaction ----

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
      this.credential = parseCredential(m.credential, ['oauth_token', 'api_key']);
      const resume = typeof m.sdk_session === 'string' ? m.sdk_session : '';
      const opening = helloReseed(m);
      queueMicrotask(() => {
        if (!this.credential) {
          this.sendError('hello carried no credential', 'auth', true);
          void this.exit(1);
          return;
        }
        if (opening) {
          // The thread's last session was another engine's, or a fresh session that never
          // started: open with a re-seed (4.4 hello `reseed`).
          this.reseedPending = opening;
          void this.doReseed();
          return;
        }
        void this.startSession(resume ? 'restart' : 'first', resume);
      });
    }
    const ready: Record<string, unknown> = { engine: 'claude', version: ENGINE_VERSION, sdk_version: SDK_VERSION, capabilities: CAPABILITIES };
    // An engine that outlived a gff-hab restart says which turn runs.
    if (this.turn) ready.turn = { turn: this.turn.id, origin: this.turn.origin };
    return ready;
  }

  // A gff-hab that restarted holds no open card: ask again for each.
  private resendApprovals(): void {
    for (const a of this.approvals.values()) this.send('approval.request', a.body);
  }

  private onMessage(m: Msg): void {
    switch (m.type) {
      case 'input':
        this.onInput(m.item as Item);
        break;
      case 'interrupt':
        void this.interrupt();
        break;
      case 'reseed':
        this.reseed(typeof m.reason === 'string' ? m.reason : '');
        break;
      case 'approval.answer':
        this.onApprovalAnswer(String(m.request ?? ''), m.allow === true, typeof m.message === 'string' ? m.message : '');
        break;
      case 'shutdown':
        this.shutdown();
        break;
      case 'context':
        this.onContext(m);
        break;
      case 'pace':
        // The governor's answers belong to a later stage.
        break;
      default:
        // Unknown types are acked and ignored.
        break;
    }
  }

  private onInput(item: Item): void {
    if (!item || typeof item.id !== 'string' || !item.id) return;
    // After shutdown nothing new goes in; gff-hab sends it again to the next
    // engine, since it was never taken.
    if (this.shuttingDown || this.exiting) return;
    if (this.handed.has(item.id) || this.pending.some((p) => p.id === item.id)) return;
    this.pending.push(item);
    void this.pump();
  }

  private pumping = false;
  private async pump(): Promise<void> {
    if (this.pumping) return;
    this.pumping = true;
    try {
      while (this.ready && !this.reseedPending && !this.shuttingDown && this.pending.length) {
        const item = this.pending.shift()!;
        if (this.handed.has(item.id)) continue;
        await this.hand(item);
      }
    } finally {
      this.pumping = false;
    }
  }

  // Hand one item to the SDK, in arrival order.
  private async hand(item: Item): Promise<void> {
    const msg = await this.toUserMessage(item);
    if (!this.ready || !this.input) {
      this.pending.unshift(item);
      return;
    }
    this.handed.add(item.id);
    this.uuidToItem.set(msg.uuid!, item.id);
    this.uuidItems.set(msg.uuid!, item);
    this.untaken.add(msg.uuid!);
    if (item.kind === 'message' || item.kind === undefined) this.userUrls.addText(item.text ?? '');
    if (!this.turn) this.beginTurn(originOf(item), [item.id]);
    this.input.push(msg);
  }

  private async toUserMessage(item: Item): Promise<SDKUserMessage> {
    const lines = itemLines(item);
    const blocks: any[] = [];
    const images: any[] = [];
    for (const p of item.images ?? []) {
      const type = IMAGE_TYPES[path.extname(p).toLowerCase()];
      if (!type) {
        lines.push(`[image ${path.basename(p)}: not a supported image type]`);
        continue;
      }
      try {
        const data = (await fs.readFile(p)).toString('base64');
        images.push({ type: 'image', source: { type: 'base64', media_type: type, data } });
      } catch {
        lines.push(`[image ${path.basename(p)}: could not be read]`);
      }
    }
    blocks.push({ type: 'text', text: lines.join('\n') }, ...images);
    return {
      type: 'user',
      message: { role: 'user', content: blocks },
      parent_tool_use_id: null,
      uuid: randomUUID() as any,
    };
  }

  private beginTurn(origin: 'user' | 'event', items: string[]): void {
    this.turn = {
      id: newTurnId(),
      origin,
      part: 0,
      interrupted: false,
      urls: new UrlSet(),
      approved: new UrlSet(),
      lastContext: 0,
      authFailed: false,
    };
    this.send('turn.started', { turn: this.turn.id, origin, items });
  }

  private take(uuid: string | undefined): void {
    if (!uuid || !this.untaken.has(uuid)) return;
    this.untaken.delete(uuid);
    const id = this.uuidToItem.get(uuid);
    if (id) this.send('input.taken', { item: id });
  }

  private async interrupt(): Promise<void> {
    if (!this.turn || !this.q) return;
    this.turn.interrupted = true;
    this.denyOpenApprovals('The user stopped this turn.');
    try {
      await this.q.interrupt();
    } catch (e) {
      this.log(`interrupt failed: ${(e as Error).message}`);
    }
  }

  private reseed(reason: string): void {
    // A "new" asked for during another re-seed still runs after it: the user asked for a clean
    // start.
    if (this.reseeding && reason !== 'new') return;
    this.reseedPending = reason || 'reseed';
    this.maybeReseed();
  }

  // A re-seed waits for the turn to end and for every approval to be answered (6.2).
  private maybeReseed(): void {
    if (this.reseedPending && !this.reseeding && !this.turn && this.approvals.size === 0) void this.doReseed();
  }

  // ---- the summary (6.1) ----

  private onContext(m: Msg): void {
    if (this.asks.answer(m)) return;
    const c = parseContext(m);
    // A quiet update the hab sent on its own: only while nothing else runs.
    if (m.for === 'quiet' && !this.turn && !this.reseeding && !this.summarizing && c.recent.length) {
      void this.updateSummary(c, this.settings.precompact_timeout_ms);
    }
  }

  // Ask gff-hab for the summary material (and the seed's parts for a re-seed); undefined when no
  // answer comes in time.
  private askContext(purpose: 'reseed' | 'compact', ms: number): Promise<ContextMsg | undefined> {
    return this.asks.ask((t, f) => this.send(t, f), purpose, ms);
  }

  // One summary call on the small model: the old summary and the new items in, the new summary
  // out (6.1). It sends `summary` and returns the text, or the old summary when the call fails
  // or runs past ms.
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

  private async summaryCall(c: ContextMsg, ms: number): Promise<string | undefined> {
    const input = new InputStream();
    let q: QueryHandle;
    try {
      q = this.d.query({ prompt: input, options: this.summaryOptions() });
    } catch (e) {
      this.log(`summary: the SDK did not start: ${(e as Error).message}`);
      return undefined;
    }
    input.push({
      type: 'user',
      message: { role: 'user', content: summaryPrompt(c.summary, c.recent, this.settings.summary_max_chars) },
      parent_tool_use_id: null,
      uuid: randomUUID() as any,
    });
    input.end();
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<undefined>((resolve) => {
      timer = setTimeout(() => resolve(undefined), Math.max(1, ms));
    });
    const read = (async () => {
      let out = '';
      for await (const m of q) {
        const a = m as any;
        if (a.type === 'assistant' && !a.error && !a.parent_tool_use_id) {
          for (const b of a.message?.content ?? []) if (b?.type === 'text' && typeof b.text === 'string') out += b.text;
        }
        if (a.type === 'result') return a.subtype === 'success' && !a.is_error ? out : undefined;
      }
      return undefined;
    })().catch((e) => {
      this.log(`summary: ${(e as Error).message}`);
      return undefined;
    });
    const got = await Promise.race([read, timeout]);
    clearTimeout(timer);
    try {
      q.close();
    } catch {
      // already closed
    }
    if (got === undefined) this.log('summary: no new summary; the old one stays');
    return got;
  }

  // The summary call loads nothing: no project files, no tools, no MCP server, no transcript.
  summaryOptions(): Options {
    const opts = this.buildOptions('');
    const out: Options = {
      cwd: opts.cwd,
      env: opts.env,
      settingSources: [],
      tools: [],
      allowedTools: [],
      disallowedTools: [...DENY_TOOLS, ...BASE_TOOLS],
      mcpServers: {},
      strictMcpConfig: true,
      persistSession: false,
      maxTurns: 1,
      systemPrompt: SUMMARY_SYSTEM,
      permissionMode: 'default',
      canUseTool: async (name) => ({ behavior: 'deny', message: `${name} is not available to the summary.` }),
      stderr: opts.stderr,
    };
    const model = this.settings.small_model ?? this.settings.model;
    if (model) out.model = model;
    return out;
  }

  // The PreCompact hook: update the summary first, within precompact_timeout; on a timeout the
  // compaction goes ahead with the old summary (6.3).
  private preCompact: HookCallback = async (input) => {
    if ((input as any).hook_event_name !== 'PreCompact') return {};
    const end = Date.now() + this.settings.precompact_timeout_ms;
    const c = await this.askContext('compact', this.settings.precompact_timeout_ms);
    if (c) await this.updateSummary(c, end - Date.now());
    return {};
  };

  private shutdown(): void {
    this.shuttingDown = true;
    if (!this.turn) void this.exit(0);
  }

  private onApprovalAnswer(request: string, allow: boolean, message: string): void {
    const a = this.approvals.get(request);
    if (!a) return;
    this.approvals.delete(request);
    if (allow) {
      if (a.url) this.turn?.approved.add(a.url);
      a.resolve({ behavior: 'allow' });
    } else {
      a.resolve({ behavior: 'deny', message: message || 'The user did not allow this.' });
    }
  }

  private denyOpenApprovals(message: string): void {
    for (const [id, a] of this.approvals) {
      this.approvals.delete(id);
      a.resolve({ behavior: 'deny', message });
    }
  }

  // ---- the SDK session ----

  buildOptions(resume: string): Options {
    const env: Record<string, string | undefined> = { ...this.d.env };
    // Only the credential from hello reaches the binary.
    delete env.CLAUDE_CODE_OAUTH_TOKEN;
    delete env.ANTHROPIC_API_KEY;
    delete env.ANTHROPIC_AUTH_TOKEN;
    delete env[ENGINE_TOKEN_VAR]; // the engine route's token stays in the engine (3.3)
    if (this.credential?.kind === 'api_key') env.ANTHROPIC_API_KEY = this.credential.value;
    else if (this.credential) env.CLAUDE_CODE_OAUTH_TOKEN = this.credential.value;
    env.CLAUDE_AGENT_SDK_CLIENT_APP = `gff-hab-capcom/${ENGINE_VERSION}`;

    const opts: Options = {
      cwd: this.d.cwd,
      env,
      settingSources: ['project'],
      permissionMode: 'default',
      tools: [...BASE_TOOLS],
      disallowedTools: [...DENY_TOOLS],
      allowedTools: [...ALLOW_TOOLS],
      canUseTool: this.canUseTool,
      hooks: {
        PreToolUse: [{ matcher: 'WebFetch', hooks: [this.preWebFetch] }],
        PostToolUse: [{ hooks: [this.postToolUse] }],
        // The SDK's own bound, a little past the engine's, so the engine's timeout decides.
        PreCompact: [{ hooks: [this.preCompact], timeout: Math.ceil(this.settings.precompact_timeout_ms / 1000) + 5 }],
      },
      // Echo each streamed user message back: the read mark (7.3).
      extraArgs: { 'replay-user-messages': null },
      stderr: (data: string) => {
        for (const l of data.split('\n')) if (l.trim()) this.log(`sdk: ${l}`);
      },
    };
    if (resume) opts.resume = resume;
    if (this.settings.model) opts.model = this.settings.model;
    if (this.settings.max_turns) opts.maxTurns = this.settings.max_turns;
    if (this.settings.max_turn_usd) opts.maxBudgetUsd = this.settings.max_turn_usd;
    return opts;
  }

  private async startSession(why: 'first' | 'restart' | 'reseed' | 'new', resume = '', seed = ''): Promise<void> {
    const gen = ++this.gen;
    this.ready = false;
    this.handed = new Set();
    this.uuidToItem.clear();
    this.uuidItems.clear();
    this.untaken.clear();
    this.userUrls = new UrlSet();
    this.totalCost = 0;
    if (resume) {
      // Rebuild the handed set from the resumed session's markers.
      const file = await findTranscript(configDir(this.d.env), this.d.cwd, resume);
      this.handed = await handedIds(file);
      if (file) await this.loadUserUrls(file);
    }
    if (gen !== this.gen) return;
    const input = new InputStream();
    this.input = input;
    let q: QueryHandle;
    try {
      q = this.d.query({ prompt: input, options: this.buildOptions(resume) });
    } catch (e) {
      this.sendError(`the SDK did not start: ${(e as Error).message}`, '', true);
      await this.exit(1);
      return;
    }
    this.q = q;
    // The probe: a message that runs no model call but makes the SDK send
    // its init with the tool list.
    input.push({
      type: 'user',
      message: { role: 'user', content: START_CHECK_TEXT },
      parent_tool_use_id: null,
      shouldQuery: false,
      uuid: randomUUID() as any,
    });
    // The seed goes in the same way: no model call of its own; its text joins the first real
    // message (6.2).
    if (seed) {
      input.push({
        type: 'user',
        message: { role: 'user', content: seed },
        parent_tool_use_id: null,
        shouldQuery: false,
        uuid: randomUUID() as any,
      });
    }
    void this.consume(gen, q, why, resume, seed ? 2 : 1);
  }

  private async loadUserUrls(file: string): Promise<void> {
    try {
      const text = await fs.readFile(file, 'utf8');
      for (const line of text.split('\n')) {
        if (!line.includes('http')) continue;
        let rec: any;
        try {
          rec = JSON.parse(line);
        } catch {
          continue;
        }
        if (rec?.type !== 'user') continue;
        const c = rec?.message?.content;
        const t = typeof c === 'string' ? c : Array.isArray(c) ? c.filter((b: any) => b?.type === 'text').map((b: any) => b.text).join('\n') : '';
        // Only inputs the engine handed (marked) are user messages of the thread.
        if (t.match(MARKER_RE)) this.userUrls.addText(t);
      }
    } catch {
      // no file, no URLs
    }
  }

  private async consume(gen: number, q: QueryHandle, why: string, resume: string, noQuery = 1): Promise<void> {
    let sawInit = false;
    let probeDone = false;
    let noQueryLeft = noQuery; // the probe, and the seed: each answers one result
    try {
      for await (const m of q) {
        if (gen !== this.gen || this.exiting) break;
        if (m.type === 'system' && (m as any).subtype === 'init') {
          if (sawInit) continue;
          sawInit = true;
          const servers = (((m as any).mcp_servers as { name?: string }[]) ?? []).map((x) => x?.name ?? '');
          const bad = ((m as any).tools as string[] ?? []).filter((t) => !onSurface(t, servers));
          if (bad.length) {
            this.sendError(`start check: tool outside the Capcom surface: ${bad.join(', ')}`, 'start', true);
            await this.exit(1);
            return;
          }
          this.sessionId = (m as any).session_id ?? '';
          this.send('session.started', { session: this.sessionId, why });
          continue;
        }
        if (!probeDone) {
          if (m.type === 'result' && sawInit && --noQueryLeft <= 0) {
            probeDone = true;
            this.ready = true;
            void this.pump();
          }
          continue;
        }
        this.onSdkMessage(m);
        if (m.type === 'result') await this.onResult(m as any);
      }
    } catch (e) {
      if (gen !== this.gen || this.exiting) return;
      if (!sawInit && resume) {
        // The session would not resume: a re-seed replaces it (3.4).
        this.sendError(`resume of ${resume} failed: ${(e as Error).message}`, '', false);
        this.reseedPending = 'restart_failed';
        this.maybeReseed();
        return;
      }
      this.endTurn('error');
      this.sendError(`the SDK stopped: ${(e as Error).message}`, '', true);
      await this.exit(1);
      return;
    }
    if (gen !== this.gen || this.exiting) return;
    if (!sawInit && resume) {
      this.sendError(`resume of ${resume} failed`, '', false);
      this.reseedPending = 'restart_failed';
      this.maybeReseed();
      return;
    }
    // The SDK ended on its own.
    this.endTurn('error');
    this.sendError('the SDK session ended', '', true);
    await this.exit(1);
  }

  private onSdkMessage(m: SDKMessage): void {
    const t = this.turn;
    switch (m.type) {
      case 'user': {
        const u = m as any;
        if (u.isReplay) this.take(u.uuid);
        // Tool results: URLs the turn has now seen.
        if (t && Array.isArray(u.message?.content)) {
          for (const b of u.message.content) if (b?.type === 'tool_result') t.urls.addText(textOf(b.content));
        }
        break;
      }
      case 'assistant': {
        const a = m as any;
        for (const id of a.user_message_uuids ?? []) this.take(id);
        if (a.error === 'authentication_failed' && t) t.authFailed = true;
        if (a.parent_tool_use_id) break;
        const usage = a.message?.usage;
        if (t && usage) {
          t.lastContext =
            (usage.input_tokens ?? 0) +
            (usage.cache_read_input_tokens ?? 0) +
            (usage.cache_creation_input_tokens ?? 0) +
            (usage.output_tokens ?? 0);
        }
        if (!t || a.error) break;
        for (const b of a.message?.content ?? []) {
          if (b?.type === 'text' && typeof b.text === 'string' && b.text.trim()) {
            this.send('reply', { turn: t.id, part: t.part++, text: b.text });
          } else if (b?.type === 'tool_use') {
            this.send('tool', { turn: t.id, name: b.name, summary: this.redact(toolSummary(b.name, b.input)) });
          } else if (b?.type === 'web_search_tool_result' || b?.type === 'web_fetch_tool_result') {
            t.urls.addText(textOf(b.content));
          }
        }
        break;
      }
      case 'system': {
        const s = m as any;
        if (s.subtype === 'compact_boundary') this.send('compacted', {});
        break;
      }
      case 'rate_limit_event':
        this.send('usage', { reading: (m as any).rate_limit_info ?? {} });
        break;
      default:
        break;
    }
  }

  private async onResult(r: any): Promise<void> {
    for (const id of r.user_message_uuids ?? []) this.take(id);
    const t = this.turn;
    if (!t) return;
    if (r.api_error_status === 401) t.authFailed = true;
    let kind: string;
    if (t.interrupted) kind = 'interrupted';
    else if (r.subtype === 'error_max_turns' || r.subtype === 'error_max_budget_usd') kind = 'limit';
    else if (r.subtype === 'success' && !r.is_error) kind = 'success';
    else kind = 'error';
    const usage = r.usage ?? {};
    let window = 0;
    for (const mu of Object.values(r.modelUsage ?? {}) as any[]) window = Math.max(window, mu?.contextWindow ?? 0);
    const total = typeof r.total_cost_usd === 'number' ? r.total_cost_usd : this.totalCost;
    const cost = Math.max(0, total - this.totalCost);
    this.totalCost = total;
    this.send('turn.ended', {
      turn: t.id,
      result: kind,
      input_tokens: (usage.input_tokens ?? 0) + (usage.cache_read_input_tokens ?? 0) + (usage.cache_creation_input_tokens ?? 0),
      output_tokens: usage.output_tokens ?? 0,
      cost_usd: cost,
      context_used: window > 0 ? Math.min(1, t.lastContext / window) : 0,
    });
    this.turn = undefined;
    this.denyOpenApprovals('The turn ended.');
    if (t.authFailed) {
      this.sendError('the model provider refused the credential', 'auth', true);
      await this.exit(1);
      return;
    }
    if (this.shuttingDown) {
      await this.exit(0);
      return;
    }
    if (this.reseedPending) {
      this.maybeReseed();
      return;
    }
    // Items handed but not taken run as the SDK's next turn.
    const left = [...this.untaken].map((u) => this.uuidToItem.get(u)!).filter(Boolean);
    if (left.length) this.beginTurn('user', left);
  }

  private endTurn(result: string): void {
    const t = this.turn;
    if (!t) return;
    this.send('turn.ended', { turn: t.id, result, input_tokens: 0, output_tokens: 0, cost_usd: 0, context_used: 0 });
    this.turn = undefined;
  }

  // A re-seed (6.2): ask gff-hab for the material, update the summary (not for a new
  // conversation, whose summary is empty), end the SDK session and start a fresh one that opens
  // with the seed. The pump holds new input until the fresh session is ready.
  private async doReseed(): Promise<void> {
    if (this.reseeding) return;
    this.reseeding = true;
    try {
      for (;;) {
        const reason = this.reseedPending ?? 'reseed';
        const ms = this.settings.precompact_timeout_ms;
        const end = Date.now() + ms;
        const c = await this.askContext('reseed', ms);
        if (this.exiting) return;
        let summary = '';
        if (reason !== 'new' && c) summary = await this.updateSummary(c, end - Date.now());
        if (this.exiting) return;
        if (this.reseedPending === 'new' && reason !== 'new') continue; // a new conversation came meanwhile
        // A quiet update still running goes out before the fresh session starts, so it can never
        // land on the new conversation's empty summary (7.5).
        if (this.summarizing) await this.summarizing;
        if (this.exiting) return;
        this.reseedPending = undefined;
        this.closeQuery();
        // Items handed but not taken go in again in the fresh session.
        const left = [...this.untaken].map((u) => this.uuidItems.get(u)).filter((x): x is Item => !!x);
        this.pending.unshift(...left);
        await this.startSession(reason === 'new' ? 'new' : 'reseed', '', seedText(reason, summary, c?.seed));
        return;
      }
    } finally {
      this.reseeding = false;
    }
    if (!this.exiting) this.maybeReseed(); // a new conversation asked for while the fresh session started
  }

  private closeQuery(): void {
    this.gen++;
    this.ready = false;
    this.input?.end();
    try {
      this.q?.close();
    } catch {
      // already closed
    }
    this.q = undefined;
    this.input = undefined;
  }

  private async exit(code: number): Promise<void> {
    if (this.exiting) return;
    this.exiting = true;
    this.denyOpenApprovals('Capcom is stopping.');
    this.asks.drain();
    this.closeQuery();
    await this.route.flush(this.d.flushTimeoutMs ?? 5000);
    this.route.close();
    this.d.exit(code);
  }

  // ---- tools ----

  private urlSeen(url: string): boolean {
    const t = this.turn;
    return this.userUrls.has(url) || (!!t && (t.urls.has(url) || t.approved.has(url)));
  }

  // 5.3's URL rule for every WebFetch, so a call the SDK would approve on
  // its own still meets it. An unseen URL goes to canUseTool as a card.
  private preWebFetch: HookCallback = async (input) => {
    const i = input as any;
    if (i.hook_event_name !== 'PreToolUse' || i.tool_name !== 'WebFetch') return {};
    const url = String(i.tool_input?.url ?? '');
    if (url && this.urlSeen(url)) {
      return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'allow' } };
    }
    return {
      hookSpecificOutput: {
        hookEventName: 'PreToolUse',
        permissionDecision: 'ask',
        permissionDecisionReason: 'This URL did not appear in the conversation: ask the user.',
      },
    };
  };

  private postToolUse: HookCallback = async (input) => {
    const i = input as any;
    if (i.hook_event_name === 'PostToolUse' && this.turn) this.turn.urls.addText(textOf(i.tool_response));
    return {};
  };

  private canUseTool: CanUseTool = async (toolName, input, opts) => {
    if (toolName !== 'WebFetch') {
      return { behavior: 'deny', message: `${toolName} is not available to Capcom.` };
    }
    const url = String((input as any)?.url ?? '');
    if (url && this.urlSeen(url)) return { behavior: 'allow', updatedInput: input };
    if (!this.turn) return { behavior: 'deny', message: 'No turn is running.' };
    let shown = url;
    try {
      const u = new URL(url);
      shown = u.host + u.pathname;
    } catch {
      // keep the raw text
    }
    const request = randomUUID();
    const body = { request, tool: 'WebFetch', input: { ...input }, reason: `Capcom wants to open ${shown}` };
    const answer = new Promise<PermissionResult>((resolve) => {
      this.approvals.set(request, { resolve, url, body });
    });
    opts?.signal?.addEventListener('abort', () => this.onApprovalAnswer(request, false, 'The turn was stopped.'));
    this.send('approval.request', body);
    const r = await answer;
    if (r.behavior === 'allow') return { behavior: 'allow', updatedInput: input };
    this.maybeReseed();
    return r;
  };
}
