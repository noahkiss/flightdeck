// The engine's source check (docs/specs/diagnostics.md 10.2): the TypeScript side of
// internal/diaglog/check, so the engine cannot become the hole the Go check closes. The types in
// src/log.ts refuse a message, component or key that is not a literal at compile time
// (test/log.test.ts); this check refuses what the types cannot see. It reads each file's
// tokens, so a ban in a comment or a string does not count.
//
// Bans, by id:
// - console: any use of console.
// - stderr: process.stderr or process.stdout written outside src/log.ts.
// - relay: relayStderr outside the files that relay raw text to engine.log.
// - message: a logger call whose message is not a plain quoted string (a template, a variable).
// - value: a logger call value that is not one call of a log.ts constructor.
// - key: a word, count or dur key that is not a plain quoted string.
// - built: a template or a + inside a safe constructor's arguments.
// - as-value: a logger method or Log.line used as a value, not called.
// - direct: Log.line called outside src/log.ts, or new Attr anywhere else.
// - component: a Log.for argument that is not a plain quoted string.
// - text: a call of text outside src/log.ts. The engine writes no free text (4.9).
// - computed: a logger method or Log.line named by a string in brackets (log['warn']).
// A call of debug, info, warn or error is a logger call whatever its receiver.
//
// freeSites lists the other kind of risk: each word, session or kind call whose value is not a
// quoted string. Its value is checked for its form when the line is written, so the site list
// in test/logcheck.test.ts pins each one, and a new site fails until someone adds it there.

export interface Finding {
  line: number;
  ban: string;
}

// The files that may relay raw text to engine.log: the SDK's stderr, and a start failure's
// own words.
export const RELAY_FILES = ['engine.ts', 'main.ts'];

const METHODS = new Set(['debug', 'info', 'warn', 'error']);
const CONSTRUCTORS = new Set(['session', 'kind', 'word', 'count', 'dur', 'errClass', 'errType', 'frames', 'text']);
const KEYED = new Set(['word', 'count', 'dur']);
// The constructors whose value is a string that only its form check guards.
const FORMED = new Set(['word', 'session', 'kind']);

type Tok = { k: 'id' | 'str' | 'tpl' | 'num' | 're' | 'p'; v: string; line: number };

// lex splits TypeScript source into tokens: identifiers, strings (with their quote), templates
// (whole, with their substitutions), regular expressions, numbers and punctuation. Comments go.
export function lex(src: string): Tok[] {
  const out: Tok[] = [];
  let i = 0;
  let line = 1;
  const n = src.length;
  const regexAfter = (t: Tok | undefined) =>
    !t || (t.k === 'p' && t.v !== ')' && t.v !== ']' && t.v !== '}') || (t.k === 'id' && ['return', 'typeof', 'case', 'in', 'of', 'new', 'delete', 'void', 'throw', 'yield', 'await'].includes(t.v));
  const skipString = (q: string): void => {
    i++;
    while (i < n && src[i] !== q) {
      if (src[i] === '\\') i++;
      else if (src[i] === '\n') line++;
      i++;
    }
    i++;
  };
  const skipTemplate = (): void => {
    i++;
    while (i < n && src[i] !== '`') {
      if (src[i] === '\\') {
        i += 2;
        continue;
      }
      if (src[i] === '\n') line++;
      if (src[i] === '$' && src[i + 1] === '{') {
        i += 2;
        let depth = 1;
        while (i < n && depth > 0) {
          const c = src[i]!;
          if (c === '{') depth++;
          else if (c === '}') depth--;
          else if (c === "'" || c === '"') {
            skipString(c);
            continue;
          } else if (c === '`') {
            skipTemplate();
            continue;
          } else if (c === '\n') line++;
          i++;
        }
        continue;
      }
      i++;
    }
    i++;
  };
  while (i < n) {
    const c = src[i]!;
    if (c === '\n') {
      line++;
      i++;
      continue;
    }
    if (/\s/.test(c)) {
      i++;
      continue;
    }
    if (c === '/' && src[i + 1] === '/') {
      while (i < n && src[i] !== '\n') i++;
      continue;
    }
    if (c === '/' && src[i + 1] === '*') {
      i += 2;
      while (i < n && !(src[i] === '*' && src[i + 1] === '/')) {
        if (src[i] === '\n') line++;
        i++;
      }
      i += 2;
      continue;
    }
    const start = i;
    const at = line;
    if (c === "'" || c === '"') {
      skipString(c);
      out.push({ k: 'str', v: src.slice(start, i), line: at });
      continue;
    }
    if (c === '`') {
      skipTemplate();
      out.push({ k: 'tpl', v: src.slice(start, i), line: at });
      continue;
    }
    if (c === '/' && regexAfter(out[out.length - 1])) {
      i++;
      let cls = false;
      while (i < n && (cls || src[i] !== '/') && src[i] !== '\n') {
        if (src[i] === '\\') i++;
        else if (src[i] === '[') cls = true;
        else if (src[i] === ']') cls = false;
        i++;
      }
      i++;
      while (i < n && /[a-z]/.test(src[i]!)) i++;
      out.push({ k: 're', v: src.slice(start, i), line: at });
      continue;
    }
    if (/[A-Za-z_$]/.test(c)) {
      while (i < n && /[\w$]/.test(src[i]!)) i++;
      out.push({ k: 'id', v: src.slice(start, i), line: at });
      continue;
    }
    if (/[0-9]/.test(c)) {
      while (i < n && /[\w.]/.test(src[i]!)) i++;
      out.push({ k: 'num', v: src.slice(start, i), line: at });
      continue;
    }
    const two = src.slice(i, i + 3);
    const p = ['...', '===', '!=='].includes(two) ? two : ['?.', '=>', '==', '!=', '&&', '||', '??', '++', '+=', '<=', '>='].includes(src.slice(i, i + 2)) ? src.slice(i, i + 2) : c;
    i += p.length;
    out.push({ k: 'p', v: p, line: at });
  }
  return out;
}

// close returns the index of the bracket that closes the one at i.
function close(t: Tok[], i: number): number {
  const open = t[i]!.v;
  const shut = open === '(' ? ')' : open === '[' ? ']' : '}';
  let depth = 0;
  for (let j = i; j < t.length; j++) {
    if (t[j]!.k !== 'p') continue;
    if (t[j]!.v === open) depth++;
    else if (t[j]!.v === shut && --depth === 0) return j;
  }
  return t.length - 1;
}

// args splits the tokens between the brackets at open and its close at the top-level commas.
function args(t: Tok[], open: number, shut: number): Tok[][] {
  const out: Tok[][] = [];
  let cur: Tok[] = [];
  for (let j = open + 1; j < shut; j++) {
    const x = t[j]!;
    if (x.k === 'p' && (x.v === '(' || x.v === '[' || x.v === '{')) {
      const e = close(t, j);
      cur.push(...t.slice(j, e + 1));
      j = e;
      continue;
    }
    if (x.k === 'p' && x.v === ',') {
      out.push(cur);
      cur = [];
      continue;
    }
    cur.push(x);
  }
  if (cur.length) out.push(cur);
  return out;
}

const plainString = (a: Tok[]) => a.length === 1 && a[0]!.k === 'str';

// isLogger reports whether the receiver before the "." at dot is a logger: a name that ends in
// log or Log (this.elog, this.o.log, log), or a call of .for(...).
function isLogger(t: Tok[], dot: number): boolean {
  const r = t[dot - 1];
  if (!r) return false;
  if (r.k === 'id') return /log$/i.test(r.v);
  if (r.k === 'p' && r.v === ')') {
    for (let j = dot - 1, depth = 0; j >= 0; j--) {
      if (t[j]!.k !== 'p') continue;
      if (t[j]!.v === ')') depth++;
      else if (t[j]!.v === '(' && --depth === 0) return t[j - 1]?.v === 'for' && t[j - 2]?.v === '.';
    }
  }
  return false;
}

// checkValue checks one value of a logger call: exactly one call of a constructor.
function checkValue(a: Tok[], found: { push: (f: Finding) => unknown }): void {
  const at = a[0]?.line ?? 0;
  const head = a[0];
  if (a.length && head!.k === 'p' && head!.v === '...') a = a.slice(1);
  const name = a[0];
  if (!name || name.k !== 'id' || !CONSTRUCTORS.has(name.v) || a[1]?.v !== '(' || close(a, 1) !== a.length - 1) {
    found.push({ line: at, ban: 'value' });
    return;
  }
  const inner = args(a, 1, a.length - 1);
  if (KEYED.has(name.v) && !(inner[0] && plainString(inner[0]))) found.push({ line: at, ban: 'key' });
  for (const x of a) {
    if (x.k === 'tpl' || (x.k === 'p' && (x.v === '+' || x.v === '+='))) {
      found.push({ line: x.line, ban: 'built' });
      break;
    }
  }
}

// check returns the bans in one file's source. name is its base name.
export function check(name: string, src: string): Finding[] {
  const t = lex(src);
  const all: Finding[] = [];
  // log.ts itself writes to stderr, relays and mints values; its own lines follow the rest.
  const own = new Set(name === 'log.ts' ? ['stderr', 'relay', 'direct'] : []);
  const found = { push: (f: Finding) => (own.has(f.ban) ? 0 : all.push(f)) };
  const result = () => all;
  for (let i = 0; i < t.length; i++) {
    const x = t[i]!;
    if (x.k === 'str' && t[i - 1]?.v === '[' && t[i + 1]?.v === ']' && (METHODS.has(x.v.slice(1, -1)) || x.v.slice(1, -1) === 'line')) {
      found.push({ line: x.line, ban: 'computed' });
      continue;
    }
    if (x.k !== 'id') continue;
    const prevDot = t[i - 1]?.k === 'p' && (t[i - 1]!.v === '.' || t[i - 1]!.v === '?.');
    if (x.v === 'console' && !prevDot) found.push({ line: x.line, ban: 'console' });
    if (x.v === 'process' && !prevDot && t[i + 1]?.v === '.' && (t[i + 2]?.v === 'stderr' || t[i + 2]?.v === 'stdout')) found.push({ line: x.line, ban: 'stderr' });
    if (x.v === 'relayStderr' && !prevDot && t[i - 1]?.v !== 'import' && !RELAY_FILES.includes(name)) {
      // An import names it too: only a use counts.
      const inImport = t.slice(Math.max(0, i - 40), i).some((y) => y.v === 'import') && t.slice(i, i + 40).some((y) => y.v === 'from');
      if (!inImport) found.push({ line: x.line, ban: 'relay' });
    }
    if (x.v === 'Attr' && t[i - 1]?.v === 'new') found.push({ line: x.line, ban: 'direct' });
    if (x.v === 'text' && !prevDot && t[i + 1]?.v === '(' && t[i - 1]?.v !== 'function' && name !== 'log.ts') found.push({ line: x.line, ban: 'text' });
    if (!prevDot) continue;
    const called = t[i + 1]?.v === '(';
    if (x.v === 'for' && called) {
      // A component, as a message: a quoted string. Only a Log has a for method.
      const as = args(t, i + 1, close(t, i + 1));
      if (as.length !== 1 || !plainString(as[0]!)) found.push({ line: x.line, ban: 'component' });
      continue;
    }
    if (x.v === 'line' && isLogger(t, i - 1)) {
      found.push({ line: x.line, ban: 'direct' });
      continue;
    }
    if (!METHODS.has(x.v)) continue;
    if (!called) {
      // Used as a value: the receiver tells a logger from another object's field (ev.error).
      if (isLogger(t, i - 1)) found.push({ line: x.line, ban: 'as-value' });
      continue;
    }
    // Every call of a level's method is a logger's, whatever its receiver: the engine has no
    // other debug, info, warn or error method.
    const shut = close(t, i + 1);
    const as = args(t, i + 1, shut);
    if (!as[0] || !plainString(as[0])) found.push({ line: x.line, ban: 'message' });
    for (const a of as.slice(1)) checkValue(a, found);
  }
  return result();
}

// A free site: a word, session or kind call whose value is not a quoted string, with the
// message of the logger call it stands in ('' when it stands in none).
export interface FreeSite {
  line: number;
  msg: string;
  site: string; // word:<key>, session or kind
}

// freeSites returns one file's free sites, in source order.
export function freeSites(src: string): FreeSite[] {
  const t = lex(src);
  const out: FreeSite[] = [];
  for (let i = 0; i < t.length; i++) {
    const x = t[i]!;
    const prevDot = t[i - 1]?.k === 'p' && (t[i - 1]!.v === '.' || t[i - 1]!.v === '?.');
    if (x.k !== 'id' || !FORMED.has(x.v) || prevDot || t[i + 1]?.v !== '(' || t[i - 1]?.v === 'function') continue;
    const shut = close(t, i + 1);
    const as = args(t, i + 1, shut);
    const val = x.v === 'word' ? as[1] : as[0];
    if (val && plainString(val)) continue;
    const key = x.v === 'word' && as[0] && plainString(as[0]) ? as[0][0]!.v.slice(1, -1) : '?';
    out.push({ line: x.line, msg: enclosingMessage(t, i), site: x.v === 'word' ? `word:${key}` : x.v });
  }
  return out;
}

// enclosingMessage is the quoted message of the innermost logger call around the token at i.
function enclosingMessage(t: Tok[], i: number): string {
  for (let j = i - 1; j >= 1; j--) {
    if (!(t[j]!.k === 'p' && t[j]!.v === '(')) continue;
    const m = t[j - 1]!;
    if (m.k !== 'id' || !METHODS.has(m.v) || t[j - 2]?.v !== '.') continue;
    if (close(t, j) < i) continue;
    const first = args(t, j, close(t, j))[0];
    return first && plainString(first) ? first[0]!.v.slice(1, -1) : '';
  }
  return '';
}
