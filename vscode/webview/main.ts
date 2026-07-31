// The client's entry point: bootstrap in, transport and renderer wired together.
//
// Small on purpose. The transport (kept) knows how to talk to `acy serve`, the
// renderer (throwaway) knows what a run looks like, and this file is the only
// place that knows both — so replacing the rendering means replacing render.ts
// and the twenty lines below that name its hooks.

import type { Action, ActionResult, Bootstrap, Theme } from './protocol';
import { Renderer, themeOf } from './render';
import { injectHighlightCss, Transport } from './transport';

interface VsCodeApi {
  postMessage(message: unknown): void;
  setState(state: unknown): void;
  getState(): unknown;
}
declare function acquireVsCodeApi(): VsCodeApi;

const boot = (window as unknown as { __ACY__?: Bootstrap }).__ACY__;
const root = document.getElementById('acy-root');

if (boot && root) {
  start(boot, root);
}

function start(bootstrap: Bootstrap, root: HTMLElement): void {
  const vscode = acquireVsCodeApi();
  // What a WebviewPanelSerializer is handed after a window reload, so the
  // restored tab knows which folder it was supervising.
  vscode.setState({ folder: bootstrap.folder });

  const log = (text: string) => vscode.postMessage({ type: 'log', text });

  const transport = new Transport({
    endpoint: { url: bootstrap.url, token: bootstrap.token },
    hooks: {
      onFrame: (frame) => renderer.apply(frame),
      onState: (state, detail) => {
        renderer.setConnection(state, detail);
        if (detail) {
          log(`${state}: ${detail}`);
        }
      },
    },
  });

  const act = (action: Action) => {
    transport.send(action).then(
      (res: ActionResult) => renderer.setNotice(res.reason),
      (err: unknown) => {
        const text = err instanceof Error ? err.message : String(err);
        renderer.setNotice(text);
        log(`action ${action.kind} failed: ${text}`);
      },
    );
  };

  const renderer = new Renderer(
    root,
    {
      submit: (text) => act({ kind: 'submit', text }),
      arm: () => act({ kind: 'arm' }),
      interject: () => act({ kind: 'interject' }),
      pause: (paused) => act({ kind: 'gatePause', paused }),
      allow: (toolUseId) => act({ kind: 'gateAllow', toolUseId }),
      deny: (toolUseId) => act({ kind: 'gateDeny', toolUseId }),
      answerAsk: (questionIndex, optionIndices) =>
        act({ kind: 'askAnswer', questionIndex, optionIndices }),
      skipAsk: () => act({ kind: 'askSkip' }),
      clearQueue: () => act({ kind: 'queueClear' }),
      resume: (sessionId) => act({ kind: 'resume', sessionId }),
      closePicker: () => act({ kind: 'pickerClose' }),
      sessions: () => transport.sessions(),
    },
    bootstrap.nonce,
  );

  // The palette the transcript's class names refer to. Swapping this one
  // document is the whole of a theme change — no entry is re-rendered, because
  // no entry ever carried a color.
  let theme: Theme | undefined;
  const loadTheme = () => {
    const want = themeOf(document);
    if (want === theme) {
      return;
    }
    theme = want;
    transport.highlightCss(want).then(
      (css) => injectHighlightCss(document, css, bootstrap.nonce),
      (err: unknown) => log(`highlight.css: ${err instanceof Error ? err.message : String(err)}`),
    );
  };
  loadTheme();
  // VS Code signals a theme change by rewriting the class on <body>.
  new MutationObserver(loadTheme).observe(document.body, {
    attributes: true,
    attributeFilter: ['class'],
  });

  window.addEventListener('unload', () => {
    void transport.stop();
    renderer.dispose();
  });

  void transport.start();
}
