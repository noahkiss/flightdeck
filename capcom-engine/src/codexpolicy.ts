// The Codex engine's tool surface (capcom-service.md 5.1 on Codex) and its start check.
//
// Codex has no init message that lists its tools, so the start check runs the pinned CLI once
// against a loopback endpoint inside the engine and reads the tool list of the request it makes
// (startcheck.ts). These are the names that may be on it.

// Feature switches that take every built-in tool outside 5.1 off the request: the shell, code
// mode, sub-agents, images, plugins, apps, browsers, hooks, skills, memories (verified against
// the pinned CLI, 0.167.0). The start check proves the result.
export const CODEX_FEATURES_OFF: Readonly<Record<string, false>> = {
  shell_tool: false,
  unified_exec: false,
  shell_snapshot: false,
  multi_agent: false,
  multi_agent_v2: false,
  view_image: false,
  sleep_tool: false,
  apps: false,
  plugins: false,
  image_generation: false,
  browser_use: false,
  computer_use: false,
  in_app_browser: false,
  tool_suggest: false,
  skill_search: false,
  goals: false,
  hooks: false,
  memories: false,
};

// Tools the start check accepts. `request_user_input` is inert under `codex exec` (it answers
// "not supported in exec mode"); `apply_patch` runs under the read-only sandbox, which refuses
// every write, and a file change ends the turn (codex.ts); `tool_search` finds the deferred MCP
// tools of the one MCP server.
const ALLOWED = new Set([
  'web_search',
  'list_mcp_resources',
  'list_mcp_resource_templates',
  'read_mcp_resource',
  'tool_search',
  'request_user_input',
  'apply_patch',
]);

// onCodexSurface reports whether a tool name from the request is on the surface. A namespaced
// name is `<namespace>.<tool>`; the flightdeck MCP server's tools are `mcp__flightdeck.<tool>`
// or `mcp__flightdeck__<tool>`.
export function onCodexSurface(name: string): boolean {
  if (ALLOWED.has(name)) return true;
  return name.startsWith('mcp__flightdeck.') || name.startsWith('mcp__flightdeck__') || name === 'mcp__flightdeck';
}

// offeredTools lists the tools of one Responses API request body: the top-level `tools` and
// those of any `additional_tools` input item, a namespace's tools as `<namespace>.<tool>`, a
// hosted tool by its type.
export function offeredTools(body: unknown): string[] {
  const out: string[] = [];
  const walk = (list: unknown, prefix: string): void => {
    if (!Array.isArray(list)) return;
    for (const t of list) {
      if (!t || typeof t !== 'object') continue;
      const name = String((t as any).name ?? (t as any).type ?? '');
      if (Array.isArray((t as any).tools)) walk((t as any).tools, `${prefix}${name}.`);
      else if (name) out.push(prefix + name);
    }
  };
  const b = body as any;
  walk(b?.tools, '');
  if (Array.isArray(b?.input)) for (const i of b.input) if (i?.type === 'additional_tools') walk(i.tools, '');
  return out;
}

// The kinds of thread item a Capcom turn may hold. Any other (a command, a file change) means a
// tool outside 5.1 ran: the engine ends the turn and exits (the second guard after the start
// check).
export const TURN_ITEMS = new Set(['agent_message', 'reasoning', 'mcp_tool_call', 'web_search', 'todo_list', 'error']);
