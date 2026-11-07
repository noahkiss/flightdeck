// The Codex engine's start check (capcom-service.md 5.1 on Codex): run the pinned CLI once with
// the engine's own settings against a Responses API endpoint on 127.0.0.1 inside the engine, and
// read the tools its request offers the model. The endpoint answers with a short reply and makes
// no model call; the CLI runs under a throwaway home with a placeholder key, so no login or
// credential of the user's takes part.
import http from 'node:http';
import type { AddressInfo } from 'node:net';

export const START_CHECK_KEY = 'flightdeck-start-check';

export interface Probe {
  url: string; // the base URL to hand the CLI (…/v1)
  // body is the first Responses request the CLI made, once it made one.
  body(): unknown;
  close(): Promise<void>;
}

// startProbe starts the loopback endpoint.
export async function startProbe(): Promise<Probe> {
  let first: unknown;
  const server = http.createServer((req, res) => {
    let raw = '';
    req.setEncoding('utf8');
    req.on('data', (c) => {
      if (raw.length < 16 << 20) raw += c;
    });
    req.on('end', () => {
      if (req.method !== 'POST' || !(req.url ?? '').endsWith('/responses')) {
        res.writeHead(404);
        res.end();
        return;
      }
      if (first === undefined) {
        try {
          first = JSON.parse(raw);
        } catch {
          first = null;
        }
      }
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      const ev = (o: Record<string, unknown>) => res.write(`event: ${o.type}\ndata: ${JSON.stringify(o)}\n\n`);
      ev({ type: 'response.created', response: { id: 'resp_start_check' } });
      ev({
        type: 'response.output_item.done',
        item: { type: 'message', role: 'assistant', id: 'msg_start_check', content: [{ type: 'output_text', text: 'ok' }] },
      });
      ev({
        type: 'response.completed',
        response: { id: 'resp_start_check', usage: { input_tokens: 0, input_tokens_details: { cached_tokens: 0 }, output_tokens: 0, output_tokens_details: { reasoning_tokens: 0 }, total_tokens: 0 } },
      });
      res.end();
    });
  });
  // The CLI tries a WebSocket first; refusing it makes it fall back to HTTPS.
  server.on('upgrade', (_req, socket) => socket.destroy());
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `http://127.0.0.1:${port}/v1`,
    body: () => first,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections?.();
        server.close(() => resolve());
      }),
  };
}
