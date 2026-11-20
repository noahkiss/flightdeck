// Bundle the engine into one ESM file. The SDK stays external: it ships a
// native binary that it finds beside its own package. The metafile lists the
// packages the bundle holds, for the release notices (pack.mjs).
import { writeFile } from 'node:fs/promises';
import { build } from 'esbuild';

const result = await build({
  entryPoints: ['src/main.ts'],
  outfile: 'dist/gff-hab-capcom.mjs',
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node18',
  external: ['@anthropic-ai/claude-agent-sdk', '@openai/codex-sdk'],
  metafile: true,
  logLevel: 'info',
});
await writeFile('dist/meta.json', JSON.stringify(result.metafile));
