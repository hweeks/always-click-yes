// The extension has two front ends now, and one of them is still the point.
//
// The terminal launcher is unchanged and remains the default: acy's whole UI is
// its TUI, a VS Code integrated terminal renders it verbatim, and all that takes
// is finding the binary, picking the workspace folder, and keeping exactly one
// supervisor terminal alive per window — two supervisors on one session is the
// classic acy footgun, so "run" on a live terminal reveals it instead of
// relaunching.
//
// The panel (acy.openPanel) is the second: `acy serve` runs the identical
// supervisor headless over HTTP and a webview renders its frames. It is
// deliberately opt-in until its design lands — acy.useTerminal defaults to true,
// so acy.run still opens the terminal exactly as it always has. The same
// one-supervisor-per-project rule governs it, one panel and one `acy serve` per
// workspace folder.

import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as vscode from 'vscode';
import { findClaude, findCodex, prependDir, type ResolvedAgent } from './claude';
import {
  buildConfigSeed,
  renderConfigSeed,
  selectAgent,
  type AgentName,
  type Defaults,
} from './config';
import { serveArgs } from './endpoint';
import { needsChmod, resolveBinary, runArgs } from './launch';
import { PANEL_VIEW_TYPE, PanelHost, panelSerializer } from './panel';
import { ServeManager, type ServeSpec } from './serve';

const TERMINAL_NAME = 'acy';
const RELEASES_URL = 'https://github.com/hweeks/always-click-yes/releases/latest';
const CLAUDE_SETUP_URL = 'https://docs.claude.com/en/docs/claude-code/setup';
const CODEX_SETUP_URL = 'https://developers.openai.com/codex/cli';
const AGENT_MUTED_KEY = 'acy.agentMissingMuted';
const INSTALL_CLAUDE = 'Install Claude Code';
const INSTALL_CODEX = 'Install Codex CLI';

let terminal: vscode.Terminal | undefined;
let servers: ServeManager | undefined;

/** How a launched binary should be invoked, once the environment is settled. */
interface Launchable {
  binPath: string;
  /**
   * A PATH value that carries a discovered `claude`'s directory, or undefined
   * when the one we already have will do. Kept as a bare value rather than a
   * built environment because the two front ends want it in different shapes: a
   * terminal's `env` is an overlay VS Code merges, a spawn's `env` is the whole
   * environment and replaces it.
   */
  pathOverride: string | undefined;
}

export function activate(context: vscode.ExtensionContext): void {
  // stderr from `acy serve` lands here, and it is how a webview that will not
  // start gets diagnosed — there is no terminal showing the process any more.
  const output = vscode.window.createOutputChannel('acy');
  servers = new ServeManager(output);
  const panels = new PanelHost({
    extensionUri: context.extensionUri,
    servers,
    output,
    spec: (folder, continuePrior) => serveSpec(context, folder, continuePrior),
  });

  const statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 0);
  statusBar.text = '$(check-all) acy';
  statusBar.tooltip = 'acy: supervise a coding-agent task';
  statusBar.command = 'acy.start';
  statusBar.show();

  context.subscriptions.push(
    statusBar,
    output,
    servers,
    panels,
    vscode.commands.registerCommand('acy.run', () => launch(context, panels, false)),
    vscode.commands.registerCommand('acy.continue', () => launch(context, panels, true)),
    vscode.commands.registerCommand('acy.openPanel', () => openPanel(panels, false)),
    vscode.commands.registerCommand('acy.initConfig', () => initConfig()),
    // Not a contributed command: it is the status bar's chooser, and a palette
    // entry that only asks a question would be a third way to do two things.
    vscode.commands.registerCommand('acy.start', () => chooseFrontEnd(context, panels)),
    vscode.window.registerWebviewPanelSerializer(PANEL_VIEW_TYPE, panelSerializer(panels)),
    vscode.window.onDidCloseTerminal((t) => {
      if (t === terminal) {
        terminal = undefined;
      }
    }),
  );

  void checkAgentOnStartup(context);
}

export function deactivate(): void {
  // The terminal is deliberately not disposed: a running supervisor should
  // outlive an extension reload, not be killed by it.
  //
  // A served supervisor is the opposite. It is this extension host's own child
  // with no window of its own, so leaving it running would orphan a claude
  // session nobody can see, reach, or stop.
  servers?.dispose();
  servers = undefined;
}

/** The status bar asks which front end, since the panel is not the default yet. */
async function chooseFrontEnd(
  context: vscode.ExtensionContext,
  panels: PanelHost,
): Promise<void> {
  const TERMINAL = 'Plan & Run in a terminal';
  const PANEL = 'Open the acy panel (preview)';
  const pick = await vscode.window.showQuickPick([TERMINAL, PANEL], {
    placeHolder: 'How should acy supervise this project?',
  });
  if (pick === TERMINAL) {
    await launch(context, panels, false);
  } else if (pick === PANEL) {
    await openPanel(panels, false);
  }
}

async function openPanel(panels: PanelHost, continuePrior: boolean): Promise<void> {
  const folder = await pickFolder();
  if (folder) {
    await panels.open(folder, continuePrior);
  }
}

/**
 * What to spawn for one folder's `acy serve`.
 *
 * Deliberately the same resolution the terminal uses, PATH-prepending included:
 * a served supervisor is spawned with no shell exactly as the terminal one is,
 * so a `claude` found off PATH is just as unreachable to it.
 */
async function serveSpec(
  context: vscode.ExtensionContext,
  folder: vscode.WorkspaceFolder,
  continuePrior: boolean,
): Promise<ServeSpec | undefined> {
  const launchable = await resolveLaunchable(context, folder);
  if (!launchable) {
    return undefined;
  }
  return {
    binPath: launchable.binPath,
    cwd: folder.uri.fsPath,
    args: serveArgs(continuePrior),
    // A spawn's env replaces the environment outright, so the inherited one has
    // to be carried along with the override rather than standing in for it.
    env: launchable.pathOverride
      ? { ...process.env, PATH: launchable.pathOverride }
      : undefined,
  };
}

async function launch(
  context: vscode.ExtensionContext,
  panels: PanelHost,
  continuePrior: boolean,
): Promise<void> {
  // The switch exists so a later task can flip it; today it is true, and
  // acy.run opens the terminal exactly as it always has.
  if (!vscode.workspace.getConfiguration('acy').get<boolean>('useTerminal', true)) {
    await openPanel(panels, continuePrior);
    return;
  }

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
  const launchable = await resolveLaunchable(context, folder);
  if (!launchable) {
    return;
  }

  // The binary IS the "shell": no user shell in between means no rc files, no
  // quoting, and the terminal closes with the supervisor.
  terminal = vscode.window.createTerminal({
    name: TERMINAL_NAME,
    cwd: folder.uri,
    shellPath: launchable.binPath,
    shellArgs: runArgs(continuePrior),
    iconPath: new vscode.ThemeIcon('check-all'),
    // An overlay VS Code merges onto the terminal's environment, exactly as
    // before — not a replacement for it.
    env: launchable.pathOverride ? { PATH: launchable.pathOverride } : undefined,
  });
  terminal.show();
}

/**
 * Finds the acy binary and settles the environment it needs, reporting every
 * failure to the user and answering undefined when the launch should not
 * proceed.
 *
 * Shared by both front ends deliberately. The terminal spawns acy as its shell
 * and the panel spawns it as a child process, but neither puts a shell in
 * between — so the binary resolution, the chmod repair and the PATH-prepending
 * for an off-PATH `claude` are the same problem twice, and a copy of this would
 * be a copy that drifts.
 */
async function resolveLaunchable(
  context: vscode.ExtensionContext,
  folder: vscode.WorkspaceFolder,
): Promise<Launchable | undefined> {
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
    return undefined;
  }

  // VS Code's install path drops the executable bit off files unpacked from a
  // .vsix, so a bundled binary can arrive unrunnable however vsce recorded it.
  // Only bundled binaries: a setting or a PATH hit is the user's own file, and
  // we have no business changing its mode.
  if (bin.source === 'bundled' && process.platform !== 'win32' && !ensureExecutable(bin.path)) {
    return undefined;
  }

  // acy has nothing to supervise without the selected agent, and since it is
  // spawned with no shell the failure would surface as a dead terminal, not a message.
  const agent = await selectedAgent(folder);
  const executable = await resolveAgent(folder, agent);
  if (!executable) {
    const install = agent === 'codex' ? INSTALL_CODEX : INSTALL_CLAUDE;
    const pick = await vscode.window.showWarningMessage(
      `acy is configured for \`${agent}\`, but that CLI was not found on your PATH.`,
      install,
      'Run anyway',
    );
    if (pick === install) {
      void vscode.env.openExternal(vscode.Uri.parse(agentSetupURL(agent)));
      return undefined;
    }
    if (pick !== 'Run anyway') {
      return undefined;
    }
  }

  return {
    binPath: bin.path,
    // A well-known hit is by definition off the extension host's PATH, which
    // acy inherits verbatim — without this it could not exec claude either.
    // No --claude-bin flag: .acy.json is the source of truth for run settings.
    pathOverride:
      executable?.source === 'wellKnown'
        ? prependDir(process.env.PATH, path.dirname(executable.path), process.platform)
        : undefined,
  };
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
 * Resolves the selected agent for one folder: .acy.json, settings default,
 * PATH, then the well-known install dirs.
 */
async function resolveAgent(
  folder: vscode.WorkspaceFolder | undefined,
  agent: AgentName,
): Promise<ResolvedAgent | undefined> {
  const project: ProjectAgentConfig = folder
    ? await readProjectAgentConfig(folder)
    : { exists: false };
  const defaults = vscode.workspace.getConfiguration('acy').get<Defaults>('defaults');
  const find = agent === 'codex' ? findCodex : findClaude;
  return find({
    configPath: agent === 'codex' ? project.codexBin : project.claudeBin,
    settingPath: agent === 'codex' ? defaults?.codexBin : defaults?.claudeBin,
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

interface ProjectAgentConfig {
  exists: boolean;
  agent?: AgentName;
  claudeBin?: string;
  codexBin?: string;
}

/** Agent selection and binary paths from .acy.json. Malformed values are unset. */
async function readProjectAgentConfig(folder: vscode.WorkspaceFolder): Promise<ProjectAgentConfig> {
  let raw: Uint8Array;
  try {
    raw = await vscode.workspace.fs.readFile(vscode.Uri.joinPath(folder.uri, '.acy.json'));
  } catch {
    return { exists: false };
  }
  try {
    const parsed: unknown = JSON.parse(Buffer.from(raw).toString('utf8'));
    const cfg = parsed as { agent?: unknown; claudeBin?: unknown; codexBin?: unknown };
    return {
      exists: true,
      agent: cfg.agent === 'codex' || cfg.agent === 'claude' ? cfg.agent : undefined,
      claudeBin: typeof cfg.claudeBin === 'string' ? cfg.claudeBin : undefined,
      codexBin: typeof cfg.codexBin === 'string' ? cfg.codexBin : undefined,
    };
  } catch {
    return { exists: true };
  }
}

async function selectedAgent(folder: vscode.WorkspaceFolder | undefined): Promise<AgentName> {
  const project = folder ? await readProjectAgentConfig(folder) : { exists: false };
  const defaultAgent = vscode.workspace.getConfiguration('acy').get<Defaults>('defaults')?.agent;
  return selectAgent(project.agent, project.exists, defaultAgent);
}

function agentSetupURL(agent: AgentName): string {
  return agent === 'codex' ? CODEX_SETUP_URL : CLAUDE_SETUP_URL;
}

/**
 * Warns once per agent if its CLI is missing, so the first run isn't the
 * discovery. Never blocks activation — the launcher works regardless.
 */
async function checkAgentOnStartup(context: vscode.ExtensionContext): Promise<void> {
  const folder = vscode.workspace.workspaceFolders?.[0];
  const agent = await selectedAgent(folder);
  const mutedKey = `${AGENT_MUTED_KEY}.${agent}`;
  if (context.globalState.get<boolean>(mutedKey)) {
    return;
  }
  if (await resolveAgent(folder, agent)) {
    return;
  }
  const install = agent === 'codex' ? INSTALL_CODEX : INSTALL_CLAUDE;
  const pick = await vscode.window.showWarningMessage(
    `acy is configured for \`${agent}\` and cannot run without that CLI, which was not found.`,
    install,
    "Don't show again",
  );
  if (pick === install) {
    void vscode.env.openExternal(vscode.Uri.parse(agentSetupURL(agent)));
  } else if (pick === "Don't show again") {
    await context.globalState.update(mutedKey, true);
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
