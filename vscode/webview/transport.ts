// The half of the client that stays.
//
// Everything about talking to `acy serve`: the initial frame, the event stream,
// actions, and the stylesheet. render.ts is deliberately replaceable; this is
// not, so it holds no opinions about what a run looks like.
//
// Three things here are not arbitrary:
//
//   - EventSource is not an option. It cannot send an Authorization header, and
//     every /api/* route requires a bearer token, so the stream is a plain fetch
//     read through a streaming reader with an SSE parser of our own. That parser
//     therefore has to be correct about chunk boundaries, because a frame split
//     across two reads is the normal case, not an edge one.
//
//   - Frames are coalesced and never queued. The hub holds one frame per
//     subscriber and replaces an undelivered one, so by the time two have
//     arrived the older is already wrong. Rendering both would be work spent to
//     display a state that no longer exists.
//
//   - A closed socket means reconnect; an `event: done` means the run itself has
//     ended and there is nothing to reconnect to. Treating them the same is how
//     a client ends up hammering a supervisor that has quit.
//
// It is kept DOM-free at the top level on purpose — the integration test drives
// this module under plain `node --test`, against a real `acy serve`, which is
// the only way any of this gets exercised without a browser.

import type { Action, ActionResult, Frame, Theme } from './protocol';

/** Where a run is and what it takes to talk to it. */
export interface Endpoint {
  url: string;
  token: string;
}

/**
 * What the connection is doing. `ended` is terminal — the run is over — while
 * `reconnecting` is not.
 */
export type ConnectionState = 'connecting' | 'live' | 'reconnecting' | 'ended' | 'stopped';

export interface TransportHooks {
  /** The newest frame. Called at most once per scheduled turn, never in a batch. */
  onFrame(frame: Frame, rev: number): void;
  onState(state: ConnectionState, detail?: string): void;
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface TransportOptions {
  endpoint: Endpoint;
  hooks: TransportHooks;
  /** Injected for tests; defaults to the global fetch. */
  fetchImpl?: FetchLike;
  /** Injected for tests; defaults to requestAnimationFrame, or a macrotask. */
  schedule?: (cb: () => void) => void;
  /** Injected for tests; defaults to a cancellable setTimeout. */
  sleep?: (ms: number) => Promise<void>;
}

/** The first retry waits this long. */
export const BACKOFF_BASE_MS = 500;
/** And no retry ever waits longer than this. */
export const BACKOFF_CAP_MS = 15_000;

/**
 * How long to wait before retry number `attempt` (0-based).
 *
 * Exponential so a supervisor that is genuinely gone is not polled forever, and
 * capped so a supervisor that comes back — a laptop waking, a machine under
 * load — is noticed within a few seconds rather than after a doubling that has
 * run away into minutes.
 */
export function backoffDelay(attempt: number): number {
  const n = Math.max(0, Math.floor(attempt));
  // Clamped before the shift as well as after: 2 ** 2000 is Infinity, and
  // Math.min(cap, Infinity) is fine but Math.min(cap, NaN) would not be.
  return Math.min(BACKOFF_CAP_MS, BACKOFF_BASE_MS * 2 ** Math.min(n, 30));
}

/** One parsed server-sent event. */
export interface SseEvent {
  /** The `event:` field, or `message` when the server named none. */
  event: string;
  /** The `id:` field verbatim; the server sets it to the hub's rev. */
  id: string;
  /** The `data:` lines, joined with newlines. */
  data: string;
}

/**
 * An SSE parser over a byte stream.
 *
 * Bytes arrive in whatever sizes the socket felt like, so `push` returns only
 * the events that are *complete* and keeps the rest. Comment lines — the `:
 * ping` heartbeat an idle run relies on to prove the connection is alive — are
 * consumed and never surface as events, which is exactly what the protocol says
 * a comment is for.
 */
export class SseParser {
  private buf = '';
  private readonly decoder = new TextDecoder();
  private event = '';
  private id = '';
  private data: string[] = [];
  private sawData = false;

  push(chunk: Uint8Array | string): SseEvent[] {
    this.buf += typeof chunk === 'string' ? chunk : this.decoder.decode(chunk, { stream: true });

    const out: SseEvent[] = [];
    for (;;) {
      const nl = this.buf.indexOf('\n');
      if (nl < 0) {
        return out;
      }
      const line = this.buf.slice(0, nl).replace(/\r$/, '');
      this.buf = this.buf.slice(nl + 1);
      const done = this.line(line);
      if (done) {
        out.push(done);
      }
    }
  }

  /** Handles one complete line, returning an event when the line ended one. */
  private line(line: string): SseEvent | undefined {
    if (line === '') {
      // A blank line dispatches. With no data lines there is nothing to
      // dispatch — that is a comment block or a stray separator — and the
      // accumulated fields are dropped, per the spec.
      const ev = this.sawData
        ? { event: this.event || 'message', id: this.id, data: this.data.join('\n') }
        : undefined;
      this.event = '';
      this.data = [];
      this.sawData = false;
      return ev;
    }
    if (line.startsWith(':')) {
      return undefined; // a comment; the heartbeat lives here
    }
    const colon = line.indexOf(':');
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) {
      value = value.slice(1);
    }
    switch (field) {
      case 'event':
        this.event = value;
        break;
      case 'data':
        this.data.push(value);
        this.sawData = true;
        break;
      case 'id':
        // `id` persists across events until the server changes it, which is what
        // makes it a resumption point rather than a per-event label.
        this.id = value;
        break;
      default:
        // `retry:` and anything else this client has no use for.
        break;
    }
    return undefined;
  }
}

/** Thrown when a route answers with something other than a 2xx. */
export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = 'HttpError';
  }
}

export class Transport {
  private readonly endpoint: Endpoint;
  private readonly hooks: TransportHooks;
  private readonly fetchImpl: FetchLike;
  private readonly schedule: (cb: () => void) => void;
  private readonly sleep: ((ms: number) => Promise<void>) | undefined;

  private stopped = false;
  private runEnded = false;
  private abort: AbortController | undefined;
  private loopDone: Promise<void> | undefined;
  /** Cuts a backoff wait short, so stop() does not have to outwait it. */
  private wake: (() => void) | undefined;

  private rev = 0;
  private pending: { frame: Frame; rev: number } | undefined;
  private scheduled = false;

  constructor(opts: TransportOptions) {
    this.endpoint = opts.endpoint;
    this.hooks = opts.hooks;
    this.fetchImpl = opts.fetchImpl ?? ((input, init) => fetch(input, init));
    this.schedule = opts.schedule ?? defaultSchedule();
    this.sleep = opts.sleep;
  }

  /** The revision of the newest frame delivered so far; 0 before the first. */
  get revision(): number {
    return this.rev;
  }

  /**
   * Fetches the current frame, then opens the event stream and keeps it open.
   *
   * The initial GET is not redundant with the stream's priming frame: it gets
   * something on screen while the stream is still connecting, and a failure here
   * (a 503 from a hub that has not produced a frame yet) is not a reason not to
   * subscribe.
   */
  async start(): Promise<void> {
    this.stopped = false;
    this.runEnded = false;
    this.hooks.onState('connecting');
    try {
      const initial = await this.fetchFrame();
      this.deliver(initial.frame, initial.rev);
    } catch (err) {
      this.hooks.onState('connecting', describe(err));
    }
    this.loopDone = this.loop();
  }

  /** Stops the stream. The supervisor is unaffected — this is a client leaving. */
  async stop(): Promise<void> {
    this.stopped = true;
    this.abort?.abort();
    this.wake?.();
    this.hooks.onState('stopped');
    await this.loopDone;
  }

  /** GET /api/frame. */
  async fetchFrame(): Promise<{ frame: Frame; rev: number }> {
    const res = await this.request('/api/frame');
    const rev = Number(res.headers.get('X-Acy-Rev') ?? '0');
    return { frame: (await res.json()) as Frame, rev: Number.isFinite(rev) ? rev : 0 };
  }

  /**
   * POST /api/action.
   *
   * A refusal comes back as a 200 carrying `accepted: false` and a reason, and
   * is returned like any other answer: the run moves on its own, so "no gate is
   * pending for that toolUseId" is the system working, not a failed request.
   * Only a malformed request (400) or a missing token (401) throws.
   */
  async send(action: Action): Promise<ActionResult> {
    const res = await this.request('/api/action', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(action),
    });
    return (await res.json()) as ActionResult;
  }

  /** GET /api/highlight.css. The palette the transcript's class names refer to. */
  async highlightCss(theme: Theme): Promise<string> {
    const res = await this.request(`/api/highlight.css?theme=${theme}`);
    return res.text();
  }

  private async request(path: string, init: RequestInit = {}): Promise<Response> {
    const headers = new Headers(init.headers);
    headers.set('Authorization', `Bearer ${this.endpoint.token}`);
    const res = await this.fetchImpl(this.url(path), { ...init, headers });
    if (!res.ok) {
      throw new HttpError(res.status, `${path}: ${res.status} ${await errorText(res)}`);
    }
    return res;
  }

  private url(path: string): string {
    return this.endpoint.url.replace(/\/+$/, '') + path;
  }

  /** Reconnects until told to stop, or until the run says it has ended. */
  private async loop(): Promise<void> {
    let attempt = 0;
    while (!this.stopped && !this.runEnded) {
      try {
        await this.stream();
        // A clean end of stream with no `done` event is the socket closing — a
        // reload, a proxy, a sleeping laptop. Reconnect, from the top of the
        // backoff: this connection worked.
        attempt = 0;
        this.hooks.onState('reconnecting', 'the stream closed');
      } catch (err) {
        if (this.stopped) {
          return;
        }
        if (err instanceof HttpError && (err.status === 401 || err.status === 403)) {
          // Retrying will not mint a better token. Say so and stay quiet, rather
          // than knocking on the door every fifteen seconds forever.
          this.hooks.onState('stopped', err.message);
          return;
        }
        this.hooks.onState('reconnecting', describe(err));
      }
      if (this.stopped || this.runEnded) {
        return;
      }
      await this.delay(backoffDelay(attempt));
      attempt += 1;
    }
  }

  /** A backoff wait that stop() can cut short. */
  private delay(ms: number): Promise<void> {
    if (this.sleep) {
      return this.sleep(ms);
    }
    return new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, ms);
      this.wake = () => {
        clearTimeout(timer);
        resolve();
      };
    });
  }

  /** One connection's worth of stream, returning when it closes. */
  private async stream(): Promise<void> {
    this.abort = new AbortController();
    const res = await this.request('/api/events', {
      headers: { Accept: 'text/event-stream' },
      signal: this.abort.signal,
    });
    if (!res.body) {
      throw new Error('/api/events: the response carried no body to read');
    }
    this.hooks.onState('live');

    const reader = res.body.getReader();
    const parser = new SseParser();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          return;
        }
        if (!value) {
          continue;
        }
        for (const ev of parser.push(value)) {
          this.dispatch(ev);
        }
        if (this.runEnded) {
          return;
        }
      }
    } finally {
      // Releasing rather than cancelling: the abort signal is what closes the
      // socket, and cancel() on an already-aborted stream throws for nothing.
      try {
        reader.releaseLock();
      } catch {
        /* already released */
      }
    }
  }

  private dispatch(ev: SseEvent): void {
    if (ev.event === 'done') {
      this.runEnded = true;
      this.hooks.onState('ended', 'the run has ended');
      return;
    }
    if (ev.event !== 'frame') {
      return;
    }
    let frame: Frame;
    try {
      frame = JSON.parse(ev.data) as Frame;
    } catch (err) {
      this.hooks.onState('live', `unreadable frame: ${describe(err)}`);
      return;
    }
    const rev = Number(ev.id);
    this.deliver(frame, Number.isFinite(rev) ? rev : 0);
  }

  /**
   * Holds the newest frame and renders once.
   *
   * Never a queue: a frame that has been superseded describes a run state that
   * is already gone, and the hub itself drops undelivered frames for the same
   * reason. An older revision arriving late (a reconnect racing a stale read) is
   * dropped rather than rendered backwards.
   */
  private deliver(frame: Frame, rev: number): void {
    if (rev > 0 && rev < this.rev) {
      return;
    }
    this.pending = { frame, rev };
    if (this.scheduled) {
      return;
    }
    this.scheduled = true;
    this.schedule(() => {
      this.scheduled = false;
      const next = this.pending;
      this.pending = undefined;
      if (!next) {
        return;
      }
      this.rev = Math.max(this.rev, next.rev);
      this.hooks.onFrame(next.frame, next.rev);
    });
  }
}

/**
 * Injects the highlight stylesheet under the document's nonce.
 *
 * It has to be a nonce'd <style> rather than a <link>: the CSS is served by the
 * acy server, which the CSP allows the page to *connect* to but not to load
 * styles from, and it cannot be inline without 'unsafe-inline' — which is the
 * one thing this policy will not have. Replacing the element's text is how a
 * theme switch happens, with no entry re-rendered: fragments carry class names
 * and no colors precisely so that this is possible.
 */
export function injectHighlightCss(doc: Document, css: string, nonce: string): void {
  injectStyle(doc, 'acy-highlight', css, nonce);
}

/** One nonce'd <style> element per id, created once and replaced thereafter. */
export function injectStyle(doc: Document, id: string, css: string, nonce: string): void {
  let el = doc.getElementById(id) as HTMLStyleElement | null;
  if (!el) {
    el = doc.createElement('style');
    el.id = id;
    // Both, deliberately: the attribute is what the parser-facing policy reads,
    // and the property is what browsers keep after they hide the attribute from
    // script.
    el.setAttribute('nonce', nonce);
    el.nonce = nonce;
    doc.head.appendChild(el);
  }
  el.textContent = css;
}

/** A frame at a time, aligned to paint where there is one. */
function defaultSchedule(): (cb: () => void) => void {
  if (typeof requestAnimationFrame === 'function') {
    return (cb) => {
      requestAnimationFrame(() => cb());
    };
  }
  return (cb) => {
    setTimeout(cb, 0);
  };
}

/** The server's `{"error":"…"}` body, or the status text when there is none. */
async function errorText(res: Response): Promise<string> {
  try {
    const body: unknown = await res.json();
    const err = (body as { error?: unknown })?.error;
    if (typeof err === 'string' && err) {
      return err;
    }
  } catch {
    /* not JSON */
  }
  return res.statusText || 'no detail';
}

function describe(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
