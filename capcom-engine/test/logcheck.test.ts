// The engine's source check (docs/specs/diagnostics.md 10.2, test 10): it fails its fixture, one
// case per ban, and passes the engine's sources.
import assert from 'node:assert/strict';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { check } from './logcheck.js';

const here = path.dirname(fileURLToPath(import.meta.url));

test('the source check finds each ban of its fixture, at its line', async () => {
  const src = await fs.readFile(path.join(here, 'fixtures', 'logcheck-bad.txt'), 'utf8');
  const want: { line: number; ban: string }[] = [];
  src.split('\n').forEach((l, i) => {
    const m = /\/\/ want: ([\w-]+)$/.exec(l);
    if (m) want.push({ line: i + 1, ban: m[1]! });
  });
  const bans = new Set(want.map((w) => w.ban));
  for (const b of ['console', 'stderr', 'relay', 'message', 'value', 'key', 'built', 'as-value', 'direct']) assert.ok(bans.has(b), `the fixture has a case of ${b}`);
  const got = check('bad.ts', src).map((f) => ({ line: f.line, ban: f.ban }));
  assert.deepEqual(got, want);
});

test('the engine sources pass the source check', async () => {
  const dir = path.join(here, '..', 'src');
  const files = (await fs.readdir(dir)).filter((f) => f.endsWith('.ts'));
  assert.ok(files.includes('log.ts') && files.includes('engine.ts'));
  for (const f of files) {
    const found = check(f, await fs.readFile(path.join(dir, f), 'utf8'));
    assert.deepEqual(found, [], `${f}: ${JSON.stringify(found)}`);
  }
});
