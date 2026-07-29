// Owning an `acy serve` process.
//
// The terminal launcher runs acy *as* a shell and lets the terminal own its
// lifetime. A webview has no such thing, so this file is the missing owner: it
// spawns the binary, reads the one endpoint line off stdout, forwards stderr to
// the `acy` output channel, and keeps exactly one supervisor per workspace
// folder alive — two supervisors on one project being this project's classic
// footgun, the same one the terminal launcher's reveal-don't-relaunch rule
// exists for.
//
// The decisions that do not need vscode (what to invoke, how to read the line)
// live in endpoint.ts and are tested there.

import * as cp from 'child_process';
import * as vscode from 'vscode';
import { parseEndpointLine, takeFirstLine, type Endpoint } from './endpoint';

/**
 * How long to wait for the endpoint line. `serve` prints it as soon as the
 * listener binds, so this is not a slow-machine allowance — it is the bound on
 * a process that started and then said nothing at all, which without a timeout
 * is a spinner that never stops.
 */
const ENDPOINT_TIMEOUT_MS = 30_000;

/** How much stderr to keep for a failure message. Enough for a Go panic's head. */
const STDERR_TAIL_BYTES = 4096;

/** Everything needed to start one supervisor. */
export interface ServeSpec {
  /** The acy binary, as resolveBinary found it. */
  binPath: string;
  /** The project directory to supervise. */
  cwd: string;
  /** serveArgs(...) — the invocation only; settings live in .acy.json. */
  args: string[];
  /**
   * Environment for the child. Used for the same reason the terminal launcher
   * uses it: a `claude` found off PATH is unreachable to acy unless its
   * directory travels along, and acy is spawned with no shell to find it.
   */
  env?: NodeJS.ProcessEnv;
}

/** A running supervisor, and the endpoint its webview talks to. */
export interface ServeSession extends vscode.Disposable {
  readonly endpoint: Endpoint;
  /** Fires once, with a human-readable reason, if the process dies on its own. */
  readonly onDidExit: vscode.Event<string>;
  /** Whether the process is still up. */
  readonly alive: boolean;
}

class Session implements ServeSession {
  private readonly exited = new vscode.EventEmitter<string>();
  readonly onDidExit = this.exited.event;
  private done = false;

  constructor(
    readonly endpoint: Endpoint,
    private readonly child: cp.ChildProcess,
    private readonly output: vscode.OutputChannel,
  ) {
    child.on('exit', (code, signal) => {
      this.done = true;
      const how = signal ? `signal ${signal}` : `exit code ${code ?? 0}`;
      this.output.appendLine(`[acy serve] exited (${how})`);
      this.exited.fire(`the acy supervisor stopped (${how})`);
    });
  }

  get alive(): boolean {
    return !this.done;
  }

  dispose(): void {
    this.exited.dispose();
    if (this.done) {
      return;
    }
    this.done = true;
    // SIGTERM, not SIGKILL: serve installs its own handler and uses it to stop
    // the claude processes it launched and to unlink the gate socket. Killing it
    // outright would leave both behind.
    this.child.kill('SIGTERM');
  }
}

/**
 * One supervisor per workspace folder, keyed by its path.
 *
 * `start` is idempotent by key while the process is alive, including while it is
 * still starting: the in-flight promise is what is cached, so two rapid
 * `acy.openPanel` invocations share one supervisor rather than racing to spawn
 * a second.
 */
export class ServeManager implements vscode.Disposable {
  private readonly starting = new Map<string, Promise<ServeSession>>();
  private readonly live = new Map<string, ServeSession>();

  constructor(private readonly output: vscode.OutputChannel) {}

  /** The running supervisor for a key, if there is one. */
  get(key: string): ServeSession | undefined {
    const s = this.live.get(key);
    return s?.alive ? s : undefined;
  }

  /** Starts a supervisor for this key, or hands back the one already running. */
  start(key: string, spec: ServeSpec): Promise<ServeSession> {
    const running = this.get(key);
    if (running) {
      return Promise.resolve(running);
    }
    const pending = this.starting.get(key);
    if (pending) {
      return pending;
    }

    const attempt = spawnServe(spec, this.output)
      .then((session) => {
        this.starting.delete(key);
        this.live.set(key, session);
        session.onDidExit(() => {
          if (this.live.get(key) === session) {
            this.live.delete(key);
          }
        });
        return session;
      })
      .catch((err: unknown) => {
        this.starting.delete(key);
        throw err;
      });
    this.starting.set(key, attempt);
    return attempt;
  }

  /** Stops the supervisor for this key, if any. */
  stop(key: string): void {
    this.live.get(key)?.dispose();
    this.live.delete(key);
  }

  dispose(): void {
    for (const key of [...this.live.keys()]) {
      this.stop(key);
    }
    this.starting.clear();
  }
}

/**
 * Spawns one `acy serve` and resolves when it has printed a usable endpoint.
 *
 * Every failure below has to arrive as a sentence someone can act on, because
 * the alternative is a webview that opens blank: the binary could not be
 * executed, it exited before saying anything, it said something that was not the
 * endpoint line, or it said nothing at all for long enough that waiting is
 * indistinguishable from hanging.
 */
function spawnServe(spec: ServeSpec, output: vscode.OutputChannel): Promise<ServeSession> {
  return new Promise<ServeSession>((resolve, reject) => {
    output.appendLine(`[acy serve] ${spec.binPath} ${spec.args.join(' ')} (cwd ${spec.cwd})`);

    let child: cp.ChildProcess;
    try {
      child = cp.spawn(spec.binPath, spec.args, {
        cwd: spec.cwd,
        env: spec.env ?? process.env,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch (err) {
      reject(new Error(`could not start ${spec.binPath}: ${message(err)}`));
      return;
    }

    let settled = false;
    let stdoutBuf = '';
    let stderrTail = '';

    const finish = (fn: () => void) => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      fn();
    };

    const fail = (reason: string) => {
      finish(() => {
        child.kill('SIGTERM');
        reject(new Error(stderrTail ? `${reason}\n\nstderr:\n${stderrTail.trim()}` : reason));
      });
    };

    const timer = setTimeout(() => {
      fail(
        `acy serve did not print its endpoint within ${ENDPOINT_TIMEOUT_MS / 1000}s. ` +
          'Check the "acy" output channel, or run `acy serve` in a terminal to see what it is waiting on.',
      );
    }, ENDPOINT_TIMEOUT_MS);

    child.on('error', (err) => {
      fail(`could not start ${spec.binPath}: ${message(err)}`);
    });

    child.on('exit', (code, signal) => {
      const how = signal ? `signal ${signal}` : `exit code ${code ?? 0}`;
      fail(`acy serve exited (${how}) before printing its endpoint.`);
    });

    // Both pipes are drained unconditionally. A child whose stderr nobody reads
    // blocks the moment the pipe buffer fills, and a supervisor frozen mid-run
    // for want of a reader is a far worse bug than a noisy output channel.
    child.stderr?.setEncoding('utf8');
    child.stderr?.on('data', (chunk: string) => {
      output.append(chunk);
      stderrTail = (stderrTail + chunk).slice(-STDERR_TAIL_BYTES);
    });

    child.stdout?.setEncoding('utf8');
    child.stdout?.on('data', (chunk: string) => {
      if (settled) {
        // serve promises nothing else on stdout, so anything here is a surprise
        // worth having in the log rather than dropping on the floor.
        output.append(chunk);
        return;
      }
      stdoutBuf += chunk;
      const first = takeFirstLine(stdoutBuf);
      if (!first) {
        return;
      }
      stdoutBuf = first.rest;
      const parsed = parseEndpointLine(first.line);
      if (!parsed.ok) {
        fail(parsed.reason);
        return;
      }
      finish(() => {
        output.appendLine(`[acy serve] listening on ${parsed.endpoint.url}`);
        resolve(new Session(parsed.endpoint, child, output));
      });
    });
  });
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
