// gff-hab-capcom: gff-hab starts it as `node gff-hab-capcom.mjs` in <state>/capcom/engine/, with
// GFF_HAB, GFF_HAB_CAPCOM_TOKEN, GFF_HAB_CAPCOM_KIND (claude or codex; claude when unset) and
// GFF_HAB_CAPCOM_LOG (capcom.log's path), and CLAUDE_CONFIG_DIR or CODEX_HOME when set.
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Codex } from '@openai/codex-sdk';
import { CodexEngine } from './codex.js';
import { findCodexBinary, writeWrapper } from './codexbin.js';
import { Engine } from './engine.js';
import { errType, installCrashHandlers, Log, relayStderr, sinkFromEnv } from './log.js';

const exit = (code: number) => process.exit(code);
const sink = sinkFromEnv(process.env);
const log = new Log(sink);
installCrashHandlers(log, exit);

// A start that fails: its line in capcom.log has the error's type; its own words, which name a
// missing file or binary and hold no content, go to engine.log.
const fail = (e: unknown) => {
  log.for('engine').error('engine did not start', errType(e));
  relayStderr(`gff-hab-capcom: ${(e as Error).message}`);
  process.exit(2);
};

if (process.env.GFF_HAB_CAPCOM_KIND === 'codex') {
  try {
    const codexPath = await writeWrapper(process.cwd(), findCodexBinary());
    new CodexEngine({ codex: (o) => new Codex(o), codexPath, env: process.env, cwd: process.cwd(), exit, log: sink }).start();
  } catch (e) {
    fail(e);
  }
} else {
  try {
    new Engine({ query: (p) => query(p), env: process.env, cwd: process.cwd(), exit, log: sink }).start();
  } catch (e) {
    fail(e);
  }
}
