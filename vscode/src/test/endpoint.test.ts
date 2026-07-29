import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseEndpointLine, serveArgs, takeFirstLine } from '../endpoint';

test('serveArgs carries only the invocation, never settings', () => {
  assert.deepEqual(serveArgs(false), ['serve']);
  assert.deepEqual(serveArgs(true), ['serve', '--continue']);
});

test('a well-formed endpoint line parses', () => {
  const r = parseEndpointLine('{"url":"http://127.0.0.1:54321","token":"8f3c"}');
  assert.deepEqual(r, { ok: true, endpoint: { url: 'http://127.0.0.1:54321', token: '8f3c' } });
});

test('malformed JSON is refused, with the line in the message', () => {
  const r = parseEndpointLine('{"url":"http://127.0.0.1:1", "token"');
  assert.equal(r.ok, false);
  assert.match(r.ok === false ? r.reason : '', /was not JSON/);
});

test('a missing field is refused, and named', () => {
  const noToken = parseEndpointLine('{"url":"http://127.0.0.1:1"}');
  assert.equal(noToken.ok, false);
  assert.match(noToken.ok === false ? noToken.reason : '', /no "token"/);

  const noUrl = parseEndpointLine('{"token":"abc"}');
  assert.equal(noUrl.ok, false);
  assert.match(noUrl.ok === false ? noUrl.reason : '', /no "url"/);

  // Present but empty is the same as absent: an empty url is not a place.
  const blank = parseEndpointLine('{"url":"","token":"abc"}');
  assert.equal(blank.ok, false);
});

test('output that is not the endpoint line is refused rather than searched', () => {
  // A wrapper script printing a banner, an old build, a shim on the wrong
  // stream: `serve` promises this is the first line, so anything else means we
  // are not talking to what we think we are.
  const first = takeFirstLine('acy v1.2.3\n{"url":"http://127.0.0.1:1","token":"t"}\n');
  assert.ok(first);
  assert.equal(first.line, 'acy v1.2.3');
  const r = parseEndpointLine(first.line);
  assert.equal(r.ok, false);
  assert.match(r.ok === false ? r.reason : '', /was not JSON: acy v1\.2\.3/);
});

test('a JSON value that is not an object is refused', () => {
  assert.equal(parseEndpointLine('["http://127.0.0.1:1"]').ok, false);
  assert.equal(parseEndpointLine('"nope"').ok, false);
  assert.equal(parseEndpointLine('null').ok, false);
});

test('an unusable url is refused where it can still be explained', () => {
  const r = parseEndpointLine('{"url":"127.0.0.1:54321","token":"t"}');
  assert.equal(r.ok, false);
  assert.match(r.ok === false ? r.reason : '', /unusable url/);
});

test('empty output is refused, and says what was expected', () => {
  const r = parseEndpointLine('');
  assert.equal(r.ok, false);
  assert.match(r.ok === false ? r.reason : '', /empty/);
  assert.equal(parseEndpointLine('   ').ok, false);
});

test('takeFirstLine waits for a whole line and strips a CR', () => {
  assert.equal(takeFirstLine('{"url":'), undefined);
  assert.deepEqual(takeFirstLine('a\r\nb'), { line: 'a', rest: 'b' });
  assert.deepEqual(takeFirstLine('a\n'), { line: 'a', rest: '' });
});

test('a line reassembled from chunks parses like a whole one', () => {
  const line = '{"url":"http://127.0.0.1:9","token":"tok"}';
  let buf = '';
  let taken: ReturnType<typeof takeFirstLine>;
  for (const chunk of [line.slice(0, 7), line.slice(7, 20), `${line.slice(20)}\n`]) {
    buf += chunk;
    taken = takeFirstLine(buf);
  }
  assert.ok(taken);
  assert.deepEqual(parseEndpointLine(taken.line), {
    ok: true,
    endpoint: { url: 'http://127.0.0.1:9', token: 'tok' },
  });
});
