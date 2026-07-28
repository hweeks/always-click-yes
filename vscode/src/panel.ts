// The webview panel: one per workspace folder, over one `acy serve`.
//
// Everything here is plumbing. The panel owns a supervisor's lifetime (open it
// and it starts, close it and it stops), hands the client a URL and a token, and
// gets out of the way — the transcript is rendered by the server and the state
// is fetched by the client. What the panel is *not* is a second implementation
// of the UI, which is why nothing in this file knows what a gate is.
//
// The document itself is built in html.ts, as a pure function, because its CSP
// is the interesting part and a policy belongs in a test.

import * as vscode from 'vscode';
import { buildWebviewHtml, makeNonce } from './html';
import type { ServeManager, ServeSpec } from './serve';

/** The viewType a serializer reattaches to after a window reload. */
export const PANEL_VIEW_TYPE = 'acyPanel';

/** Where the built client lives inside the extension, and the only root the webview may load from. */
export const WEBVIEW_DIST = ['webview', 'dist'];

/** What the client stores with setState, so a reload knows what it was attached to. */
interface PanelState {
  folder?: string;
}

export interface PanelDeps {
  extensionUri: vscode.Uri;
  servers: ServeManager;
  output: vscode.OutputChannel;
  /**
   * Resolves what to spawn for a folder — the acy binary, the environment a
   * discovered `claude` needs — or undefined when it has already told the user
   * why it cannot.
   */
  spec(folder: vscode.WorkspaceFolder, continuePrior: boolean): Promise<ServeSpec | undefined>;
}

export class PanelHost implements vscode.Disposable {
  private readonly panels = new Map<string, vscode.WebviewPanel>();

  constructor(private readonly deps: PanelDeps) {}

  /**
   * Opens the panel for a folder, or reveals the one already open.
   *
   * Reveal rather than reopen for the same reason the terminal launcher reveals:
   * a second panel would want a second supervisor on the same project, and two
   * supervisors on one project is the footgun this extension has always been
   * careful about.
   */
  async open(folder: vscode.WorkspaceFolder, continuePrior: boolean): Promise<void> {
    const key = folder.uri.fsPath;
    const existing = this.panels.get(key);
    if (existing) {
      existing.reveal(existing.viewColumn ?? vscode.ViewColumn.Active);
      return;
    }

    const panel = vscode.window.createWebviewPanel(
      PANEL_VIEW_TYPE,
      'acy',
      vscode.ViewColumn.Active,
      this.panelOptions(),
    );
    await this.attach(panel, folder, continuePrior);
  }

  /**
   * Reattaches a panel VS Code restored across a window reload.
   *
   * The supervisor did not survive the reload — it was this window's child — so
   * this starts a fresh one for the same folder. That is honest about what
   * happened: the alternative is a restored tab showing a transcript nothing is
   * behind, which looks like a live run right up until you press a button.
   */
  async restore(panel: vscode.WebviewPanel, state: unknown): Promise<void> {
    panel.webview.options = this.panelOptions();
    const folder = this.folderFromState(state);
    if (!folder) {
      panel.webview.html = errorHtml(
        'This acy panel was attached to a folder that is no longer open. Close it and run "ACY: Open Panel" again.',
      );
      return;
    }
    const existing = this.panels.get(folder.uri.fsPath);
    if (existing && existing !== panel) {
      // Two tabs for one folder: keep the live one, and say so in the corpse
      // rather than silently starting a second supervisor behind it.
      panel.webview.html = errorHtml('acy is already open for this folder in another tab.');
      return;
    }
    await this.attach(panel, folder, false);
  }

  dispose(): void {
    for (const panel of [...this.panels.values()]) {
      panel.dispose();
    }
    this.panels.clear();
  }

  /** Starts (or reuses) the folder's supervisor and points the panel at it. */
  private async attach(
    panel: vscode.WebviewPanel,
    folder: vscode.WorkspaceFolder,
    continuePrior: boolean,
  ): Promise<void> {
    const key = folder.uri.fsPath;
    this.panels.set(key, panel);

    // Starting a supervisor takes a moment, and a tab can be closed inside it.
    // Everything below therefore checks this before touching the panel: writing
    // html to a disposed webview throws, and — worse — a supervisor whose panel
    // went away while it was starting would keep running with nothing to stop
    // it.
    let closed = false;
    panel.onDidDispose(() => {
      closed = true;
      if (this.panels.get(key) === panel) {
        this.panels.delete(key);
      }
      // The panel was the only reason this supervisor existed.
      this.deps.servers.stop(key);
    });
    panel.webview.onDidReceiveMessage((msg: unknown) => this.onClientMessage(msg));

    const spec = await this.deps.spec(folder, continuePrior);
    if (closed) {
      return;
    }
    if (!spec) {
      panel.webview.html = errorHtml(
        'acy could not be started for this folder — see the message that just appeared, or the "acy" output channel.',
      );
      return;
    }

    try {
      const session = await vscode.window.withProgress(
        { location: vscode.ProgressLocation.Window, title: 'Starting acy…' },
        () => this.deps.servers.start(key, spec),
      );
      if (closed) {
        this.deps.servers.stop(key);
        return;
      }
      session.onDidExit((reason) => {
        void vscode.window.showErrorMessage(`acy: ${reason}. See the "acy" output channel.`);
      });

      // Through asExternalUri, so a Remote-SSH or Codespaces window forwards the
      // port instead of handing the webview a 127.0.0.1 that is somebody else's
      // machine. It is also the origin the CSP is pinned to, so the two cannot
      // disagree.
      const external = await vscode.env.asExternalUri(vscode.Uri.parse(session.endpoint.url));
      if (closed) {
        return;
      }

      panel.webview.html = buildWebviewHtml({
        scriptUri: panel.webview
          .asWebviewUri(vscode.Uri.joinPath(this.deps.extensionUri, ...WEBVIEW_DIST, 'webview.js'))
          .toString(),
        cspSource: panel.webview.cspSource,
        bootstrap: {
          url: external.toString(),
          token: session.endpoint.token,
          nonce: makeNonce(),
          folder: key,
        },
      });
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      this.deps.output.appendLine(`[acy] panel: ${reason}`);
      if (!closed) {
        panel.webview.html = errorHtml(reason);
      }
      void vscode.window.showErrorMessage(`acy: ${reason}`);
    }
  }

  /** The client's only channel back to the host, and it is a log line. */
  private onClientMessage(msg: unknown): void {
    const m = msg as { type?: unknown; text?: unknown };
    if (m?.type === 'log' && typeof m.text === 'string') {
      this.deps.output.appendLine(`[acy webview] ${m.text}`);
    }
  }

  private panelOptions(): vscode.WebviewOptions & vscode.WebviewPanelOptions {
    return {
      enableScripts: true,
      // The run keeps going while the tab is in the background; throwing the
      // client's DOM away and rebuilding it on reveal would drop the event
      // stream with it.
      retainContextWhenHidden: true,
      enableFindWidget: true,
      // The bundle and nothing else. The transcript is fetched over HTTP, not
      // loaded as a local resource, so there is nothing else to grant.
      localResourceRoots: [vscode.Uri.joinPath(this.deps.extensionUri, ...WEBVIEW_DIST)],
    };
  }

  private folderFromState(state: unknown): vscode.WorkspaceFolder | undefined {
    const folders = vscode.workspace.workspaceFolders ?? [];
    const want = (state as PanelState | undefined)?.folder;
    if (typeof want === 'string') {
      const hit = folders.find((f) => f.uri.fsPath === want);
      if (hit) {
        return hit;
      }
    }
    return folders.length === 1 ? folders[0] : undefined;
  }
}

/** The serializer VS Code hands a restored tab. */
export function panelSerializer(host: PanelHost): vscode.WebviewPanelSerializer {
  return {
    deserializeWebviewPanel: (panel, state) => host.restore(panel, state),
  };
}

/** A document for a panel that has nothing to show. No script, so no policy to write. */
function errorHtml(reason: string): string {
  const text = reason.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  return `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta http-equiv="Content-Security-Policy" content="default-src 'none';" />
    <title>acy</title>
  </head>
  <body><p>${text}</p></body>
</html>
`;
}
