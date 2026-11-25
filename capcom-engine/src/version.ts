// The engine's version and the pinned SDK versions (package.json).
// A test checks each SDK version against its pin.

// The release version, which build.mjs writes into the bundle from $GFF_ENGINE_VERSION (the
// release's version, the same as gff-hab's). gff-hab warns when the two differ.
declare const __GFF_ENGINE_VERSION__: string | undefined;
export const ENGINE_VERSION = typeof __GFF_ENGINE_VERSION__ === 'string' ? __GFF_ENGINE_VERSION__ : 'dev';
export const SDK_VERSION = '0.3.332';
export const CODEX_SDK_VERSION = '0.172.0';
