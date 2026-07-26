// Pure launch logic: everything the extension decides that does not need the
// `vscode` module. Kept vscode-free so it runs under plain `node --test` — the
// extension-host harness is far too heavy a hammer for path arithmetic.

import * as path from 'path';

/** Where a resolved binary came from, in resolution order. */
export type BinarySource = 'setting' | 'bundled' | 'path';

export interface ResolvedBinary {
  /** Absolute path to the acy binary (or the user's setting verbatim). */
  path: string;
  source: BinarySource;
}

export interface ResolveOptions {
  /** The acy.binaryPath setting, if the user set one. Used verbatim. */
  settingPath?: string;
  /** The extension's install directory — platform builds ship bin/acy there. */
  extensionRoot: string;
  /** process.platform */
  platform: NodeJS.Platform;
  /** process.env.PATH */
  envPath?: string;
  /** Filesystem probe, injected for tests. */
  isFile: (p: string) => boolean;
}

/** The binary's file name on this platform. */
export function exeName(platform: NodeJS.Platform): string {
  return platform === 'win32' ? 'acy.exe' : 'acy';
}

/** The path a platform-specific .vsix bundles its binary at. */
export function bundledBinaryPath(extensionRoot: string, platform: NodeJS.Platform): string {
  return path.join(extensionRoot, 'bin', exeName(platform));
}

/** Scans PATH for the acy binary, returning an absolute path or undefined. */
export function findOnPath(
  envPath: string | undefined,
  platform: NodeJS.Platform,
  isFile: (p: string) => boolean,
): string | undefined {
  if (!envPath) {
    return undefined;
  }
  const sep = platform === 'win32' ? ';' : ':';
  for (const dir of envPath.split(sep)) {
    if (!dir) {
      continue;
    }
    const candidate = path.join(dir, exeName(platform));
    if (isFile(candidate)) {
      return candidate;
    }
  }
  return undefined;
}

/**
 * Resolves the acy binary: explicit setting → bundled with the extension →
 * PATH. An explicit setting is trusted verbatim even if the probe can't see it
 * (network drives, wrappers) — the terminal will say so loudly if it's wrong.
 * The other two must exist to be chosen.
 */
export function resolveBinary(opts: ResolveOptions): ResolvedBinary | undefined {
  const setting = opts.settingPath?.trim();
  if (setting) {
    return { path: setting, source: 'setting' };
  }
  const bundled = bundledBinaryPath(opts.extensionRoot, opts.platform);
  if (opts.isFile(bundled)) {
    return { path: bundled, source: 'bundled' };
  }
  const onPath = findOnPath(opts.envPath, opts.platform, opts.isFile);
  if (onPath) {
    return { path: onPath, source: 'path' };
  }
  return undefined;
}

/** Arguments for a supervisor launch. The .acy.json carries everything else. */
export function runArgs(continuePrior: boolean): string[] {
  return continuePrior ? ['run', '--continue'] : ['run'];
}
