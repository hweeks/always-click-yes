// The extension is a launcher, deliberately: acy's whole UI is its TUI, and a
// VS Code integrated terminal renders it verbatim. All this file does is find
// the binary, pick the workspace folder, and keep exactly one supervisor
// terminal alive per window — two supervisors on one session is the classic
// acy footgun, so "run" on a live terminal reveals it instead of relaunching.

import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as vscode from 'vscode';
import { findClaude, prependDir, type ResolvedClaude } from './claude';
import { buildConfigSeed, renderConfigSeed, type Defaults } from './config';
import { needsChmod, resolveBinary, runArgs } from './launch';

const TERMINAL_NAME = 'acy';
const RELEASES_URL = 'https://github.com/hweeks/always-click-yes/releases/latest';
const CLAUDE_SETUP_URL = 'https://docs.claude.com/en/docs/claude-code/setup';
const CLAUDE_MUTED_KEY = 'acy.claudeMissingMuted';
const INSTALL_CLAUDE = 'Install Claude Code';

let terminal: vscode.Terminal | undefined;

export function activate(context: vscode.ExtensionContext): void {
  const statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 0);
  statusBar.text = '$(check-all) acy';
  statusBar.tooltip = 'acy: Plan & Run — supervise a Claude Code task';
  statusBar.command = 'acy.run';
  statusBar.show();

  context.subscriptions.push(
    statusBar,
    vscode.commands.registerCommand('acy.run', () => launch(context, false)),
    vscode.commands.registerCommand('acy.continue', () => launch(context, true)),
    vscode.commands.registerCommand('acy.initConfig', () => initConfig()),
    vscode.window.onDidCloseTerminal((t) => {
      if (t === terminal) {
        terminal = undefined;
      }
    }),
  );

  void checkClaudeOnStartup(context);
}

export function deactivate(): void {
  // The terminal is deliberately not disposed: a running supervisor should
  // outlive an extension reload, not be killed by it.
}

async function launch(context: vscode.ExtensionContext, continuePrior: boolean): Promise<void> {
  // A supervisor is already up (or its terminal is still open): reveal it.
  // exitStatus is set once the process has ended — that terminal is a corpse
  // showing the final frame, so replace it rather than revealing it.
  if (terminal && terminal.exitStatus === undefined) {
    terminal.show();
    void vscode.window.showInformationMessage(
      'acy is already running in this window — one supervisor per session.',
    );
    return;
  }
  terminal?.dispose();
  terminal = undefined;

  const folder = await pickFolder();
  if (!folder) {
    return;
  }

  const bin = resolveBinary({
    settingPath: vscode.workspace.getConfiguration('acy').get<string>('binaryPath'),
    extensionRoot: context.extensionUri.fsPath,
    platform: process.platform,
    envPath: process.env.PATH,
    isFile: (p) => {
      try {
        return fs.statSync(p).isFile();
      } catch {
        return false;
      }
    },
  });
  if (!bin) {
    const pick = await vscode.window.showErrorMessage(
      'No acy binary found: set "acy.binaryPath", install a platform build of this extension, or put acy on your PATH.',
      'Open Settings',
      'Get acy',
    );
    if (pick === 'Open Settings') {
      void vscode.commands.executeCommand('workbench.action.openSettings', 'acy.binaryPath');
    } else if (pick === 'Get acy') {
      void vscode.env.openExternal(vscode.Uri.parse(RELEASES_URL));
    }
    return;
  }

  // VS Code's install path drops the executable bit off files unpacked from a
  // .vsix, so a bundled binary can arrive unrunnable however vsce recorded it.
  // Only bundled binaries: a setting or a PATH hit is the user's own file, and
  // we have no business changing its mode.
  if (bin.source === 'bundled' && process.platform !== 'win32' && !ensureExecutable(bin.path)) {
    return;
  }

  // acy has nothing to supervise without claude, and since it is spawned with
  // no shell the failure would surface as a dead terminal, not a message.
  const claude = await resolveClaude(folder);
  if (!claude) {
    const pick = await vscode.window.showWarningMessage(
      'acy supervises a `claude` session, and the Claude Code CLI was not found on your PATH.',
      INSTALL_CLAUDE,
      'Run anyway',
    );
    if (pick === INSTALL_CLAUDE) {
      void vscode.env.openExternal(vscode.Uri.parse(CLAUDE_SETUP_URL));
      return;
    }
    if (pick !== 'Run anyway') {
      return;
    }
  }

  // The binary IS the "shell": no user shell in between means no rc files, no
  // quoting, and the terminal closes with the supervisor.
  terminal = vscode.window.createTerminal({
    name: TERMINAL_NAME,
    cwd: folder.uri,
    shellPath: bin.path,
    shellArgs: runArgs(continuePrior),
    iconPath: new vscode.ThemeIcon('check-all'),
    // A well-known hit is by definition off the extension host's PATH, which
    // acy inherits verbatim — without this it could not exec claude either.
    // No --claude-bin flag: .acy.json is the source of truth for run settings.
    env:
      claude?.source === 'wellKnown'
        ? { PATH: prependDir(process.env.PATH, path.dirname(claude.path), process.platform) }
        : undefined,
  });
  terminal.show();
}

/**
 * Restores the executable bit VS Code's unpack may have stripped, reporting
 * false if it could not. A failure has to stop the launch: the terminal is the
 * binary itself, so launching regardless would replace this message with a
 * window that vanishes on a bare EACCES.
 */
function ensureExecutable(binPath: string): boolean {
  try {
    if (needsChmod(fs.statSync(binPath).mode)) {
      fs.chmodSync(binPath, 0o755);
    }
    return true;
  } catch {
    void vscode.window.showErrorMessage(
      `acy's bundled binary is not executable and could not be fixed: ${binPath} — run \`chmod +x ${binPath}\`, or set "acy.binaryPath" to a copy you can run.`,
    );
    return false;
  }
}

/**
 * Resolves claude for one folder: its .acy.json, then the settings default,
 * then PATH and the well-known install dirs.
 */
async function resolveClaude(
  folder: vscode.WorkspaceFolder | undefined,
): Promise<ResolvedClaude | undefined> {
  return findClaude({
    configPath: folder ? await readConfiguredClaudeBin(folder) : undefined,
    settingPath: vscode.workspace.getConfiguration('acy').get<Defaults>('defaults')?.claudeBin,
    platform: process.platform,
    envPath: process.env.PATH,
    home: os.homedir(),
    appData: process.env.APPDATA,
    isFile: (p) => {
      try {
        return fs.statSync(p).isFile();
      } catch {
        return false;
      }
    },
  });
}

/** claudeBin from the folder's .acy.json. Missing or malformed means "unset". */
async function readConfiguredClaudeBin(folder: vscode.WorkspaceFolder): Promise<string | undefined> {
  try {
    const raw = await vscode.workspace.fs.readFile(vscode.Uri.joinPath(folder.uri, '.acy.json'));
    const parsed: unknown = JSON.parse(Buffer.from(raw).toString('utf8'));
    const bin = (parsed as { claudeBin?: unknown })?.claudeBin;
    return typeof bin === 'string' ? bin : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Warns once per install if claude is missing, so the first run isn't the
 * discovery. Never blocks activation — the launcher works regardless.
 */
async function checkClaudeOnStartup(context: vscode.ExtensionContext): Promise<void> {
  if (context.globalState.get<boolean>(CLAUDE_MUTED_KEY)) {
    return;
  }
  if (await resolveClaude(vscode.workspace.workspaceFolders?.[0])) {
    return;
  }
  const pick = await vscode.window.showWarningMessage(
    'acy supervises a `claude` session and cannot run without the Claude Code CLI, which was not found.',
    INSTALL_CLAUDE,
    "Don't show again",
  );
  if (pick === INSTALL_CLAUDE) {
    void vscode.env.openExternal(vscode.Uri.parse(CLAUDE_SETUP_URL));
  } else if (pick === "Don't show again") {
    await context.globalState.update(CLAUDE_MUTED_KEY, true);
  }
}

/**
 * Creates .acy.json in the chosen folder, seeded from the acy.defaults.*
 * settings, and opens it. An existing file is opened, never overwritten —
 * the file, not the settings, is the source of truth once it exists.
 */
async function initConfig(): Promise<void> {
  const folder = await pickFolder();
  if (!folder) {
    return;
  }
  const target = vscode.Uri.joinPath(folder.uri, '.acy.json');

  let exists = true;
  try {
    await vscode.workspace.fs.stat(target);
  } catch {
    exists = false;
  }
  if (exists) {
    void vscode.window.showInformationMessage('.acy.json already exists — opened it instead.');
  } else {
    const defaults = vscode.workspace.getConfiguration('acy').get<Defaults>('defaults') ?? {};
    const body = renderConfigSeed(buildConfigSeed(defaults));
    await vscode.workspace.fs.writeFile(target, Buffer.from(body, 'utf8'));
  }
  await vscode.window.showTextDocument(target);
}

async function pickFolder(): Promise<vscode.WorkspaceFolder | undefined> {
  const folders = vscode.workspace.workspaceFolders ?? [];
  if (folders.length === 0) {
    void vscode.window.showErrorMessage('acy needs an open folder — it supervises a project directory.');
    return undefined;
  }
  if (folders.length === 1) {
    return folders[0];
  }
  return vscode.window.showWorkspaceFolderPick({
    placeHolder: 'Which folder should acy supervise?',
  });
}
