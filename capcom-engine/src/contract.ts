// The engine contract both engines share (docs/specs/capcom-service.md 4): hello's fields, an
// input item and its text for the model, the context message, and the wait for a
// context.request's answer. Each engine maps these onto its own SDK.
import { randomUUID } from 'node:crypto';
import type { Recent, SeedParts } from './context.js';
import type { Msg } from './route.js';
import { markerLine, neutralise } from './transcript.js';

// The model credential hello hands the engine (10.2). `login` carries no value: the engine's
// CLI uses the login the user made with its own command (the Codex engine's `codex login`).
export interface Credential {
  kind: 'oauth_token' | 'api_key' | 'login';
  value: string;
}

// One thread item for the model (4.4 input).
export interface Item {
  id: string;
  kind?: string;
  author?: string;
  time?: string;
  tz?: string;
  text?: string;
  images?: string[];
  context?: { kind?: string; title?: string; text?: string }[];
  send_now?: boolean; // sent with "Send now": runs while user turns wait (10.1)
}

// hello's settings, with 19.2's defaults where hello carries none.
export interface Settings {
  model?: string;
  small_model?: string;
  max_turns?: number;
  max_turn_usd?: number;
  summary_max_chars: number;
  precompact_timeout_ms: number;
}

export const SUMMARY_MAX_CHARS = 6000;
export const PRECOMPACT_TIMEOUT_MS = 30_000;

export function defaultSettings(): Settings {
  return { summary_max_chars: SUMMARY_MAX_CHARS, precompact_timeout_ms: PRECOMPACT_TIMEOUT_MS };
}

export function parseSettings(raw: unknown): Settings {
  const s = (raw ?? {}) as Record<string, unknown>;
  const pos = (v: unknown): number | undefined => (typeof v === 'number' && v > 0 ? v : undefined);
  return {
    model: typeof s.model === 'string' && s.model ? s.model : undefined,
    small_model: typeof s.small_model === 'string' && s.small_model ? s.small_model : undefined,
    max_turns: pos(s.max_turns),
    max_turn_usd: pos(s.max_turn_usd),
    summary_max_chars: pos(s.summary_max_chars) ?? SUMMARY_MAX_CHARS,
    precompact_timeout_ms: pos(s.precompact_timeout_ms) ?? PRECOMPACT_TIMEOUT_MS,
  };
}

// The credential of a hello, when its kind is one the engine takes.
export function parseCredential(raw: unknown, kinds: readonly Credential['kind'][]): Credential | undefined {
  const c = raw as Partial<Credential> | undefined;
  if (!c || !kinds.includes(c.kind as Credential['kind'])) return undefined;
  const value = typeof c.value === 'string' ? c.value : '';
  if (c.kind !== 'login' && !value) return undefined;
  return { kind: c.kind as Credential['kind'], value: c.kind === 'login' ? '' : value };
}

// The re-seed a hello asks the engine to open with instead of an empty session: `switch` when
// the thread's last session was another engine's, or the reason of a re-seed whose fresh session
// never started (4.4 hello `reseed`).
export function helloReseed(m: Msg): string {
  return typeof m.reseed === 'string' && !m.sdk_session ? m.reseed : '';
}

// itemLines is an input's text for the model: its marker line, who sent it and when, the
// attached context blocks, then the text. Every outside line is neutralised (4.4 Markers).
export function itemLines(item: Item): string[] {
  const lines = [markerLine(item.id, item.kind)];
  const when = [item.time, item.tz ? `(${item.tz})` : ''].filter(Boolean).join(' ');
  if (item.kind === 'agent') lines.push(`Channel message from ${item.author ?? 'an agent'}${when ? ` at ${when}` : ''}.`);
  else if (item.kind === 'note') lines.push(`Flightdeck note${when ? ` at ${when}` : ''}.`);
  else if (item.kind === 'event') lines.push(`Flightdeck events${when ? ` at ${when}` : ''}: an event turn. Nobody asked you anything.`);
  else if (when || item.author) lines.push(`Sent${item.author ? ` by ${item.author}` : ''}${when ? ` at ${when}` : ''}.`);
  for (const c of item.context ?? []) {
    lines.push('', `[context: ${c.kind ?? 'item'}${c.title ? `: ${neutralise(c.title).replace(/[\]\n]/g, ' ')}` : ''}]`, neutralise(c.text ?? ''));
  }
  lines.push('', neutralise(item.text ?? ''));
  return lines;
}

// isUserKind: an item of the user's own text, whose URLs count as seen (5.3).
export function isUserKind(kind: string | undefined): boolean {
  return kind === 'message' || kind === undefined || kind === '';
}

// opensUserTurn: an item that makes its turn a user turn for what it may do (9.4): the user's
// message, or a Flightdeck note the model gets. An event batch or an agent report does not.
export function opensUserTurn(kind: string | undefined): boolean {
  return isUserKind(kind) || kind === 'note';
}

// The refusal of a WebFetch of an unseen URL in an event turn: no card (9.4).
export const EVENT_FETCH_REFUSAL = 'This URL did not appear in the conversation, and nobody asked for this turn: ask the user first.';

// A turn's origin from the item that opened it.
export function originOf(item: Item): 'user' | 'event' {
  return item.kind === 'event' ? 'event' : 'user';
}

let turnSeq = 0;
export function newTurnId(): string {
  return `t${Date.now().toString(36)}-${++turnSeq}`;
}

// A context message from gff-hab (6): the summary at hand, the items since it, and for a
// re-seed the seed's parts.
export interface ContextMsg {
  summary: string;
  through: string;
  recent: Recent[];
  seed?: SeedParts;
}

export function parseContext(m: Msg): ContextMsg {
  return {
    summary: typeof m.summary === 'string' ? m.summary : '',
    through: typeof m.through === 'string' ? m.through : '',
    recent: Array.isArray(m.recent) ? (m.recent as Recent[]) : [],
    seed: m.seed && typeof m.seed === 'object' ? (m.seed as SeedParts) : undefined,
  };
}

// ContextAsks pairs each context.request with its answer, or undefined after a bound.
export class ContextAsks {
  private wait = new Map<string, (c: ContextMsg | undefined) => void>();

  ask(send: (type: string, fields: Record<string, unknown>) => void, purpose: 'reseed' | 'compact', ms: number): Promise<ContextMsg | undefined> {
    const request = randomUUID();
    return new Promise((resolve) => {
      const t = setTimeout(() => {
        this.wait.delete(request);
        resolve(undefined);
      }, Math.max(1, ms));
      this.wait.set(request, (c) => {
        clearTimeout(t);
        resolve(c);
      });
      send('context.request', { request, for: purpose });
    });
  }

  // answer hands a context message to the request it answers; false when it answers none.
  answer(m: Msg): boolean {
    const req = typeof m.request === 'string' ? m.request : '';
    const w = req ? this.wait.get(req) : undefined;
    if (!w) return false;
    this.wait.delete(req);
    w(parseContext(m));
    return true;
  }

  // drain answers every open request with nothing (the engine is stopping).
  drain(): void {
    for (const w of this.wait.values()) w(undefined);
    this.wait.clear();
  }
}

// A redactor of one secret value in log and error text.
export function redactor(value: () => string | undefined): (s: string) => string {
  return (s) => {
    const v = value();
    return v && v.length >= 4 ? s.split(v).join('[redacted]') : s;
  };
}

// ---- the governor's pace (10.1) ----

// The governor's answer: the pace of the next user turn and of the next event turn. Until the
// first pace message, every turn runs.
export interface Pace {
  user: 'ok' | 'small' | 'wait';
  event: 'ok' | 'wait';
}

export const PACE_OK: Pace = { user: 'ok', event: 'ok' };

// parsePace reads a pace message; an unknown word is ok.
export function parsePace(m: Msg): Pace {
  const user = m.pace === 'small' || m.pace === 'wait' ? m.pace : 'ok';
  const event = m.event_pace === 'wait' ? 'wait' : 'ok';
  return { user, event };
}

// isPaced: an item paced at the event step: event batches, and agent reports and round
// messages (sent as agent), which no user typed.
export function isPaced(i: Item): boolean {
  return i.kind === 'event' || i.kind === 'agent';
}

// admit decides whether the pending items may begin a turn now, and on which model. The head
// item's kind gives the turn's origin. An event turn that waits lets a later user message begin
// a user turn that takes the event with it, so a user message never waits behind an event; a
// send-now message runs, with every item before it, on the small model while user turns wait.
// null: the items wait for the next pace.
export function admit(pending: readonly Item[], p: Pace): { origin: 'user' | 'event'; small: boolean } | null {
  const head = pending[0];
  if (!head) return null;
  const sendNow = pending.some((i) => i.send_now);
  const userTurn = (): { origin: 'user'; small: boolean } | null => {
    if (p.user === 'ok') return { origin: 'user', small: false };
    if (p.user === 'small') return { origin: 'user', small: true };
    return sendNow ? { origin: 'user', small: true } : null;
  };
  if (!isPaced(head)) return userTurn();
  if (p.event === 'ok') return { origin: originOf(head), small: p.user !== 'ok' };
  if (pending.some((i) => !isPaced(i))) return userTurn();
  return null;
}

// mayHand says whether the head of pending may go to the model now, into the turn admit began
// or one already running. While user turns wait, a user item goes only with a send-now item at
// or after it, and a paced item only while event turns may run: an item queued after the last
// send-now one waits for the next pace, not the send-now turn.
export function mayHand(pending: readonly Item[], p: Pace): boolean {
  if (p.user !== 'wait') return true;
  const head = pending[0];
  if (!head) return false;
  if (pending.some((i) => i.send_now)) return true;
  return isPaced(head) && p.event === 'ok';
}

// handCount is how many items from the head of pending one admitted turn takes, for an engine
// that hands a turn's items at once (mayHand item by item).
export function handCount(pending: readonly Item[], p: Pace): number {
  let n = 0;
  while (n < pending.length && mayHand(pending.slice(n), p)) n++;
  return n;
}
