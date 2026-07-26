// Bundles the extension host entry point. `vscode` stays external — the editor
// provides it at runtime — and everything else (there are no runtime deps
// today) would be inlined, so the .vsix never ships node_modules.
import esbuild from 'esbuild';

const watch = process.argv.includes('--watch');

const ctx = await esbuild.context({
  entryPoints: ['src/extension.ts'],
  bundle: true,
  platform: 'node',
  format: 'cjs',
  target: 'node20',
  external: ['vscode'],
  outfile: 'dist/extension.js',
  sourcemap: true,
});

if (watch) {
  await ctx.watch();
} else {
  await ctx.rebuild();
  await ctx.dispose();
}
