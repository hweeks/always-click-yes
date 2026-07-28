// Pure logic for owning an `acy serve` process: what to invoke it with, and how
// to read the one line it prints back. Kept vscode-free like launch.ts and
// claude.ts so it runs under plain `node --test` — the whole contract with a
// headless supervisor is a line of JSON, and parsing a line of JSON does not
// need an extension host.

/**
 * The endpoint line `acy serve` writes to stdout, exactly once, as soon as its
 * listener is up. Documented in docs/webui-protocol.md; the field names are
 * protocol, not an implementation detail.
 */
export interface Endpoint {
  /** Where the server is listening, e.g. `http://127.0.0.1:54321`. */
  url: string;
  /** The bearer token every `/api/*` request must carry. */
  token: string;
}

/**
 * The outcome of reading that line. A discriminated union rather than a throw
 * because every failure here has to reach the user as a sentence — a webview
 * that never opens is otherwise indistinguishable from one that opened blank.
 */
export type EndpointParse =
  | { ok: true; endpoint: Endpoint }
  | { ok: false; reason: string };

/**
 * Arguments for a headless supervisor. Like runArgs, it carries only the
 * invocation: run settings travel in .acy.json so the CLI and the extension
 * cannot disagree about them. The port is left at its default 0 — the kernel
 * picks one and the endpoint line says which.
 */
export function serveArgs(continuePrior: boolean): string[] {
  return continuePrior ? ['serve', '--continue'] : ['serve'];
}

/**
 * Splits the first complete line off a stdout buffer, or undefined while the
 * line is still arriving. A pipe hands over whatever bytes are ready, so the
 * endpoint line routinely arrives in pieces — and the caller must not parse a
 * half-written one and declare the JSON malformed.
 */
export function takeFirstLine(buf: string): { line: string; rest: string } | undefined {
  const nl = buf.indexOf('\n');
  if (nl < 0) {
    return undefined;
  }
  return { line: buf.slice(0, nl).replace(/\r$/, ''), rest: buf.slice(nl + 1) };
}

/**
 * Reads the endpoint line, strictly.
 *
 * Strict on purpose: `acy serve` promises that its *first* line of stdout is
 * this JSON and that nothing else is ever written there, so anything else means
 * we are not talking to the binary we think we are — an old build, a wrapper
 * script that logs a banner, a shim printing to the wrong stream. Guessing
 * around that would produce a webview pointed at nothing, with the real cause
 * two layers back.
 */
export function parseEndpointLine(line: string): EndpointParse {
  const trimmed = line.trim();
  if (!trimmed) {
    return {
      ok: false,
      reason: "acy serve's first line of output was empty; it should be the JSON endpoint line.",
    };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return {
      ok: false,
      reason: `acy serve's first line of output was not JSON: ${preview(trimmed)}`,
    };
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return {
      ok: false,
      reason: `acy serve's endpoint line was not a JSON object: ${preview(trimmed)}`,
    };
  }

  const { url, token } = parsed as { url?: unknown; token?: unknown };
  if (typeof url !== 'string' || url === '') {
    return { ok: false, reason: `acy serve's endpoint line carried no "url": ${preview(trimmed)}` };
  }
  if (typeof token !== 'string' || token === '') {
    return {
      ok: false,
      reason: `acy serve's endpoint line carried no "token": ${preview(trimmed)}`,
    };
  }
  try {
    // Parsed rather than pattern-matched: everything downstream (the CSP's
    // connect-src, the fetches) derives an origin from this, and a value that
    // will not parse fails there instead, far from the cause.
    new URL(url);
  } catch {
    return { ok: false, reason: `acy serve reported an unusable url: ${preview(url)}` };
  }

  return { ok: true, endpoint: { url, token } };
}

/** A one-line excerpt for an error message, so a stray megabyte cannot become one. */
function preview(s: string): string {
  const oneLine = s.split('\n', 1)[0];
  return oneLine.length > 200 ? `${oneLine.slice(0, 200)}…` : oneLine;
}
