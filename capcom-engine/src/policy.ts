// The tool surface (5.1) and the web fetch rule (5.3).

// Built-in tools denied by name (5.1).
export const DENY_TOOLS: readonly string[] = [
  'Bash',
  'Read',
  'Edit',
  'Write',
  'Glob',
  'Grep',
  'Agent',
  'Skill',
  'ToolSearch',
  'AskUserQuestion',
  'TaskCreate',
  'TaskUpdate',
  'NotebookEdit',
];

// The SDK's MCP resource tools. They read only what an MCP server offers, so they are on the
// surface while flightdeck is the only MCP server (5.1, Q5).
export const RESOURCE_TOOLS: readonly string[] = ['ListMcpResourcesTool', 'ReadMcpResourceTool'];

// Tools allowed with no callback (5.1). WebFetch is never here.
export const ALLOW_TOOLS: readonly string[] = ['mcp__flightdeck__*', 'WebSearch', ...RESOURCE_TOOLS];

// The built-in base set. Deny rules match names only, so this keeps a
// built-in the deny list does not name out of the session.
export const BASE_TOOLS: readonly string[] = ['WebSearch', 'WebFetch'];

// A tool the start check accepts. servers are the MCP server names the SDK's init lists: the
// resource tools pass only when flightdeck is the only one.
export function onSurface(tool: string, servers: readonly string[] = []): boolean {
  if (RESOURCE_TOOLS.includes(tool)) {
    return servers.length > 0 && servers.every((s) => s === 'flightdeck');
  }
  return tool === 'WebSearch' || tool === 'WebFetch' || tool.startsWith('mcp__flightdeck__');
}

const URL_RE = /https?:\/\/[^\s<>"'`\\]+/g;

// Every http(s) URL in a text, with trailing punctuation cut.
export function extractUrls(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(URL_RE)) {
    let u = m[0];
    u = u.replace(/[.,;:!?)\]}>]+$/, '');
    if (u.length > 8) out.push(u);
  }
  return out;
}

// The form two URLs are compared in: scheme, host and path, with no
// fragment and no trailing slash. Undefined for a URL that does not parse.
export function urlKey(raw: string): string | undefined {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return undefined;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return undefined;
  let p = u.pathname;
  if (p.length > 1 && p.endsWith('/')) p = p.slice(0, -1);
  return `${u.protocol}//${u.host.toLowerCase()}${p}${u.search}`;
}

// A set of URLs compared by urlKey.
export class UrlSet {
  private keys = new Set<string>();
  add(raw: string): void {
    const k = urlKey(raw);
    if (k) this.keys.add(k);
  }
  addText(text: string): void {
    for (const u of extractUrls(text)) this.add(u);
  }
  has(raw: string): boolean {
    const k = urlKey(raw);
    return k !== undefined && this.keys.has(k);
  }
  clear(): void {
    this.keys.clear();
  }
  get size(): number {
    return this.keys.size;
  }
}

// Collect the text of any value: strings, arrays and object fields, to a
// depth, for URLs in tool results.
export function textOf(v: unknown, depth = 0): string {
  if (depth > 6 || v == null) return '';
  if (typeof v === 'string') return v;
  if (Array.isArray(v)) return v.map((x) => textOf(x, depth + 1)).join('\n');
  if (typeof v === 'object') return Object.values(v as object).map((x) => textOf(x, depth + 1)).join('\n');
  return '';
}

// A group plan's title, from group_create's plan JSON (groups.md 15.2); '' when it has none.
function planTitle(plan: unknown): string {
  if (typeof plan !== 'string') return '';
  try {
    const p = JSON.parse(plan) as { title?: unknown };
    return typeof p?.title === 'string' ? p.title : '';
  } catch {
    return '';
  }
}

const COORD_KEYS = new Set(['lat', 'lon', 'latitude', 'longitude', 'lng']);

// One line for the folded tool line (7.2).
export function toolSummary(name: string, input: unknown): string {
  let s = '';
  if (input && typeof input === 'object') {
    const o = input as Record<string, unknown>;
    s = planTitle(o.plan);
    for (const k of ['url', 'query', 'q', 'project', 'title', 'name', 'place', 'near', 'member', 'group', 'text', 'message']) {
      if (s) break;
      if (typeof o[k] === 'string' && o[k]) {
        s = String(o[k]);
        break;
      }
    }
    if (!s) {
      // Never a position (capcom-service.md 8.1): lat and lon stay out of the tool line.
      const first = Object.entries(o).find(([k, x]) => !COORD_KEYS.has(k) && typeof x === 'string' && x)?.[1];
      if (typeof first === 'string') s = first;
    }
  }
  const short = name.startsWith('mcp__flightdeck__') ? name.slice('mcp__flightdeck__'.length) : name;
  const line = (s ? `${short}: ${s}` : short).replace(/\s+/g, ' ').trim();
  return line.length > 120 ? line.slice(0, 119) + '…' : line;
}

// The site a URL is on, as gff-hab keeps "Allow this site" (5.3): the host,
// lowercase, with no port, no trailing dot and no leading "www.". Undefined
// for a URL that is not http or https.
export function siteOf(raw: string): string | undefined {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return undefined;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return undefined;
  let h = u.hostname.toLowerCase();
  if (h.endsWith('.')) h = h.slice(0, -1);
  if (h.startsWith('www.')) h = h.slice(4);
  return h || undefined;
}
