// The engine's structured log lines (docs/specs/diagnostics.md 4.2, 4.9): one JSON object per line
// in capcom.log, in the format gff-hab's diaglog writes, so gff-hab logs and the bundle read both
// alike. A line's message and component are string literals; every other value comes from a
// constructor of this file, and its key says how a bundle treats it (8.3). Values are strings or
// integers only: the bundle's first pass reads any other JSON value as a number, which is safe.
//
// The source check (test/logcheck.test.ts) keeps the other files to these rules (10.2): no
// console, no direct stderr write, messages and keys as plain literals, no template or
// concatenation in a constructor's value, and no text outside this file.
import fs from 'node:fs';
import path from 'node:path';

// LOG_PATH_VAR names capcom.log's path in the engine's environment; gff-hab sets it (4.3).
export const LOG_PATH_VAR = 'GFF_HAB_CAPCOM_LOG';

// The file's cap and its old files (4.3): capcom.log.1 and capcom.log.2.
export const LOG_MAX = 5 << 20;
export const LOG_KEEP = 2;

// MAX_LINE is the longest line written, newline included; a longer line has its longest values
// cut and marked with CUT (4.2).
export const MAX_LINE = 8 << 10;
export const CUT = '…(cut)';

// How often the file sink checks that its path still names its file (4.3).
const CHECK_LINES = 1000;
const CHECK_MS = 60_000;

// ---- the types that keep messages, components and keys constant ----

// Lit<S> is S when S is a string literal type, else never: a variable (string), a template with a
// value (a pattern such as `turn ${string}`), or a built string does not compile as a message,
// component or key. {} extends Record<S, 1> only when S makes an index signature: string or a
// pattern, never a literal.
type Lit<S extends string> = {} extends Record<S, 1> ? never : S;

// The keys a line always has, and the fixed keys of gff-hab's constructors (diaglog, 4.2). A
// caller's key may not be one: word, count and dur write a value under such a key as
// <key>_text, as diaglog does, and the type refuses it at compile time too.
const RESERVED = [
  't', 'lvl', 'proc', 'epoch', 'comp', 'msg', 'trace', 'suppressed',
  'session', 'pane', 'method', 'kind', 'harness', 'event', 'client', 'client_id',
  'host', 'label', 'device', 'device_was', 'project', 'branch', 'title', 'group', 'path', 'err', 'text',
] as const;
type Reserved = (typeof RESERVED)[number];
type Key<K extends string> = K extends Reserved ? never : Lit<K>;

const reserved = new Set<string>(RESERVED);

// The fixed keys this file writes itself.
const KEY_CLASS = 'class'; // an error class (4.9)
const KEY_ERR_TYPE = 'err_type'; // an error's constructor name
const KEY_CODE = 'code'; // a Node error code, such as ECONNRESET
const KEY_FRAMES = 'frames'; // a stack's function names

// Attr is one key and value of a line. Only this file's constructors make one: the constructor
// takes a token no other file holds, and the private fields make the type nominal, so an
// object literal of the same shape is no Attr. A line writes only real Attrs (marshal).
const MINT: unique symbol = Symbol('attr');
export class Attr {
  readonly #key: string;
  readonly #val: string | number;
  constructor(mint: typeof MINT, key: string, val: string | number) {
    if (mint !== MINT) throw new TypeError('an Attr comes from the constructors of log.ts');
    this.#key = key;
    this.#val = val;
  }
  get key(): string {
    return this.#key;
  }
  get val(): string | number {
    return this.#val;
  }
}
const attr = (key: string, val: string | number) => new Attr(MINT, key, val);

// ---- value forms, as diaglog checks them ----

// idForm: an id the hab or an SDK minted: letters, digits and . _ - / : @, at most 128 bytes.
export function idForm(s: string): boolean {
  return s.length > 0 && s.length <= 128 && /^[A-Za-z0-9._\-/:@]+$/.test(s);
}

// wordForm: a known word: letters, digits and . _ -, at most 64 bytes. No space, so no prose.
export function wordForm(s: string): boolean {
  return s.length > 0 && s.length <= 64 && /^[A-Za-z0-9._-]+$/.test(s);
}

const checked = (key: string, v: string, ok: boolean): Attr => (ok ? attr(key, v) : attr(key + '_text', v));
const callerKey = (key: string): string => (reserved.has(key) ? key + '_text' : key);

// ---- the constructors ----

// session is an SDK session or Codex thread id (safe).
export function session(id: string): Attr {
  return checked('session', id, idForm(id));
}

// kind is a kind word (safe): a message type, an error kind.
export function kind(k: string): Attr {
  return checked('kind', k, wordForm(k));
}

// word is a known word under a key of the caller's. A value that is not a word is kept as
// <key>_text.
export function word<K extends string>(key: Key<K>, w: string): Attr {
  const k = callerKey(key);
  return wordForm(w) && k === key ? attr(k, w) : attr(k === key ? k + '_text' : k, w);
}

// count is an integer under a key of the caller's: a count, a size, a status code.
export function count<K extends string>(key: Key<K>, n: number): Attr {
  return attr(callerKey(key), Number.isFinite(n) ? Math.trunc(n) : 0);
}

// dur is a duration in milliseconds under <key>_ms.
export function dur<K extends string>(key: Key<K>, ms: number): Attr {
  return attr(callerKey(key + '_ms'), Number.isFinite(ms) ? Math.round(ms) : 0);
}

// The error classes of a provider's error (4.9). Never the error's text.
const ERR_CLASSES = ['auth', 'rate_limit', 'network', 'provider', 'other'] as const;
export type ErrClass = (typeof ERR_CLASSES)[number];
const errClasses = new Set<string>(ERR_CLASSES);

// errorClass reads an error's text for its class only: the text itself is never logged, since a
// provider can echo a prompt fragment in it (1.5).
export function errorClass(text: string): ErrClass {
  if (/\b40[13]\b|unauthori[sz]ed|forbidden|invalid[_ ]api[_ ]key|authentication|not logged in|log ?in again|credential/i.test(text)) return 'auth';
  if (/\b429\b|rate[_ -]?limit|too many requests|quota|usage limit/i.test(text)) return 'rate_limit';
  if (/\bE(CONNREFUSED|CONNRESET|NOTFOUND|TIMEDOUT|AI_AGAIN|HOSTUNREACH|NETUNREACH|PIPE)\b|socket hang up|fetch failed|network|timed? ?out|stream disconnected|connection (reset|closed|refused)/i.test(text)) return 'network';
  if (/\b5\d\d\b|overloaded|server error|bad gateway|service unavailable|api error|provider/i.test(text)) return 'provider';
  return 'other';
}

// errClass is the class of an error's text, under class. The value is checked at run time too:
// a cast (as ErrClass) passes the type and the source check, and class is a plain key, so a
// value that is not one of the five classes is written as other.
export function errClass(c: ErrClass): Attr {
  return attr(KEY_CLASS, typeof c === 'string' && errClasses.has(c) ? c : 'other');
}

// errType is what an error is, never what it says: its constructor's name under err_type and a
// Node error code under code. Neither holds the message.
export function errType(e: unknown): Attr[] {
  const out: Attr[] = [];
  const name = e instanceof Error ? e.constructor?.name || e.name : typeof e;
  out.push(wordForm(name) ? attr(KEY_ERR_TYPE, name) : attr(KEY_ERR_TYPE, 'unknown'));
  const code = (e as { code?: unknown } | null)?.code;
  if (typeof code === 'string' && /^[A-Z][A-Z0-9_]{1,63}$/.test(code)) out.push(attr(KEY_CODE, code));
  else if (typeof code === 'number' && Number.isInteger(code)) out.push(attr(KEY_CODE, code));
  return out;
}

// frames is a stack's function names, innermost first, at most 12: no file path and no message,
// which may hold content (4.9). The header line V8 writes (the error's name and message) is
// skipped whole, so a message line that looks like a frame is never read as one.
export function frames(e: unknown): Attr {
  const names: string[] = [];
  if (e instanceof Error && typeof e.stack === 'string') {
    const header = String(e);
    const body = e.stack.startsWith(header) ? e.stack.slice(header.length) : '';
    for (const line of body.split('\n')) {
      const m = /^\s+at (?:async )?(?:new )?([A-Za-z_$][\w$.<>]*)(?: \[as [\w$]+\])? \(/.exec(line);
      if (m) names.push(m[1]!);
      else if (/^\s+at /.test(line)) names.push('<anonymous>');
      if (names.length >= 12) break;
    }
  }
  return attr(KEY_FRAMES, names.join(' '));
}

// text is free text (class text): a bundle gives it pass 2 only. The engine writes no content
// with it: only this file calls it, for the CUT marker; the source check bans it elsewhere.
export function text(s: string): Attr {
  return attr('text', s);
}

// ---- lines ----

export type Level = 'debug' | 'info' | 'warn' | 'error';
const LEVELS: Record<Level, number> = { debug: -4, info: 0, warn: 4, error: 8 };

// Sink takes one line, newline included; false when it could not be written.
export type Sink = (line: string) => unknown;

// Log writes the lines of one engine process.
export class Log {
  private dropped = 0;
  constructor(
    private sink: Sink,
    private redact: (s: string) => string = (s) => s,
    private floor: Level = 'info',
    private now: () => Date = () => new Date(),
  ) {}

  // for returns the logger of a component.
  for<C extends string>(comp: Lit<C>): ComponentLog {
    return new ComponentLog(this, comp);
  }

  // line writes one line at once, synchronously: the crash handlers write theirs just before the
  // exit.
  line(lvl: Level, comp: string, msg: string, attrs: (Attr | Attr[])[]): void {
    if (LEVELS[lvl] < LEVELS[this.floor]) return;
    const flat = attrs.flat();
    if (this.dropped > 0) {
      const n = this.dropped;
      this.dropped = 0;
      if (this.sink(encode(this.now(), 'warn', 'log', 'lines dropped', [count('lines', n)], this.redact)) === false) this.dropped += n;
    }
    if (this.sink(encode(this.now(), lvl, comp, msg, flat, this.redact)) === false) this.dropped++;
  }
}

// ComponentLog writes one component's lines. msg is a string literal; values go in attrs.
export class ComponentLog {
  constructor(
    private log: Log,
    private comp: string,
  ) {}
  debug<M extends string>(msg: Lit<M>, ...attrs: (Attr | Attr[])[]): void {
    this.log.line('debug', this.comp, msg, attrs);
  }
  info<M extends string>(msg: Lit<M>, ...attrs: (Attr | Attr[])[]): void {
    this.log.line('info', this.comp, msg, attrs);
  }
  warn<M extends string>(msg: Lit<M>, ...attrs: (Attr | Attr[])[]): void {
    this.log.line('warn', this.comp, msg, attrs);
  }
  error<M extends string>(msg: Lit<M>, ...attrs: (Attr | Attr[])[]): void {
    this.log.line('error', this.comp, msg, attrs);
  }
}

// encode is one JSON line, newline included, at most MAX_LINE bytes: while it is longer, every
// string value longer than a cap is cut at the cap, and the cap halves. Keys come in diaglog's
// order: t, lvl, msg, proc, comp, then the values.
export function encode(t: Date, lvl: Level, comp: string, msg: string, attrs: Attr[], redact: (s: string) => string = (s) => s): string {
  let limit = MAX_LINE;
  for (let i = 0; i < 10; i++) {
    const s = marshal(t, lvl, comp, msg, attrs, redact, limit);
    if (Buffer.byteLength(s) <= MAX_LINE) return s;
    limit = Math.floor(limit / 2);
  }
  return marshal(t, lvl, comp, msg, [text(CUT)], redact, MAX_LINE);
}

function marshal(t: Date, lvl: Level, comp: string, msg: string, attrs: Attr[], redact: (s: string) => string, limit: number): string {
  const o: Record<string, string | number> = { t: t.toISOString(), lvl, msg, proc: 'capcom', comp };
  for (const a of attrs) {
    if (!(a instanceof Attr) || !a.key || a.key in o) continue;
    o[a.key] = typeof a.val === 'number' ? a.val : cut(redact(a.val), limit);
  }
  return JSON.stringify(o) + '\n';
}

// cut shortens s to at most limit bytes of UTF-8 on a character boundary, marked with CUT.
export function cut(s: string, limit: number): string {
  const b = Buffer.from(s, 'utf8');
  if (b.length <= limit) return s;
  let n = Math.max(limit - Buffer.byteLength(CUT), 0);
  while (n > 0 && (b[n]! & 0xc0) === 0x80) n--;
  return b.subarray(0, n).toString('utf8') + CUT;
}

// ---- capcom.log ----

// FileSink appends lines to capcom.log, the engine its only writer (4.3). It renames the file
// to .1 at its cap, the older files one step on; at every CHECK_LINES lines or CHECK_MS it
// checks that its path still names the open file, and reopens it when not (the user removed the
// logs folder, 12). A rename that fails leaves the file to grow until that check. A write that
// fails, as on a full disk, drops the line; Log counts it.
export class FileSink {
  private fd = -1;
  private ino = 0;
  private size = 0;
  private lines = 0;
  private checked = 0;
  // stuck: the last rotation could not rename the file (EACCES, say). Rotation waits for the next
  // periodic check instead of running again on every line.
  private stuck = false;
  constructor(
    readonly file: string,
    private max = LOG_MAX,
    private keep = LOG_KEEP,
  ) {}

  write = (line: string): boolean => {
    try {
      if (this.fd < 0 || ++this.lines >= CHECK_LINES || Date.now() - this.checked >= CHECK_MS) {
        this.stuck = false;
        this.check();
      }
      const n = Buffer.byteLength(line);
      if (this.size > 0 && this.size + n > this.max) this.rotate();
      fs.writeSync(this.fd, line);
      this.size += n;
      return true;
    } catch {
      this.close();
      return false;
    }
  };

  private check(): void {
    this.lines = 0;
    this.checked = Date.now();
    if (this.fd >= 0) {
      try {
        const st = fs.statSync(this.file);
        if (st.ino === this.ino) {
          this.size = fs.fstatSync(this.fd).size;
          return;
        }
      } catch {
        // gone: reopen
      }
      this.close();
    }
    fs.mkdirSync(path.dirname(this.file), { recursive: true, mode: 0o700 });
    this.fd = fs.openSync(this.file, 'a', 0o600);
    const st = fs.fstatSync(this.fd);
    this.ino = st.ino;
    this.size = st.size;
  }

  private rotate(): void {
    if (this.stuck) return;
    const ino = this.ino;
    this.close();
    for (let i = this.keep; i >= 1; i--) {
      const from = i === 1 ? this.file : `${this.file}.${i - 1}`;
      try {
        fs.renameSync(from, `${this.file}.${i}`);
      } catch {
        // not there
      }
    }
    this.check();
    // The same file reopened: its rename failed.
    if (this.ino === ino) this.stuck = true;
  }

  close(): void {
    if (this.fd >= 0) {
      try {
        fs.closeSync(this.fd);
      } catch {
        // already closed
      }
    }
    this.fd = -1;
  }
}

// relayStderr writes raw text to the engine's own stderr, which gff-hab appends to engine.log:
// the SDK's stderr lines (counted only in capcom.log, 4.9), and a start failure's own words.
// engine.log is in no bundle (8.2). The source check allows it in the listed files only.
export function relayStderr(s: string): void {
  try {
    process.stderr.write(s.endsWith('\n') ? s : s + '\n');
  } catch {
    // no stderr
  }
}

// stderrSink writes lines to stderr, for an engine started with no capcom.log path.
export const stderrSink: Sink = (line) => {
  relayStderr(line);
};

// sinkFromEnv is capcom.log at the path gff-hab gave, else stderr.
export function sinkFromEnv(env: NodeJS.ProcessEnv): Sink {
  const p = env[LOG_PATH_VAR];
  return p ? new FileSink(p).write : stderrSink;
}

// installCrashHandlers makes an uncaught exception and an unhandled rejection each write one
// error line, with the error's type and its frames' function names, then exit 1 (4.9). gff-hab
// counts the exit as a crash and records it (6.3). main.ts installs them: tests build engines in
// this process and keep their own handlers.
export function installCrashHandlers(log: Log, exit: (code: number) => void): void {
  const crash = log.for('crash');
  // The exit runs even when the line throws (a Proxy, a stack getter that throws): the crash is
  // still counted, with no line.
  process.on('uncaughtException', (e) => {
    try {
      crash.error('uncaught exception', errType(e), frames(e));
    } finally {
      exit(1);
    }
  });
  process.on('unhandledRejection', (e) => {
    try {
      crash.error('unhandled rejection', errType(e), frames(e));
    } finally {
      exit(1);
    }
  });
}
