// Reading the SDK session's transcript for item markers (4.3, 4.4).
import { promises as fs } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// A marker counts only at the start of a line.
export const MARKER_RE = /^\[flightdeck item ([^\]\s]+)\]/gm;

export function markerLine(id: string): string {
  return `[flightdeck item ${id}]`;
}

// The Claude config directory: $CLAUDE_CONFIG_DIR, else ~/.claude.
export function configDir(env: NodeJS.ProcessEnv = process.env): string {
  return env.CLAUDE_CONFIG_DIR || path.join(os.homedir(), '.claude');
}

// The CLI keys a project folder by its path with every character that is
// not a letter or digit turned into '-'.
export function encodeCwd(cwd: string): string {
  return cwd.replace(/[^a-zA-Z0-9]/g, '-');
}

export function transcriptPath(cfg: string, cwd: string, session: string): string {
  return path.join(cfg, 'projects', encodeCwd(cwd), `${session}.jsonl`);
}

// Find the transcript: the computed path first (the CLI keys the folder by
// its real path, with links resolved), else any project folder that holds
// <session>.jsonl (a resume by id may land outside this folder).
export async function findTranscript(cfg: string, cwd: string, session: string): Promise<string | undefined> {
  let real = cwd;
  try {
    real = await fs.realpath(cwd);
  } catch {
    // keep cwd
  }
  for (const c of new Set([real, cwd])) {
    const p = transcriptPath(cfg, c, session);
    try {
      await fs.access(p);
      return p;
    } catch {
      // next
    }
  }
  let dirs: string[] = [];
  try {
    dirs = await fs.readdir(path.join(cfg, 'projects'));
  } catch {
    return undefined;
  }
  for (const d of dirs) {
    const q = path.join(cfg, 'projects', d, `${session}.jsonl`);
    try {
      await fs.access(q);
      return q;
    } catch {
      // next
    }
  }
  return undefined;
}

// Collect the item ids of every marker in user records of a transcript.
export async function handedIds(file: string | undefined): Promise<Set<string>> {
  const ids = new Set<string>();
  if (!file) return ids;
  let text: string;
  try {
    text = await fs.readFile(file, 'utf8');
  } catch {
    return ids;
  }
  for (const line of text.split('\n')) {
    if (!line.includes('[flightdeck item ')) continue;
    let rec: any;
    try {
      rec = JSON.parse(line);
    } catch {
      continue; // a half-written last line
    }
    if (rec?.type !== 'user' && rec?.message?.role !== 'user') continue;
    for (const t of textsOf(rec?.message?.content)) {
      for (const m of t.matchAll(MARKER_RE)) ids.add(m[1]!);
    }
  }
  return ids;
}

function textsOf(content: unknown): string[] {
  if (typeof content === 'string') return [content];
  if (!Array.isArray(content)) return [];
  const out: string[] = [];
  for (const b of content) {
    if (b && typeof b === 'object' && (b as any).type === 'text' && typeof (b as any).text === 'string') {
      out.push((b as any).text);
    }
  }
  return out;
}
