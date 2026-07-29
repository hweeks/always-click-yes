import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildCsp, buildWebviewHtml, makeNonce, originOf } from '../html';

const bootstrap = {
  url: 'http://127.0.0.1:54321/',
  token: 'sekrit',
  nonce: 'deadbeef',
  folder: '/work/project',
};

function html(): string {
  return buildWebviewHtml({
    scriptUri: 'https://file%2B.vscode-resource.vscode-cdn.net/ext/webview/dist/webview.js',
    cspSource: 'https://file+.vscode-resource.vscode-cdn.net',
    bootstrap,
  });
}

test('a nonce is present on every script and on the policy', () => {
  const doc = html();
  assert.match(doc, /<script nonce="deadbeef">window\.__ACY__ = /);
  assert.match(doc, /<script nonce="deadbeef" src="https:\/\/file/);
  assert.match(doc, /script-src 'nonce-deadbeef'/);
});

test('connect-src is pinned to the server origin, not to http:', () => {
  const csp = buildCsp('vscode-resource:', originOf(bootstrap.url), 'n1');
  assert.match(csp, /connect-src http:\/\/127\.0\.0\.1:54321(;|$)/);
  // The origin only: the trailing slash the url carried is gone, and a bare
  // scheme — which would hand the panel every port on this machine — never
  // appears.
  assert.doesNotMatch(csp, /connect-src[^;]*54321\//);
  assert.doesNotMatch(csp, /connect-src[^;]*\bhttp:(;|\s|$)/);
  assert.equal(originOf('http://127.0.0.1:9/api/frame'), 'http://127.0.0.1:9');
});

test("the policy starts at default-src 'none' and never relaxes to unsafe-inline", () => {
  const doc = html();
  assert.match(doc, /content="default-src 'none';/);
  assert.doesNotMatch(doc, /unsafe-inline/);
  assert.doesNotMatch(doc, /unsafe-eval/);
});

test('the bootstrap cannot close the script element it lives in', () => {
  const doc = buildWebviewHtml({
    scriptUri: 'about:blank',
    cspSource: 'vscode-resource:',
    bootstrap: { ...bootstrap, token: '</script><script>alert(1)</script>' },
  });
  assert.doesNotMatch(doc, /<\/script><script>alert/);
  assert.match(doc, /\\u003c\/script/);
});

test('every nonce is different, and is a valid source expression', () => {
  const a = makeNonce();
  const b = makeNonce();
  assert.notEqual(a, b);
  assert.match(a, /^[0-9a-f]{32}$/);
});
