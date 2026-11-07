// Bundle the engine into one ESM file. The SDK stays external: it ships a
// native binary that it finds beside its own package.
import { build } from 'esbuild';

await build({
  entryPoints: ['src/main.ts'],
  outfile: 'dist/gff-hab-capcom.mjs',
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node18',
  external: ['@anthropic-ai/claude-agent-sdk'],
  logLevel: 'info',
});
