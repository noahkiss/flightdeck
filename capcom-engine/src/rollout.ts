// Reading a Codex thread's rollout file for item markers and context use (capcom-service.md 4.3,
// 4.4; the Codex engine). Codex writes each thread to
// <CODEX_HOME>/sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl, one JSON record a line:
// {type: session_meta | response_item | event_msg | turn_context, payload}. A user message is a
// response_item {type: message, role: user, content: [{type: input_text, text}]}; an event_msg
// token_count carries info.last_token_usage, info.model_context_window and rate_limits: the plan
// windows the CLI read from the x-codex-* response headers, or null when a response carries none,
// as the API under a key (probed with CLI 0.167 against a loopback endpoint).
import { promises as fs } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { MARKER_RE } from './transcript.js';

// The Codex home: $CODEX_HOME, else ~/.codex, as an absolute path (the engine home's links must
// not resolve against the engine home itself).
export function codexHome(env: NodeJS.ProcessEnv = process.env): string {
  return path.resolve(env.CODEX_HOME || path.join(os.homedir(), '.codex'));
}

// findRollout finds the thread's rollout under <home>/sessions, newest day first. A thread id is
// a UUID, so the name's end is enough.
export async function findRollout(home: string, thread: string): Promise<string | undefined> {
  if (!/^[0-9a-fA-F-]{8,}$/.test(thread)) return undefined;
  const suffix = `-${thread}.jsonl`;
  const root = path.join(home, 'sessions');
  const sorted = async (d: string): Promise<string[]> => {
    try {
      return (await fs.readdir(d)).sort().reverse();
    } catch {
      return [];
    }
  };
  for (const y of await sorted(root)) {
    for (const m of await sorted(path.join(root, y))) {
      for (const d of await sorted(path.join(root, y, m))) {
        for (const f of await sorted(path.join(root, y, m, d))) {
          if (f.startsWith('rollout-') && f.endsWith(suffix)) return path.join(root, y, m, d, f);
        }
      }
    }
  }
  return undefined;
}

async function records(file: string | undefined): Promise<any[]> {
  if (!file) return [];
  let text: string;
  try {
    text = await fs.readFile(file, 'utf8');
  } catch {
    return [];
  }
  const out: any[] = [];
  for (const line of text.split('\n')) {
    if (!line.trim()) continue;
    try {
      out.push(JSON.parse(line));
    } catch {
      // a half-written last line
    }
  }
  return out;
}

function userTexts(rec: any): string[] {
  const p = rec?.payload;
  if (rec?.type !== 'response_item' || p?.type !== 'message' || p?.role !== 'user' || !Array.isArray(p.content)) return [];
  return p.content.filter((b: any) => b?.type === 'input_text' && typeof b.text === 'string').map((b: any) => b.text as string);
}

// rolloutHandedIds collects the item ids of every marker at a line start in the user messages
// of a rollout.
export async function rolloutHandedIds(file: string | undefined): Promise<Set<string>> {
  const ids = new Set<string>();
  for (const rec of await records(file)) {
    for (const t of userTexts(rec)) for (const m of t.matchAll(MARKER_RE)) ids.add(m[1]!);
  }
  return ids;
}

// rolloutContext is the context use after the last model call: its input and output tokens over
// the model's window, from the last token_count; 0 when the rollout has none.
export async function rolloutContext(file: string | undefined): Promise<number> {
  return (await rolloutTurnEnd(file)).used;
}

// The rollout as read at a turn's end: the context use, and the newest token_count record with
// rate limits of each limit id (a login's plan windows; an API key's rollout has none), oldest
// first. Each record goes to gff-hab as it is, timestamp included (capcom-service.md 10.1).
export interface RolloutTurnEnd {
  used: number;
  rateLimits: any[];
}

export async function rolloutTurnEnd(file: string | undefined): Promise<RolloutTurnEnd> {
  let used = 0;
  const limits = new Map<string, any>();
  for (const rec of await records(file)) {
    const p = rec?.payload;
    if (rec?.type !== 'event_msg' || p?.type !== 'token_count') continue;
    const rl = p.rate_limits;
    if (rl && typeof rl === 'object' && !Array.isArray(rl)) {
      const id = typeof rl.limit_id === 'string' && rl.limit_id ? rl.limit_id : 'codex';
      limits.delete(id); // a newer record goes last
      limits.set(id, rec);
    }
    const last = p.info?.last_token_usage;
    const window = p.info?.model_context_window;
    if (!last || !(window > 0)) continue;
    used = Math.min(1, ((last.input_tokens ?? 0) + (last.output_tokens ?? 0)) / window);
  }
  return { used, rateLimits: [...limits.values()] };
}
