// The engine's source check (docs/specs/diagnostics.md 10.2, test 10): it fails its fixture, one
// case per ban, and passes the engine's sources.
import assert from 'node:assert/strict';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { check, freeSites } from './logcheck.js';

const here = path.dirname(fileURLToPath(import.meta.url));

test('the source check finds each ban of its fixture, at its line', async () => {
  const src = await fs.readFile(path.join(here, 'fixtures', 'logcheck-bad.txt'), 'utf8');
  const want: { line: number; ban: string }[] = [];
  src.split('\n').forEach((l, i) => {
    const m = /\/\/ want: ([\w-]+)$/.exec(l);
    if (m) want.push({ line: i + 1, ban: m[1]! });
  });
  const bans = new Set(want.map((w) => w.ban));
  for (const b of ['console', 'stderr', 'relay', 'message', 'value', 'key', 'built', 'as-value', 'direct', 'component', 'text', 'computed']) assert.ok(bans.has(b), `the fixture has a case of ${b}`);
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

// The engine's free sites, like Go's TestFreeTextSites: every word, session or kind call whose
// value is not a quoted string, by file, message and constructor. Each value is an id, a fixed
// name, a model setting or a gff-hab message type, and its form check keeps prose out of its
// key. A new site fails this test until it is read and added here.
const FREE_SITES = [
  'codex.ts home entry moved aside word:entry',
  'codex.ts session started session',
  'codex.ts session started word:why',
  'engine.ts resume failed session',
  'engine.ts resume failed session',
  'engine.ts session started session',
  'engine.ts session started word:why',
  'engine.ts set model failed word:model',
  'route.ts route handler failed word:type',
  'route.ts route message failed every try word:type',
];

test('the engine free sites are the listed ones', async () => {
  const dir = path.join(here, '..', 'src');
  const files = (await fs.readdir(dir)).filter((f) => f.endsWith('.ts') && f !== 'log.ts').sort();
  const got: string[] = [];
  for (const f of files) for (const s of freeSites(await fs.readFile(path.join(dir, f), 'utf8'))) got.push(`${f} ${s.msg} ${s.site}`);
  assert.deepEqual(got.sort(), [...FREE_SITES].sort());
});

test('freeSites finds a site with no message and a multi-line call', () => {
  const src = "x.info(\n  'turn failed',\n  word('model', m),\n);\nconst a = session(id);\nlog.warn('ok', word('k', 'lit'), kind('fixed'));\n";
  assert.deepEqual(freeSites(src), [
    { line: 3, msg: 'turn failed', site: 'word:model' },
    { line: 5, msg: '', site: 'session' },
  ]);
});
