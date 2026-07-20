// The extension is a launcher, deliberately: acy's whole UI is its TUI, and a
// VS Code integrated terminal renders it verbatim. All this file does is find
// the binary, pick the workspace folder, and keep exactly one supervisor
// terminal alive per window — two supervisors on one session is the classic
// acy footgun, so "run" on a live terminal reveals it instead of relaunching.

import * as fs from 'fs';
import * as vscode from 'vscode';
import { resolveBinary, runArgs } from './launch';

const TERMINAL_NAME = 'acy';
const RELEASES_URL = 'https://github.com/hweeks/always-click-yes/releases/latest';

let terminal: vscode.Terminal | undefined;

export function activate(context: vscode.ExtensionContext): void {
  context.subscriptions.push(
    vscode.commands.registerCommand('acy.run', () => launch(context, false)),
    vscode.commands.registerCommand('acy.continue', () => launch(context, true)),
    vscode.window.onDidCloseTerminal((t) => {
      if (t === terminal) {
        terminal = undefined;
      }
    }),
  );
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

  // The binary IS the "shell": no user shell in between means no rc files, no
  // quoting, and the terminal closes with the supervisor.
  terminal = vscode.window.createTerminal({
    name: TERMINAL_NAME,
    cwd: folder.uri,
    shellPath: bin.path,
    shellArgs: runArgs(continuePrior),
    iconPath: new vscode.ThemeIcon('check-all'),
  });
  terminal.show();
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
