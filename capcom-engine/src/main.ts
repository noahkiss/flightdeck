// gff-hab-capcom: gff-hab starts it as `node gff-hab-capcom.mjs` in <state>/capcom/engine/, with
// GFF_HAB, GFF_HAB_CAPCOM_TOKEN and GFF_HAB_CAPCOM_KIND (claude or codex; claude when unset), and
// CLAUDE_CONFIG_DIR or CODEX_HOME when set.
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Codex } from '@openai/codex-sdk';
import { CodexEngine } from './codex.js';
import { findCodexBinary, writeWrapper } from './codexbin.js';
import { Engine } from './engine.js';

const exit = (code: number) => process.exit(code);
const fail = (e: unknown) => {
  process.stderr.write(`gff-hab-capcom: ${(e as Error).message}\n`);
  process.exit(2);
};

if (process.env.GFF_HAB_CAPCOM_KIND === 'codex') {
  try {
    const codexPath = await writeWrapper(process.cwd(), findCodexBinary());
    new CodexEngine({ codex: (o) => new Codex(o), codexPath, env: process.env, cwd: process.cwd(), exit }).start();
  } catch (e) {
    fail(e);
  }
} else {
  try {
    new Engine({ query: (p) => query(p), env: process.env, cwd: process.cwd(), exit }).start();
  } catch (e) {
    fail(e);
  }
}
