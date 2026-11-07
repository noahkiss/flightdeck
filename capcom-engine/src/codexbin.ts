// The Codex CLI the Codex engine runs: the binary pinned with the SDK, behind a wrapper.
//
// The SDK passes only `--config` overrides, but the engine needs two flags of `codex exec`:
// --ignore-user-config (the user's $CODEX_HOME/config.toml, with its MCP servers and features,
// never loads) and --ignore-rules (nor the user's rules). The wrapper puts them after `exec`, and
// --ephemeral as well when GFF_CODEX_EPHEMERAL is set (the start check and the summary save no
// thread). It is written into the engine folder at each start, 0700.
import { existsSync, promises as fs, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { EPHEMERAL_VAR } from './codex.js';

const TRIPLES: Record<string, string> = {
  'linux-x64': 'x86_64-unknown-linux-musl',
  'linux-arm64': 'aarch64-unknown-linux-musl',
  'darwin-x64': 'x86_64-apple-darwin',
  'darwin-arm64': 'aarch64-apple-darwin',
};

// findCodexBinary finds the native CLI of the @openai/codex package the SDK depends on, as the
// SDK itself does.
export function findCodexBinary(from: string = import.meta.url): string {
  const key = `${process.platform}-${process.arch}`;
  const triple = TRIPLES[key];
  if (!triple) throw new Error(`the Codex CLI has no build for ${key}`);
  // The SDK's exports offer only an `import` entry, which require.resolve cannot take: look
  // for its folder up the node_modules chain, as Node does.
  let sdkDir = '';
  for (let d = path.dirname(fileURLToPath(from)); ; d = path.dirname(d)) {
    const c = path.join(d, 'node_modules', '@openai', 'codex-sdk');
    if (existsSync(path.join(c, 'package.json'))) {
      sdkDir = c;
      break;
    }
    if (path.dirname(d) === d) throw new Error('the Codex SDK (@openai/codex-sdk) is not installed beside the engine');
  }
  const codexPkg = createRequire(path.join(sdkDir, 'dist', 'index.js')).resolve('@openai/codex/package.json');
  const platformPkg = createRequire(codexPkg).resolve(`@openai/codex-${key}/package.json`);
  const root = path.join(path.dirname(platformPkg), 'vendor', triple);
  for (const c of [path.join(root, 'bin', 'codex'), path.join(root, 'codex', 'codex')]) {
    try {
      if (statSync(c).isFile()) return c;
    } catch {
      // next
    }
  }
  throw new Error(`the Codex CLI is not in ${root}`);
}

function shQuote(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

export function wrapperScript(real: string): string {
  return [
    '#!/bin/sh',
    '# Written by gff-hab-capcom at each start: the pinned Codex CLI without the user\'s',
    '# config.toml and rules (capcom-service.md 3.4 on Codex).',
    'sub=$1',
    'shift',
    `if [ -n "$${EPHEMERAL_VAR}" ]; then`,
    '  set -- --ephemeral "$@"',
    'fi',
    `exec ${shQuote(real)} "$sub" --ignore-user-config --ignore-rules "$@"`,
    '',
  ].join('\n');
}

// writeWrapper writes the wrapper into dir, through a temporary file and a rename, and returns
// its path.
export async function writeWrapper(dir: string, real: string): Promise<string> {
  const p = path.join(dir, '.codex-exec');
  const tmp = `${p}.${process.pid}.tmp`;
  await fs.writeFile(tmp, wrapperScript(real), { mode: 0o700 });
  await fs.chmod(tmp, 0o700);
  await fs.rename(tmp, p);
  return p;
}
