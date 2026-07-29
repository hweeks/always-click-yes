// Two bundles, because there are two runtimes.
//
// The extension host entry point runs in node, with `vscode` provided by the
// editor — external, so it is never inlined — and everything else (there are no
// runtime deps today) bundled, so the .vsix never ships node_modules.
//
// The webview client runs in a browser context with a CSP of `default-src
// 'none'` and a per-load nonce, so it has to be one plain script with no
// imports at load time: an IIFE, not an ES module.
import esbuild from 'esbuild';

const watch = process.argv.includes('--watch');

const builds = [
  {
    entryPoints: ['src/extension.ts'],
    bundle: true,
    platform: 'node',
    format: 'cjs',
    target: 'node20',
    external: ['vscode'],
    outfile: 'dist/extension.js',
    sourcemap: true,
  },
  {
    entryPoints: ['webview/main.ts'],
    bundle: true,
    platform: 'browser',
    format: 'iife',
    target: 'es2022',
    outfile: 'webview/dist/webview.js',
    sourcemap: true,
  },
];

const contexts = await Promise.all(builds.map((b) => esbuild.context(b)));

if (watch) {
  await Promise.all(contexts.map((c) => c.watch()));
} else {
  await Promise.all(contexts.map((c) => c.rebuild()));
  await Promise.all(contexts.map((c) => c.dispose()));
}
