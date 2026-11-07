// The engine route (3.3, 4.4): an HTTP/1.1 Upgrade on the hook socket, then
// one JSON object per line both ways, with seq numbers, acks and resend.
import http from 'node:http';
import type { Duplex } from 'node:stream';
import { enginePath } from './addr.js';

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
  log: (line: string) => void;
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
    const fail = (why: string) => {
      if (done) return;
      done = true;
      this.o.log(`route: ${why}; retrying in ${this.backoff} ms`);
      this.retry();
    };
    req.on('upgrade', (res, socket, head) => {
      if (done) return;
      done = true;
      if (res.statusCode !== 101) {
        socket.destroy();
        this.o.log(`route: status ${res.statusCode}; retrying in ${this.backoff} ms`);
        this.retry();
        return;
      }
      this.attach(socket, head);
    });
    req.on('response', (res) => {
      res.resume();
      fail(`status ${res.statusCode}`);
    });
    req.on('error', (e) => fail(`connect failed: ${(e as Error).message}`));
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
        this.o.log('route: line over 4 MiB; dropping the connection');
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
      this.o.log(`route: connection dropped; retrying in ${this.backoff} ms`);
      this.retry();
    });
    if (head.length) onData(head);
  }

  private onLine(line: string): void {
    let m: Msg;
    try {
      m = JSON.parse(line);
    } catch {
      this.o.log('route: a line that is not JSON; ignored');
      return;
    }
    if (!m || typeof m !== 'object') return;
    if (m.type === 'ack') {
      this.onAck(Number((m as any).ack));
      return;
    }
    const sock = this.sock;
    // Messages are handled one at a time, in order, then acked.
    this.inQueue = this.inQueue.then(async () => {
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
        this.o.log(`route: handling ${String(m.type)} failed: ${(e as Error).message}`);
      }
      if (this.sock === sock && typeof m.seq === 'number') this.raw({ type: 'ack', ack: m.seq });
    });
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
