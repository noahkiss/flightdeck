// The engine home against the pinned Codex CLI: one run with the engine's options against the
// loopback endpoint of the start check, then a resume from the user's own home. No model call,
// no real login: both homes are throwaway folders. Skipped when the platform binary is missing.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { promises as fs } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { Codex, type ThreadOptions } from '@openai/codex-sdk';
import { CodexEngine } from '../src/codex.js';
import { findCodexBinary, writeWrapper } from '../src/codexbin.js';
import { startProbe } from '../src/startcheck.js';

function binary(): string | undefined {
  try {
    return findCodexBinary(new URL('../src/codexbin.ts', import.meta.url).href);
  } catch {
    return undefined;
  }
}
const bin = binary();

// Texts planted in the user's own Codex home and home folder. None may reach the engine's request.
const PLANTED = {
  agents: 'PLANTED_CODEX_HOME_AGENTS',
  skill: 'PLANTED_CODEX_HOME_SKILL',
  homeSkill: 'PLANTED_HOME_AGENTS_SKILL',
  homeSkillBody: 'PLANTED_HOME_AGENTS_SKILL_BODY',
};

async function skill(dir: string, name: string, description: string, body = 'body'): Promise<void> {
  await fs.mkdir(path.join(dir, name), { recursive: true });
  await fs.writeFile(path.join(dir, name, 'SKILL.md'), `---\nname: ${name}\ndescription: ${description}\n---\n${body}\n`);
}

// run makes one turn with the CLI and returns the thread id.
async function run(codex: Codex, opts: ThreadOptions, text: string, resume?: string): Promise<string | null> {
  const th = resume ? codex.resumeThread(resume, opts) : codex.startThread(opts);
  await th.run(text);
  return th.id;
}

test('the engine home keeps the user\'s global instructions, its skills and the bundled skills out, and codex resume finds the thread', { skip: !bin && 'no Codex CLI for this platform', timeout: 120_000 }, async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-codex-home-'));
  try {
    const user = path.join(dir, 'codex-home'); // the user's CODEX_HOME
    const home = path.join(dir, 'home'); // the user's HOME
    const cwd = path.join(dir, 'engine');
    await fs.mkdir(user);
    await fs.mkdir(cwd);
    await fs.writeFile(path.join(user, 'AGENTS.md'), `${PLANTED.agents}\n`);
    await skill(path.join(user, 'skills'), 'userskill', PLANTED.skill);
    await skill(path.join(home, '.agents', 'skills'), 'homeskill', PLANTED.homeSkill, PLANTED.homeSkillBody);
    await fs.writeFile(path.join(cwd, 'AGENTS.md'), 'the engine manual\n');
    const wrapper = await writeWrapper(cwd, bin!);
    const env = { PATH: process.env.PATH ?? '/usr/bin:/bin', CODEX_HOME: user, HOME: home };

    const engine = new CodexEngine({ codex: (o) => new Codex(o), codexPath: wrapper, env, cwd, exit: () => {} });
    await engine.prepareHome();
    const opts = { ...engine.threadOptions('gpt-5.5'), webSearchMode: 'disabled' as const };

    // The engine's run: a mention of each skill, as a user could type it.
    let probe = await startProbe();
    let first: string | null;
    try {
      const codex = new Codex(engine.codexOptions({ mcp: false, baseUrl: probe.url, apiKey: 'placeholder' }));
      first = await run(codex, opts, 'first turn: use $homeskill and $userskill');
    } finally {
      await probe.close();
    }
    const body = JSON.stringify(probe.body());
    assert.ok(body.includes('the engine manual'), 'the engine folder AGENTS.md loads');
    assert.ok(body.includes('first turn'));
    for (const [what, mark] of Object.entries(PLANTED)) assert.ok(!body.includes(mark), `${what} reached the request`);
    // The CLI's bundled system skills stay off (skills.bundled.enabled = false).
    for (const mark of ['skills/.system', 'skill-creator', 'skill-installer']) assert.ok(!body.includes(mark), `bundled skill ${mark} reached the request`);
    assert.ok(first);

    // A plain-terminal resume, in the user's own homes, finds the thread and its turn.
    probe = await startProbe();
    try {
      const codex = new Codex({ codexPathOverride: bin, env, baseUrl: probe.url, apiKey: 'placeholder' });
      assert.equal(await run(codex, opts, 'second turn', first), first);
    } finally {
      await probe.close();
    }
    const resumed = JSON.stringify(probe.body());
    assert.ok(resumed.includes('first turn'), 'the resume carries the first turn');
    assert.ok(resumed.includes('second turn'));
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('the pinned CLI writes a login through the engine home\'s auth.json link', { skip: !bin && 'no Codex CLI for this platform', timeout: 60_000 }, async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'capcom-codex-auth-'));
  try {
    const user = path.join(dir, 'codex-home');
    const engineHome = path.join(dir, 'engine-home');
    await fs.mkdir(user);
    await fs.mkdir(engineHome);
    await fs.writeFile(path.join(user, 'auth.json'), '{"OPENAI_API_KEY":"old"}');
    await fs.symlink(path.join(user, 'auth.json'), path.join(engineHome, 'auth.json'));
    const child = spawn(bin!, ['login', '--with-api-key'], { env: { PATH: process.env.PATH ?? '', CODEX_HOME: engineHome, HOME: engineHome }, stdio: ['pipe', 'ignore', 'ignore'] });
    child.stdin.end('new-placeholder-key');
    assert.equal(await new Promise((r) => child.on('exit', r)), 0);
    assert.ok((await fs.lstat(path.join(engineHome, 'auth.json'))).isSymbolicLink(), 'the link stays a link');
    assert.match(await fs.readFile(path.join(user, 'auth.json'), 'utf8'), /new-placeholder-key/);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
});
