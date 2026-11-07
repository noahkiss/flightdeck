// GFF_HAB parsing. The format matches internal/hook/hook.go:
// http+unix://<socket path>/hooks/<token>.

export const SCHEME = 'http+unix://';

export interface HabAddr {
  socket: string;
  token: string;
}

export function parseHabAddr(s: string | undefined): HabAddr {
  if (!s) throw new Error('GFF_HAB is not set');
  if (!s.startsWith(SCHEME)) throw new Error(`GFF_HAB does not start with ${SCHEME}`);
  const rest = s.slice(SCHEME.length);
  const i = rest.lastIndexOf('/hooks/');
  if (i <= 0) throw new Error('GFF_HAB has no /hooks/<token>');
  const socket = rest.slice(0, i);
  const token = rest.slice(i + '/hooks/'.length);
  if (!socket.startsWith('/') || !/^[0-9a-f]{16,128}$/.test(token)) {
    throw new Error('GFF_HAB is malformed');
  }
  return { socket, token };
}

// ENGINE_TOKEN_VAR names the engine route's own token (capcom-service.md 3.3). It differs from
// the pane route token in GFF_HAB, which the SDK's binary and gff-hab mcp get; this one never
// leaves the engine.
export const ENGINE_TOKEN_VAR = 'GFF_HAB_CAPCOM_TOKEN';

// engineToken reads and checks the engine route token.
export function engineToken(s: string | undefined): string {
  if (!s) throw new Error(`${ENGINE_TOKEN_VAR} is not set`);
  if (!/^[0-9a-f]{16,128}$/.test(s)) throw new Error(`${ENGINE_TOKEN_VAR} is malformed`);
  return s;
}

// The engine route on the same socket.
export function enginePath(token: string): string {
  return `/capcom/${token}`;
}
