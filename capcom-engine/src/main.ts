// gff-hab-capcom: gff-hab starts it as `node gff-hab-capcom.mjs` in
// <state>/capcom/engine/, with GFF_HAB (and CLAUDE_CONFIG_DIR when set).
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Engine } from './engine.js';

const engine = new Engine({
  query: (p) => query(p),
  env: process.env,
  cwd: process.cwd(),
  exit: (code) => process.exit(code),
});

try {
  engine.start();
} catch (e) {
  process.stderr.write(`gff-hab-capcom: ${(e as Error).message}\n`);
  process.exit(2);
}
