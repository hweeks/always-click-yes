// The one test that proves the plumbing.
//
// Everything else here is arithmetic on strings; this builds the real Go binary,
// starts a real `acy serve`, and drives it through the real transport.ts — the
// same module the webview loads — under plain node. It is the only evidence
// available without an interactive VS Code window, so it exercises the whole
// round trip rather than mocking any part of it: read the endpoint line, fetch a
// frame, open the event stream, POST an action, watch the resulting frame arrive
// over SSE.
//
// It skips, loudly, if the binary cannot be built. A machine without a Go
// toolchain should not fail this suite, but it must not silently pass it either.

import assert from 'node:assert/strict';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { after, test } from 'node:test';
import { parseEndpointLine, takeFirstLine, type Endpoint } from '../endpoint';
import type { Frame } from '../../webview/protocol';
import { Transport, type ConnectionState } from '../../webview/transport';

const repoRoot = path.resolve(__dirname, '..', '..', '..', '..');
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'acy-panel-'));
const binPath = path.join(scratch, os.platform() === 'win32' ? 'acy.exe' : 'acy');

const cleanup: Array<() => void> = [];
after(() => {
  for (const fn of cleanup.reverse()) {
    try {
      fn();
    } catch {
      /* best effort */
    }
  }
  fs.rmSync(scratch, { recursive: true, force: true });
});

/** Builds acy, or explains why the test cannot run. */
function buildAcy(): string | undefined {
  if (os.platform() === 'win32') {
    return 'the claude stub this test spawns is a POSIX shell script';
  }
  const go = spawnSync('go', ['build', '-o', binPath, '.'], {
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: 300_000,
  });
  if (go.error) {
    return `could not run \`go build\` in ${repoRoot}: ${go.error.message}`;
  }
  if (go.status !== 0) {
    return `\`go build\` failed (exit ${go.status}): ${(go.stderr || go.stdout || '').trim()}`;
  }
  return undefined;
}

/**
 * A stand-in for the claude CLI.
 *
 * acy launches claude as soon as its model initialises, and this test has no
 * business starting a real session — it would cost money and talk to a live
 * account to prove something about an HTTP transport. The stub holds stdin open
 * and says nothing, which is exactly the shape of a session that has not
 * answered yet: the supervisor is up, the frame is real, and no turn is in
 * flight.
 */
function writeClaudeStub(): string {
  const stub = path.join(scratch, 'claude-stub');
  fs.writeFileSync(stub, '#!/bin/sh\nexec cat > /dev/null\n', { mode: 0o755 });
  return stub;
}

/** Spawns `acy serve` and reads its endpoint line with the extension's own parser. */
function startServe(cwd: string, claudeBin: string): Promise<{ child: ChildProcess; endpoint: Endpoint }> {
  return new Promise((resolve, reject) => {
    const child = spawn(
      binPath,
      ['serve', '--port', '0', '--log', '', '--claude-bin', claudeBin],
      {
        cwd,
        // A scratch state dir, so a test run cannot land a snapshot in the
        // user's real ~/.config/acy and turn up in their /resume picker.
        env: { ...process.env, ACY_STATE_DIR: path.join(scratch, 'state') },
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    );
    cleanup.push(() => child.kill('SIGKILL'));

    let stdout = '';
    let stderr = '';
    const timer = setTimeout(() => {
      reject(new Error(`acy serve printed no endpoint in 30s; stderr:\n${stderr}`));
    }, 30_000);

    child.stderr?.setEncoding('utf8');
    child.stderr?.on('data', (c: string) => {
      stderr += c;
    });
    child.on('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.on('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`acy serve exited (${code}) before printing an endpoint; stderr:\n${stderr}`));
    });
    child.stdout?.setEncoding('utf8');
    child.stdout?.on('data', (chunk: string) => {
      stdout += chunk;
      const first = takeFirstLine(stdout);
      if (!first) {
        return;
      }
      clearTimeout(timer);
      const parsed = parseEndpointLine(first.line);
      if (!parsed.ok) {
        reject(new Error(parsed.reason));
        return;
      }
      resolve({ child, endpoint: parsed.endpoint });
    });
  });
}

async function waitFor<T>(what: string, probe: () => T | undefined, ms = 20_000): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const hit = probe();
    if (hit !== undefined) {
      return hit;
    }
    if (Date.now() > deadline) {
      throw new Error(`timed out waiting for ${what}`);
    }
    await new Promise((r) => setTimeout(r, 25));
  }
}

test('transport.ts drives a real acy serve end to end', async (t) => {
  const why = buildAcy();
  if (why) {
    t.skip(`skipping the acy serve integration test: ${why}`);
    return;
  }

  const project = fs.mkdtempSync(path.join(scratch, 'project-'));
  const { child, endpoint } = await startServe(project, writeClaudeStub());

  // The endpoint line is the whole contract with whatever launched acy.
  assert.match(endpoint.url, /^http:\/\/127\.0\.0\.1:\d+$/);
  assert.ok(endpoint.token.length >= 32, 'the token should be 256 bits of hex');

  const frames: Array<{ frame: Frame; rev: number }> = [];
  const states: ConnectionState[] = [];
  const transport = new Transport({
    endpoint,
    hooks: {
      onFrame: (frame, rev) => frames.push({ frame, rev }),
      onState: (state) => states.push(state),
    },
  });
  cleanup.push(() => void transport.stop());

  // 1. The initial GET /api/frame, then the stream. A 503 here is "the model
  //    has not produced a frame yet" and start() is allowed to swallow it — the
  //    stream primes every new subscriber with the current frame, so a frame
  //    arrives by one route or the other.
  await transport.start();
  const first = await waitFor('the first frame', () => frames[0]);
  assert.equal(path.basename(first.frame.cwd), path.basename(project));
  assert.equal(first.frame.phase, 'PLAN');
  assert.ok(Array.isArray(first.frame.entries));

  // 2. The event stream is open.
  await waitFor('the event stream to go live', () => (states.includes('live') ? true : undefined));

  // 3. An action the model accepts, and the frame it produces, over SSE.
  const before = transport.revision;
  const paused = await transport.send({ kind: 'gatePause', paused: true });
  assert.deepEqual(paused, { accepted: true, reason: 'countdowns paused' });

  const streamed = await waitFor(
    'a streamed frame showing the pause',
    () => frames.find((f) => f.rev > before && f.frame.paused),
  );
  assert.ok(streamed.rev > before, 'the hub rev should advance for a distinct frame');

  // 4. An action the model refuses. A refusal is a domain answer — the run moves
  //    on its own — so it comes back 200 with a reason, not as a thrown error.
  const refused = await transport.send({ kind: 'gateAllow', toolUseId: 'toolu_nope' });
  assert.equal(refused.accepted, false);
  assert.match(refused.reason, /no gate is pending/);

  // 5. The stylesheet the transcript's class names refer to.
  const css = await transport.highlightCss('dark');
  assert.match(css, /\.chroma/);
  assert.notEqual(await transport.highlightCss('light'), css);

  // 6. A bad token is refused, and the transport surfaces it rather than
  //    silently rendering nothing.
  const impostor = new Transport({
    endpoint: { url: endpoint.url, token: 'not-the-token' },
    hooks: { onFrame: () => undefined, onState: () => undefined },
  });
  await assert.rejects(() => impostor.fetchFrame(), /401/);

  await transport.stop();
  child.kill('SIGTERM');
});
