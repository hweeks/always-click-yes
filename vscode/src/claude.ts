// Pure discovery logic for the coding-agent CLIs acy can supervise. Kept
// vscode-free like launch.ts so it runs under plain `node --test`.

import * as path from 'path';

/** Where a resolved claude came from, in resolution order. */
export type ClaudeSource = 'config' | 'setting' | 'path' | 'wellKnown';

export interface ResolvedClaude {
  /** Path to the claude CLI (or an explicit setting verbatim). */
  path: string;
  source: ClaudeSource;
}

export interface FindClaudeOptions {
  /** claudeBin from the project's .acy.json — the source of truth at run time. */
  configPath?: string;
  /** The acy.defaults.claudeBin setting, used when the project set nothing. */
  settingPath?: string;
  /** process.platform */
  platform: NodeJS.Platform;
  /** process.env.PATH */
  envPath?: string;
  /** os.homedir() */
  home?: string;
  /** process.env.APPDATA */
  appData?: string;
  /** Filesystem probe, injected for tests. */
  isFile: (p: string) => boolean;
}

export type ResolvedAgent = ResolvedClaude;
export type FindAgentOptions = FindClaudeOptions;

/**
 * The CLI's file names on this platform. Windows gets three because the npm
 * install writes a `.cmd` shim and the native installer an `.exe`.
 */
export function claudeExeNames(platform: NodeJS.Platform): string[] {
  return platform === 'win32' ? ['claude.exe', 'claude.cmd', 'claude.bat'] : ['claude'];
}

export function codexExeNames(platform: NodeJS.Platform): string[] {
  return platform === 'win32' ? ['codex.exe', 'codex.cmd', 'codex.bat'] : ['codex'];
}

/**
 * The places Claude Code installs itself. A GUI-launched VS Code inherits the
 * login environment, not a shell's, so these are routinely absent from PATH
 * even though `claude` works fine in the user's terminal.
 */
export function wellKnownDirs(
  platform: NodeJS.Platform,
  env: { home?: string; appData?: string },
): string[] {
  const dirs: string[] = [];
  if (env.home) {
    dirs.push(path.join(env.home, '.local', 'bin'), path.join(env.home, '.claude', 'local'));
  }
  if (platform === 'win32') {
    if (env.appData) {
      dirs.push(path.join(env.appData, 'npm'));
    }
  } else {
    dirs.push('/opt/homebrew/bin', '/usr/local/bin');
  }
  return dirs;
}

function findInDirs(
  dirs: string[],
  names: string[],
  isFile: (p: string) => boolean,
): string | undefined {
  for (const dir of dirs) {
    if (!dir) {
      continue;
    }
    for (const name of names) {
      const candidate = path.join(dir, name);
      if (isFile(candidate)) {
        return candidate;
      }
    }
  }
  return undefined;
}

/**
 * Resolves claude: .acy.json → setting → PATH → well-known install dirs. Like
 * resolveBinary in launch.ts, an explicit path is trusted verbatim without a
 * probe (network drives, wrappers); the scanned candidates must exist.
 */
export function findClaude(opts: FindClaudeOptions): ResolvedClaude | undefined {
  return findAgent(opts, claudeExeNames(opts.platform), wellKnownDirs(opts.platform, opts));
}

/** Resolves codex with the same precedence as claude. */
export function findCodex(opts: FindAgentOptions): ResolvedAgent | undefined {
  const dirs = wellKnownDirs(opts.platform, opts).filter(
    (dir) => !dir.endsWith(path.join('.claude', 'local')),
  );
  return findAgent(opts, codexExeNames(opts.platform), dirs);
}

function findAgent(
  opts: FindAgentOptions,
  exeNames: string[],
  knownDirs: string[],
): ResolvedAgent | undefined {
  const configured = opts.configPath?.trim();
  if (configured) {
    return { path: configured, source: 'config' };
  }
  const setting = opts.settingPath?.trim();
  if (setting) {
    return { path: setting, source: 'setting' };
  }
  const sep = opts.platform === 'win32' ? ';' : ':';
  const onPath = findInDirs((opts.envPath ?? '').split(sep), exeNames, opts.isFile);
  if (onPath) {
    return { path: onPath, source: 'path' };
  }
  const wellKnown = findInDirs(knownDirs, exeNames, opts.isFile);
  if (wellKnown) {
    return { path: wellKnown, source: 'wellKnown' };
  }
  return undefined;
}

/**
 * Puts `dir` first in a PATH value. acy is spawned as the terminal's shell, so
 * it inherits the extension host's PATH verbatim — a claude found off-PATH is
 * unreachable to it unless the directory travels along.
 */
export function prependDir(
  envPath: string | undefined,
  dir: string,
  platform: NodeJS.Platform,
): string {
  const sep = platform === 'win32' ? ';' : ':';
  if (!envPath) {
    return dir;
  }
  if (envPath.split(sep).some((entry) => entry === dir)) {
    return envPath;
  }
  return dir + sep + envPath;
}
