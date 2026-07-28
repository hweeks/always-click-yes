import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  backoffDelay,
  BACKOFF_BASE_MS,
  BACKOFF_CAP_MS,
  SseParser,
} from '../../webview/transport';

test('backoff doubles from the base and stops at the cap', () => {
  assert.equal(backoffDelay(0), BACKOFF_BASE_MS);
  assert.equal(backoffDelay(1), 1000);
  assert.equal(backoffDelay(2), 2000);
  assert.equal(backoffDelay(3), 4000);
  assert.equal(backoffDelay(4), 8000);
  assert.equal(backoffDelay(5), BACKOFF_CAP_MS);
  assert.equal(backoffDelay(50), BACKOFF_CAP_MS);
  // A supervisor that comes back has to be noticed in seconds, not after a
  // doubling that ran away into minutes.
  assert.ok(BACKOFF_CAP_MS <= 30_000);
});

test('backoff never returns a negative or a NaN wait', () => {
  assert.equal(backoffDelay(-1), BACKOFF_BASE_MS);
  assert.equal(backoffDelay(1.9), 1000); // floored
  assert.equal(backoffDelay(1e9), BACKOFF_CAP_MS);
});

const enc = new TextEncoder();

test('a whole event parses out of one chunk', () => {
  const p = new SseParser();
  const got = p.push(enc.encode('id: 7\nevent: frame\ndata: {"phase":"PLAN"}\n\n'));
  assert.deepEqual(got, [{ event: 'frame', id: '7', data: '{"phase":"PLAN"}' }]);
});

test('a frame split across chunks is held until it is complete', () => {
  const p = new SseParser();
  const whole = 'id: 12\nevent: frame\ndata: {"phase":"AUTO-RUN","entries":[]}\n\n';
  // Split mid-field, mid-JSON and just before the blank line that dispatches.
  const cuts = [4, 19, 40, whole.length - 1];
  const chunks: string[] = [];
  let prev = 0;
  for (const cut of cuts) {
    chunks.push(whole.slice(prev, cut));
    prev = cut;
  }
  chunks.push(whole.slice(prev));

  const seen: unknown[] = [];
  chunks.forEach((chunk, i) => {
    const out = p.push(enc.encode(chunk));
    if (i < chunks.length - 1) {
      assert.deepEqual(out, [], `chunk ${i} dispatched early`);
    }
    seen.push(...out);
  });
  assert.deepEqual(seen, [
    { event: 'frame', id: '12', data: '{"phase":"AUTO-RUN","entries":[]}' },
  ]);
});

test('a multi-byte character split across chunks survives', () => {
  const p = new SseParser();
  const bytes = enc.encode('event: frame\ndata: {"status":"working…"}\n\n');
  const cut = 30; // lands inside the three bytes of "…"
  assert.deepEqual(p.push(bytes.slice(0, cut)), []);
  assert.deepEqual(p.push(bytes.slice(cut)), [
    { event: 'frame', id: '', data: '{"status":"working…"}' },
  ]);
});

test('heartbeats are consumed and never surface as events', () => {
  const p = new SseParser();
  assert.deepEqual(p.push(': ping\n\n'), []);
  assert.deepEqual(p.push(': ping\n\n: ping\n\n'), []);
  // …and a frame arriving after them is still delivered.
  assert.deepEqual(p.push('event: frame\ndata: {}\n\n'), [
    { event: 'frame', id: '', data: '{}' },
  ]);
});

test('several events in one chunk come back in order', () => {
  const p = new SseParser();
  const got = p.push(
    'id: 1\nevent: frame\ndata: {"a":1}\n\n: ping\n\nid: 2\nevent: frame\ndata: {"a":2}\n\nevent: done\ndata: {"reason":"the run has ended"}\n\n',
  );
  assert.deepEqual(got, [
    { event: 'frame', id: '1', data: '{"a":1}' },
    { event: 'frame', id: '2', data: '{"a":2}' },
    // id persists until the server changes it, per the SSE spec.
    { event: 'done', id: '2', data: '{"reason":"the run has ended"}' },
  ]);
});

test('CRLF framing and multi-line data are handled', () => {
  const p = new SseParser();
  assert.deepEqual(p.push('event: frame\r\ndata: one\r\ndata: two\r\n\r\n'), [
    { event: 'frame', id: '', data: 'one\ntwo' },
  ]);
});

test('an unnamed event defaults to message, and a field with no space is not eaten', () => {
  const p = new SseParser();
  assert.deepEqual(p.push('data:{"a":1}\n\n'), [{ event: 'message', id: '', data: '{"a":1}' }]);
});
