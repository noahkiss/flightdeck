// The engine route (3.3, 4.4): an HTTP/1.1 Upgrade on the hook socket, then
// one JSON object per line both ways, with seq numbers, acks and resend.
import http from 'node:http';
import type { Duplex } from 'node:stream';
import { createHash } from 'node:crypto';
import { enginePath } from './addr.js';
import { type ComponentLog, count, dur, errClass, errorClass, errType, word } from './log.js';

// HANDLER_TRIES is how many times one message may fail its handler. Each failure drops the
// connection unacknowledged, so gff-hab sends the message again on the next one (diagnostics.md
// 1.2 #12); the last asks the engine to exit, so gff-hab's crash limit ends a loop.
export const HANDLER_TRIES = 3;

export const MAX_LINE = 4 * 1024 * 1024;

export type Msg = { type: string; seq?: number; [k: string]: unknown };

export interface RouteOptions {
  socket: string;
  token: string;
  backoffStartMs?: number;
  backoffMaxMs?: number;
  // Handles one message from gff-hab. Acked once it returns.
  onMessage: (m: Msg) => void | Promise<void>;
  // Answers a hello with the ready message's fields.
  onHello: (m: Msg) => Record<string, unknown> | Promise<Record<string, unknown>>;
  // Runs after each ready and the resend of the outbox.
  afterReady?: () => void;
  log: ComponentLog;
  // Called when one message failed its handler HANDLER_TRIES times: the engine exits.
  onFatal?: () => void;
}

interface Out {
  body: Record<string, unknown>;
  seq?: number; // seq on the current connection, once sent
}

export class Route {
  private o: RouteOptions;
  private sock?: Duplex;
  private greeted = false;
  private seq = 0;
  private outbox: Out[] = [];
  private closed = false;
  private backoff: number;
  private backoffMax: number;
  private inQueue: Promise<void> = Promise.resolve();
  private drainWaiters: (() => void)[] = [];
  private failures = new Map<string, number>(); // a message's digest: its handler's failures
  private failedSock?: Duplex; // the connection whose messages are left after a failure
  connects = 0;

  constructor(o: RouteOptions) {
    this.o = o;
    this.backoff = o.backoffStartMs ?? 1000;
    this.backoffMax = o.backoffMaxMs ?? 30000;
  }

  setBackoffMax(ms: number): void {
    if (Number.isFinite(ms) && ms >= 1000 && ms <= 600000) this.backoffMax = ms;
  }

  start(): void {
    this.connect();
  }

  // Queue a message; it is resent on each connection until acked.
  send(type: string, fields: Record<string, unknown> = {}): void {
    const out: Out = { body: { ...fields, type } };
    this.outbox.push(out);
    if (this.sock && this.greeted) this.write(out);
  }

  get pending(): number {
    return this.outbox.length;
  }

  // Wait until every queued message is acked, or the timeout.
  flush(timeoutMs: number): Promise<boolean> {
    if (this.outbox.length === 0) return Promise.resolve(true);
    return new Promise((resolve) => {
      const t = setTimeout(() => resolve(false), timeoutMs);
      this.drainWaiters.push(() => {
        clearTimeout(t);
        resolve(true);
      });
    });
  }

  close(): void {
    this.closed = true;
    this.sock?.destroy();
    this.sock = undefined;
  }

  // Drop the current connection; the route reconnects with backoff.
  drop(): void {
    this.sock?.destroy();
  }

  private write(out: Out): void {
    out.seq = ++this.seq;
    this.raw({ seq: out.seq, ...out.body });
  }

  private raw(obj: Record<string, unknown>): void {
    this.sock?.write(JSON.stringify(obj) + '\n');
  }

  private connect(): void {
    if (this.closed) return;
    const req = http.request({
      socketPath: this.o.socket,
      path: enginePath(this.o.token),
      method: 'POST',
      headers: { Host: 'gff-hab', Connection: 'Upgrade', Upgrade: 'gff-capcom/1', 'Content-Length': '0' },
    });
    let done = false;
    const refused = (status: number | undefined) => {
      if (done) return;
      done = true;
      this.o.log.warn('route refused', count('status', status ?? 0), dur('retry', this.backoff));
      this.retry();
    };
    req.on('upgrade', (res, socket, head) => {
      if (done) return;
      if (res.statusCode !== 101) {
        socket.destroy();
        refused(res.statusCode);
        return;
      }
      done = true;
      this.attach(socket, head);
    });
    req.on('response', (res) => {
      res.resume();
      refused(res.statusCode);
    });
    req.on('error', (e) => {
      if (done) return;
      done = true;
      this.o.log.warn('route connect failed', errType(e), dur('retry', this.backoff));
      this.retry();
    });
    req.end();
  }

  private retry(): void {
    if (this.closed) return;
    const wait = this.backoff;
    this.backoff = Math.min(this.backoff * 2, this.backoffMax);
    setTimeout(() => this.connect(), wait);
  }

  private attach(socket: Duplex, head: Buffer): void {
    this.connects++;
    this.sock = socket;
    this.greeted = false;
    this.seq = 0;
    for (const out of this.outbox) out.seq = undefined;
    this.backoff = this.o.backoffStartMs ?? 1000;
    let buf = '';
    const onData = (chunk: Buffer | string) => {
      buf += typeof chunk === 'string' ? chunk : chunk.toString('utf8');
      for (;;) {
        const i = buf.indexOf('\n');
        if (i < 0) break;
        const line = buf.slice(0, i);
        buf = buf.slice(i + 1);
        if (line.trim()) this.onLine(line);
      }
      if (buf.length > MAX_LINE) {
        this.o.log.warn('route line too long', count('bytes', buf.length));
        socket.destroy();
      }
    };
    socket.on('data', onData);
    socket.on('error', () => {});
    socket.on('close', () => {
      if (this.sock !== socket) return;
      this.sock = undefined;
      this.greeted = false;
      if (this.closed) return;
      this.o.log.warn('route dropped', dur('retry', this.backoff));
      this.retry();
    });
    if (head.length) onData(head);
  }

  private onLine(line: string): void {
    let m: Msg;
    try {
      m = JSON.parse(line);
    } catch {
      this.o.log.warn('route line not json');
      return;
    }
    if (!m || typeof m !== 'object') return;
    if (m.type === 'ack') {
      this.onAck(Number((m as any).ack));
      return;
    }
    const sock = this.sock;
    // Messages are handled one at a time, in order, then acked. After a handler failed, the
    // later inputs of that connection are left: gff-hab sends every input past the last take
    // again, in order, on the next one. Other messages run: gff-hab sends them once.
    this.inQueue = this.inQueue.then(async () => {
      if (sock && sock === this.failedSock && m.type === 'input') return;
      try {
        if (m.type === 'hello') {
          const ready = await this.o.onHello(m);
          if (this.sock !== sock) return;
          this.raw({ seq: ++this.seq, type: 'ready', ...ready });
          this.greeted = true;
          for (const out of this.outbox) this.write(out);
          this.o.afterReady?.();
        } else {
          await this.o.onMessage(m);
        }
      } catch (e) {
        this.handlerFailed(m, sock, e);
        return;
      }
      if (this.sock === sock && typeof m.seq === 'number') this.raw({ type: 'ack', ack: m.seq });
    });
  }

  // handlerFailed logs the failure by class, never its text, and leaves the message
  // unacknowledged: the connection drops, and gff-hab sends it again (diagnostics.md 1.2 #12,
  // 4.9). After HANDLER_TRIES failures of one message the engine exits instead.
  private handlerFailed(m: Msg, sock: Duplex | undefined, e: unknown): void {
    // The digest leaves out seq, which each connection numbers anew, wherever the key stands.
    const { seq: _seq, ...body } = m;
    const digest = createHash('sha256').update(JSON.stringify(body)).digest('hex');
    const n = (this.failures.get(digest) ?? 0) + 1;
    if (this.failures.size >= 64) this.failures.clear();
    this.failures.set(digest, n);
    const msg = e instanceof Error ? e.message : String(e);
    this.o.log.error('route handler failed', word('type', String(m.type)), count('tries', n), errType(e), errClass(errorClass(msg)));
    this.failedSock = sock;
    if (n >= HANDLER_TRIES) {
      this.o.log.error('route message failed every try', word('type', String(m.type)));
      this.o.onFatal?.();
      return;
    }
    sock?.destroy();
  }

  private onAck(n: number): void {
    if (!Number.isFinite(n)) return;
    this.outbox = this.outbox.filter((o) => o.seq === undefined || o.seq > n);
    if (this.outbox.length === 0) {
      const w = this.drainWaiters;
      this.drainWaiters = [];
      for (const f of w) f();
    }
  }
}
