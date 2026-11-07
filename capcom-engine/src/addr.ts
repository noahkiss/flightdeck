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

// The engine route on the same socket.
export function enginePath(token: string): string {
  return `/capcom/${token}`;
}
