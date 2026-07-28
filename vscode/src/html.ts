// The webview document, built as a pure function.
//
// vscode-free like launch.ts and endpoint.ts, because the interesting part is
// the Content-Security-Policy and a policy is exactly the kind of thing that
// should be asserted in a test rather than eyeballed in a running editor. The
// caller supplies the two values only vscode knows — the asWebviewUri of the
// bundled script, and webview.cspSource — and everything else is arithmetic.

import { randomBytes } from 'crypto';

/** What the client needs to know before it can do anything. */
export interface Bootstrap {
  /** The acy server, as the *webview* can reach it (post asExternalUri). */
  url: string;
  /** The bearer token for every /api/* request. */
  token: string;
  /** The document nonce, so the client can inject a permitted <style>. */
  nonce: string;
  /** The workspace folder this panel supervises; restored on a window reload. */
  folder: string;
}

export interface WebviewHtmlOptions {
  /** The bundled client bundle, already run through webview.asWebviewUri. */
  scriptUri: string;
  /** webview.cspSource — the scheme the editor serves local resources from. */
  cspSource: string;
  /** Everything the client is handed at load. */
  bootstrap: Bootstrap;
}

/**
 * A fresh nonce per load. It is the whole of the script policy: `default-src
 * 'none'` plus a per-document nonce means the only code that can run is the
 * bundle we shipped, and the transcript — which is model output and raw tool
 * results, rendered to HTML by the server — cannot smuggle a script tag past it
 * even if bluemonday somehow let one through.
 */
export function makeNonce(): string {
  return randomBytes(16).toString('hex');
}

/**
 * The origin part of a URL, which is what a CSP connect-src wants: a scheme,
 * host and port, with no path. Throws on a URL that will not parse, which
 * parseEndpointLine has already refused.
 */
export function originOf(url: string): string {
  return new URL(url).origin;
}

/**
 * The CSP for one load.
 *
 * `default-src 'none'` and then exactly what is needed, which is deliberately
 * very little:
 *
 *   - script only from the nonce. No 'unsafe-inline', so a fragment carrying a
 *     handler attribute is inert markup rather than code.
 *   - style from the editor's own cspSource plus the nonce, because the
 *     highlight stylesheet arrives at runtime over fetch and is injected as a
 *     <style>. This is also why entry fragments carry class names and no colors
 *     — an inline style attribute would need 'unsafe-inline' and there is no
 *     version of this policy that includes it.
 *   - connect only to the one acy server this panel owns. A webview that can
 *     talk to a supervisor can approve tool calls, so the port is pinned rather
 *     than left as http:.
 */
export function buildCsp(cspSource: string, origin: string, nonce: string): string {
  return [
    "default-src 'none'",
    `img-src ${cspSource} data: https:`,
    `font-src ${cspSource}`,
    `style-src ${cspSource} 'nonce-${nonce}'`,
    `script-src 'nonce-${nonce}'`,
    `connect-src ${origin}`,
  ].join('; ');
}

/** The whole document. */
export function buildWebviewHtml(opts: WebviewHtmlOptions): string {
  const { nonce } = opts.bootstrap;
  const csp = buildCsp(opts.cspSource, originOf(opts.bootstrap.url), nonce);
  return `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <meta http-equiv="Content-Security-Policy" content="${escapeAttr(csp)}" />
    <title>acy</title>
  </head>
  <body>
    <div id="acy-root"></div>
    <script nonce="${nonce}">window.__ACY__ = ${embedJson(opts.bootstrap)};</script>
    <script nonce="${nonce}" src="${escapeAttr(opts.scriptUri)}"></script>
  </body>
</html>
`;
}

/**
 * The bootstrap object, embedded so it cannot end the script element it lives
 * in. `</script>` inside a JSON string is the classic way an inline blob
 * escapes; escaping every `<` costs nothing and closes it.
 */
function embedJson(v: unknown): string {
  return JSON.stringify(v).replace(/</g, '\\u003c');
}

function escapeAttr(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
}
