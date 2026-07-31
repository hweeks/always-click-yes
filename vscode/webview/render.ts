// The half of the client that goes.
//
// This is a deliberately plain rendering of a Frame: enough header, transcript,
// gate panel, queue panel and composer to drive a real run, and no design at
// all. A later task replaces this file wholesale against a reference mock, so
// nothing here is worth polishing and nothing outside it should depend on the
// shape of what it builds — the seam is `apply(frame)` in and RenderHooks out.
//
// The one rule that is not throwaway: **the client renders no markdown and
// highlights no code.** Every entry arrives as a sanitized HTML fragment in
// `entry.html`, produced by internal/htmlrender, and is inserted as-is. Two
// implementations of one transcript is exactly what Frame exists to prevent.

import type { Ask, Frame, Gate, SessionRow, Theme } from './protocol';
import type { ConnectionState } from './transport';
import { injectStyle } from './transport';

/** What the UI can ask of a run. Every one of these is an ui.Action. */
export interface RenderHooks {
  submit(text: string): void;
  arm(): void;
  interject(): void;
  pause(paused: boolean): void;
  allow(toolUseId: string): void;
  deny(toolUseId: string): void;
  answerAsk(questionIndex: number, optionIndices: number[]): void;
  skipAsk(): void;
  clearQueue(): void;
  resume(sessionId: string): void;
  closePicker(): void;
  /**
   * The resume list. A hook rather than a transport call so that this file goes
   * on knowing nothing about HTTP; main.ts is the only place that knows both.
   */
  sessions(): Promise<SessionRow[]>;
}

export class Renderer {
  private readonly els: Elements;
  private frame: Frame | undefined;
  private connection: ConnectionState = 'connecting';
  private detail = '';
  private notice = '';
  private renderedSeqs: number[] = [];
  private gateIds = '';
  private sessionIds = '';
  /** Rows this client fetched itself, as opposed to rows off a frame's picker. */
  private fetchedSessions: readonly SessionRow[] | undefined;
  /** Whether the person asked for that list; a frame's picker outranks it. */
  private sessionsOpen = false;
  private askId = '';
  /** The question the rows on screen belong to — never "whatever is current". */
  private askShown: Ask | null = null;
  private askBoxes: HTMLInputElement[] = [];
  private readonly ticker: ReturnType<typeof setInterval>;

  constructor(
    private readonly root: HTMLElement,
    private readonly hooks: RenderHooks,
    nonce: string,
  ) {
    injectStyle(root.ownerDocument, 'acy-base', BASE_CSS, nonce);
    this.els = build(root.ownerDocument, hooks);
    this.els.askSubmit.addEventListener('click', () => this.submitAsk());
    // Both of these depend on which of the two lists is live, which only the
    // renderer knows, so neither can be wired at build time.
    this.els.openSessions.addEventListener('click', () => this.loadSessions());
    this.els.sessionsDismiss.addEventListener('click', () => this.dismissSessions());
    root.replaceChildren(this.els.shell);
    // Countdowns animate from the client's own clock against an absolute
    // deadline — Frame carries no `now`, on purpose, so that an idle run's
    // frames are byte-identical and the server stays silent.
    this.ticker = setInterval(() => {
      this.paintGates();
      this.paintAsk();
    }, 200);
  }

  dispose(): void {
    clearInterval(this.ticker);
  }

  /** A new frame. Always the newest one; the transport never queues them. */
  apply(frame: Frame): void {
    this.frame = frame;
    this.paintHeader();
    this.paintTranscript(frame);
    this.paintAsk();
    this.paintGates();
    this.paintQueue(frame);
    this.paintSessions();
    this.paintComposer(frame);
  }

  setConnection(state: ConnectionState, detail?: string): void {
    this.connection = state;
    this.detail = detail ?? '';
    this.paintHeader();
  }

  /** The reason string off an ActionResult — accepted or refused, it is the answer. */
  setNotice(text: string): void {
    this.notice = text;
    this.els.notice.textContent = text;
  }

  private paintHeader(): void {
    const f = this.frame;
    const bits = [
      f ? f.phase : '…',
      f ? f.status : 'connecting',
      f?.model ? f.model : '',
      f?.sessionId ? `session ${f.sessionId.slice(0, 8)}` : '',
      f ? `$${f.cost.total.toFixed(4)}` : '',
      f && f.tokens.context ? `ctx ${f.tokens.context.toLocaleString()}` : '',
    ].filter(Boolean);
    this.els.header.textContent = bits.join('  ·  ');
    this.els.connection.textContent = this.detail
      ? `${this.connection} — ${this.detail}`
      : this.connection;
    this.els.connection.className = `acy-conn acy-conn--${this.connection}`;
    this.els.hint.textContent = f ? f.hint.text : '';
    this.els.notice.textContent = this.notice;
  }

  /**
   * Appends what is new and rebuilds only when it has to.
   *
   * `seq` is an identity that is never reused, so "the first N are the same" is
   * a sound test for an append — and /clear, which empties the transcript
   * without rewinding the counter, is what makes the rebuild path necessary.
   */
  private paintTranscript(frame: Frame): void {
    const seqs = frame.entries.map((e) => e.seq);
    const appendOnly =
      seqs.length >= this.renderedSeqs.length &&
      this.renderedSeqs.every((s, i) => seqs[i] === s);

    const doc = this.root.ownerDocument;
    const atBottom = nearBottom(this.els.transcript);
    if (!appendOnly) {
      this.els.transcript.replaceChildren();
      this.renderedSeqs = [];
    }
    for (const entry of frame.entries.slice(this.renderedSeqs.length)) {
      const wrap = doc.createElement('div');
      wrap.className = 'acy-entry-slot';
      if (entry.html) {
        // Safe by construction: goldmark without WithUnsafe, then bluemonday.
        // See docs/webui-protocol.md — the fragment is inert on purpose.
        wrap.innerHTML = entry.html;
      } else {
        // A run that was not asked for HTML. `acy serve` always asks, so this is
        // a fallback rather than a path — and text, never markup.
        const pre = doc.createElement('pre');
        pre.textContent = entry.body;
        wrap.appendChild(pre);
      }
      this.els.transcript.appendChild(wrap);
    }
    this.renderedSeqs = seqs;
    if (atBottom) {
      this.els.transcript.scrollTop = this.els.transcript.scrollHeight;
    }
  }

  /** Runs on every frame and on the 200ms tick, because only the clock moved. */
  private paintGates(): void {
    const gates = this.frame?.gates ?? [];
    const paused = this.frame?.paused ?? false;
    this.els.gates.hidden = gates.length === 0;
    this.els.pause.textContent = paused ? 'Resume countdowns' : 'Pause countdowns';
    this.els.pause.dataset.paused = String(paused);

    const doc = this.root.ownerDocument;
    // Rebuilt when the *identities* change, not the count: a gate that
    // auto-approved while another arrived leaves the length alone and every row
    // wrong.
    const ids = gates.map((g) => g.toolUseId).join('\0');
    if (this.gateIds !== ids) {
      this.gateIds = ids;
      this.els.gateList.replaceChildren(...gates.map((g) => gateRow(doc, g, this.hooks)));
    }
    gates.forEach((g, i) => {
      const row = this.els.gateList.children[i];
      const label = row?.querySelector('.acy-gate-when');
      if (label) {
        label.textContent = countdown(g, paused);
      }
    });
  }

  /**
   * The blocked question. Like paintGates, this runs on every frame and on the
   * tick — but unlike a gate list, the rows here hold state a person is part way
   * through entering, so they are rebuilt only when the *question* changes and
   * never merely because a frame arrived.
   */
  private paintAsk(): void {
    const ask = this.frame?.ask ?? null;
    this.els.ask.hidden = ask === null;
    if (!ask) {
      this.askId = '';
      this.askShown = null;
      this.askBoxes = [];
      return;
    }
    const id = askIdentity(ask);
    if (this.askId !== id) {
      this.askId = id;
      this.buildAsk(ask);
    }
    this.els.askWhen.textContent = askCountdown(ask);
  }

  /** The rows for one question, built once and then left alone. */
  private buildAsk(ask: Ask): void {
    const doc = this.root.ownerDocument;
    this.askShown = ask;
    this.els.askHead.textContent =
      ask.total > 1 ? `Question ${ask.index + 1} of ${ask.total} · ${ask.header}` : ask.header;
    this.els.askQuestion.textContent = ask.question;

    this.askBoxes = [];
    const rows = ask.options.map((opt, i) => {
      const row = el(doc, 'div', 'acy-ask-option');
      const text = el(doc, 'div', 'acy-ask-option-text');
      const label = el(doc, 'div', 'acy-ask-label');
      label.textContent = opt.label;
      text.appendChild(label);
      if (opt.description) {
        const desc = el(doc, 'div', 'acy-ask-desc');
        desc.textContent = opt.description;
        text.appendChild(desc);
      }
      if (ask.multiSelect) {
        const box = doc.createElement('input');
        box.type = 'checkbox';
        box.className = 'acy-ask-check';
        box.checked = opt.selected;
        box.addEventListener('change', () => this.syncAskSubmit());
        this.askBoxes.push(box);
        row.append(box, text);
      } else {
        // One click answers, naming the question by index: whatever replaced it
        // is a different question and the server says so.
        row.append(
          button(doc, 'Choose', () => this.hooks.answerAsk(ask.index, askSelection(ask, [i]))),
          text,
        );
      }
      return row;
    });
    this.els.askOptions.replaceChildren(...rows);

    this.els.askSubmit.hidden = !ask.multiSelect;
    if (ask.multiSelect) {
      this.syncAskSubmit();
    }
  }

  /** What is ticked right now, normalised into what may be sent. */
  private askTicked(): number[] {
    const ask = this.askShown;
    if (!ask) {
      return [];
    }
    const ticked: number[] = [];
    this.askBoxes.forEach((box, i) => {
      if (box.checked) {
        ticked.push(i);
      }
    });
    return askSelection(ask, ticked);
  }

  // An empty answer is refused by the server with "no option chosen", so the
  // button is closed off rather than the refusal coming back as a notice.
  private syncAskSubmit(): void {
    this.els.askSubmit.disabled = this.askTicked().length === 0;
  }

  private submitAsk(): void {
    const ask = this.askShown;
    const chosen = this.askTicked();
    if (!ask || chosen.length === 0) {
      return;
    }
    this.hooks.answerAsk(ask.index, chosen);
  }

  private paintQueue(frame: Frame): void {
    this.els.queue.hidden = frame.queue.length === 0;
    this.els.queueList.replaceChildren(
      ...frame.queue.map((text) => {
        const li = this.root.ownerDocument.createElement('li');
        li.textContent = text;
        return li;
      }),
    );
  }

  /**
   * The resume list, from whichever of its two sources is live.
   *
   * Rows are rebuilt only when the *identities* change, exactly as paintGates
   * does and for the same reason: this runs on every frame, and a rebuild throws
   * away the scroll position and the focus of someone reading the list. `selected`
   * moves as the terminal's cursor moves, so it is repainted in place instead.
   */
  private paintSessions(): void {
    const { rows, source } = visibleSessions(this.frame, this.fetchedSessions, this.sessionsOpen);
    this.els.sessions.hidden = source === 'none';

    const ids = rows.map((r) => r.id).join('\0');
    if (this.sessionIds !== ids) {
      this.sessionIds = ids;
      const doc = this.root.ownerDocument;
      this.els.sessionList.replaceChildren(...rows.map((r) => sessionRow(doc, r, this.hooks)));
    }
    rows.forEach((r, i) => {
      this.els.sessionList.children[i]?.classList.toggle('acy-session--selected', r.selected);
    });
  }

  /** The local path: ask for the list, then show it. */
  private loadSessions(): void {
    this.hooks.sessions().then(
      (rows) => {
        this.fetchedSessions = rows;
        this.sessionsOpen = true;
        this.paintSessions();
      },
      (err: unknown) => {
        // Treated like a refused action: the reason is shown, not swallowed, and
        // the panel is left closed rather than holding whatever it held before.
        this.fetchedSessions = undefined;
        this.sessionsOpen = false;
        this.paintSessions();
        this.setNotice(err instanceof Error ? err.message : String(err));
      },
    );
  }

  /**
   * Dismiss means two different things, and the source decides which. A frame's
   * picker is the model's modal and has to be closed on the server; a list this
   * client fetched exists only here, so hiding it is the whole of closing it.
   */
  private dismissSessions(): void {
    const { source } = visibleSessions(this.frame, this.fetchedSessions, this.sessionsOpen);
    if (source === 'frame') {
      this.hooks.closePicker();
      return;
    }
    this.fetchedSessions = undefined;
    this.sessionsOpen = false;
    this.paintSessions();
  }

  private paintComposer(frame: Frame): void {
    this.els.arm.disabled = frame.phase !== 'PLAN' || !frame.sessionId;
    this.els.interject.disabled = !frame.processing || frame.gates.length > 0;
    this.els.send.disabled = frame.ended && !this.els.input.value.startsWith('/');
    this.els.clearQueue.disabled = frame.queue.length === 0;
  }
}

interface Elements {
  shell: HTMLElement;
  header: HTMLElement;
  connection: HTMLElement;
  hint: HTMLElement;
  notice: HTMLElement;
  transcript: HTMLElement;
  ask: HTMLElement;
  askHead: HTMLElement;
  askQuestion: HTMLElement;
  askOptions: HTMLElement;
  askWhen: HTMLElement;
  askSubmit: HTMLButtonElement;
  gates: HTMLElement;
  gateList: HTMLElement;
  pause: HTMLButtonElement;
  queue: HTMLElement;
  queueList: HTMLElement;
  clearQueue: HTMLButtonElement;
  sessions: HTMLElement;
  sessionList: HTMLElement;
  sessionsDismiss: HTMLButtonElement;
  openSessions: HTMLButtonElement;
  input: HTMLTextAreaElement;
  send: HTMLButtonElement;
  arm: HTMLButtonElement;
  interject: HTMLButtonElement;
}

function build(doc: Document, hooks: RenderHooks): Elements {
  const shell = el(doc, 'div', 'acy-shell');

  const headerBar = el(doc, 'header', 'acy-header');
  const header = el(doc, 'div', 'acy-title');
  const connection = el(doc, 'div', 'acy-conn');
  headerBar.append(header, connection);

  const transcript = el(doc, 'div', 'acy-transcript');

  // Above the gates: a question blocks claude's turn outright, so it is the
  // most urgent thing on the screen. Every word in it comes off the frame.
  const ask = el(doc, 'section', 'acy-panel acy-ask');
  const askHead = el(doc, 'div', 'acy-panel-head');
  const askQuestion = el(doc, 'div', 'acy-ask-question');
  const askOptions = el(doc, 'div', 'acy-ask-options');
  const askWhen = el(doc, 'div', 'acy-ask-when');
  // Wired by the renderer itself, which knows which question the rows on screen
  // belong to; the skip has nothing to name.
  const askSubmit = button(doc, 'Submit');
  const askSkip = button(doc, 'Skip', () => hooks.skipAsk());
  const askButtons = el(doc, 'div', 'acy-buttons');
  askButtons.append(askSubmit, askSkip);
  ask.append(askHead, askQuestion, askOptions, askWhen, askButtons);
  ask.hidden = true;

  const gates = el(doc, 'section', 'acy-panel acy-gates');
  const gatesHead = el(doc, 'div', 'acy-panel-head');
  gatesHead.textContent = 'Permission requests';
  // The button states the state it wants, read off the last frame, rather than
  // toggling: a client that toggles can start the countdowns someone just froze
  // from the terminal in the moment between the frame it saw and its request.
  const pause = button(doc, 'Pause countdowns', () => {
    hooks.pause(pause.dataset.paused !== 'true');
  });
  const gateList = el(doc, 'div', 'acy-gate-list');
  gates.append(gatesHead, gateList, pause);
  gates.hidden = true;

  const queue = el(doc, 'section', 'acy-panel acy-queue');
  const queueHead = el(doc, 'div', 'acy-panel-head');
  queueHead.textContent = 'Queued messages';
  const queueList = el(doc, 'ul', 'acy-queue-list');
  const clearQueue = button(doc, 'Drop queued', () => hooks.clearQueue());
  queue.append(queueHead, queueList, clearQueue);
  queue.hidden = true;

  const sessions = el(doc, 'section', 'acy-panel acy-sessions');
  const sessionsHead = el(doc, 'div', 'acy-panel-head');
  sessionsHead.textContent = 'Sessions';
  const sessionList = el(doc, 'div', 'acy-session-list');
  const sessionsDismiss = button(doc, 'Close');
  sessions.append(sessionsHead, sessionList, sessionsDismiss);
  sessions.hidden = true;

  const hint = el(doc, 'div', 'acy-hint');
  const notice = el(doc, 'div', 'acy-notice');

  const composer = el(doc, 'div', 'acy-composer');
  const input = doc.createElement('textarea');
  input.className = 'acy-input';
  input.rows = 3;
  input.placeholder = 'Message the supervising session…  (Enter sends, Shift+Enter for a newline)';
  const send = button(doc, 'Send', () => {
    const text = input.value;
    if (!text.trim()) {
      return;
    }
    input.value = '';
    hooks.submit(text);
  });
  const arm = button(doc, 'Arm (start delegating)', () => hooks.arm());
  const interject = button(doc, 'Interject', () => hooks.interject());
  const openSessions = button(doc, 'Resume session');
  input.addEventListener('keydown', (ev: KeyboardEvent) => {
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.altKey && !ev.ctrlKey && !ev.metaKey) {
      ev.preventDefault();
      send.click();
    }
  });
  const buttons = el(doc, 'div', 'acy-buttons');
  buttons.append(send, arm, interject, openSessions);
  composer.append(input, buttons);

  shell.append(headerBar, transcript, ask, gates, queue, sessions, hint, notice, composer);
  return {
    shell,
    header,
    connection,
    hint,
    notice,
    transcript,
    ask,
    askHead,
    askQuestion,
    askOptions,
    askWhen,
    askSubmit,
    gates,
    gateList,
    pause,
    queue,
    queueList,
    clearQueue,
    sessions,
    sessionList,
    sessionsDismiss,
    openSessions,
    input,
    send,
    arm,
    interject,
  };
}

/**
 * One gate row. Allow and deny name the gate by `toolUseId` and never by
 * position: gates auto-approve on their own countdown, so the row under the
 * pointer may not be the row that was there when the click lands.
 */
function gateRow(doc: Document, gate: Gate, hooks: RenderHooks): HTMLElement {
  const row = el(doc, 'div', 'acy-gate');
  const what = el(doc, 'div', 'acy-gate-what');
  what.textContent = gate.task ? `${gate.tool}  (task ${gate.task})` : gate.tool;
  const args = el(doc, 'div', 'acy-gate-args');
  args.textContent = gate.args;
  const when = el(doc, 'div', 'acy-gate-when');
  const actions = el(doc, 'div', 'acy-buttons');
  actions.append(
    button(doc, 'Allow', () => hooks.allow(gate.toolUseId)),
    button(doc, 'Deny', () => hooks.deny(gate.toolUseId)),
  );
  row.append(what, args, when, actions);
  return row;
}

/** Which session rows are on screen, with the model's modal taking precedence. */
function visibleSessions(
  frame: Frame | undefined,
  fetched: readonly SessionRow[] | undefined,
  fetchedOpen: boolean,
): { rows: readonly SessionRow[]; source: 'frame' | 'fetched' | 'none' } {
  if (frame?.picking) {
    return { rows: frame.picker, source: 'frame' };
  }
  if (fetchedOpen) {
    return { rows: fetched ?? [], source: 'fetched' };
  }
  return { rows: [], source: 'none' };
}

/** One resumable session. All product content is supplied by SessionRow. */
function sessionRow(doc: Document, row: SessionRow, hooks: RenderHooks): HTMLElement {
  const item = el(doc, 'div', 'acy-session');
  const title = el(doc, 'div', 'acy-session-title');
  title.textContent = row.label || row.id;
  const detail = el(doc, 'div', 'acy-session-detail');
  const when = new Date(row.modTimeUnixMs).toLocaleString();
  detail.textContent = row.summary ? `${when} · ${row.summary}` : when;
  item.append(button(doc, 'Resume', () => hooks.resume(row.id)), title, detail);
  return item;
}

/**
 * What makes this a *different* question, and so what makes the rows worth
 * rebuilding. Deliberately not the whole Ask: `cursor` moves as someone drives
 * the terminal picker, and rebuilding on that would throw away the checkboxes a
 * panel user is half way through ticking.
 */
function askIdentity(ask: Ask): string {
  return [
    String(ask.index),
    String(ask.total),
    String(ask.multiSelect),
    ask.header,
    ask.question,
    ask.options.map((o) => o.label).join(''),
  ].join('\0');
}

/**
 * Empty at a zero deadline, which is the PLAN phase: a person is already
 * watching, so the question waits rather than expiring under them.
 */
function askCountdown(ask: Ask): string {
  if (!ask.deadlineUnixMs) {
    return '';
  }
  const left = Math.max(0, ask.deadlineUnixMs - Date.now());
  return `auto-skips in ${(left / 1000).toFixed(1)}s`;
}

/**
 * A raw set of chosen option indices, normalised into what may travel as
 * `optionIndices`: in range, no duplicates, ascending — and cut to one for a
 * single-select question, which `answerAsk` refuses more than one option for.
 */
export function askSelection(ask: Ask, ticked: readonly number[]): number[] {
  const seen = new Set<number>();
  for (const i of ticked) {
    if (Number.isInteger(i) && i >= 0 && i < ask.options.length) {
      seen.add(i);
    }
  }
  const chosen = [...seen].sort((a, b) => a - b);
  return ask.multiSelect ? chosen : chosen.slice(0, 1);
}

function countdown(gate: Gate, paused: boolean): string {
  if (paused) {
    return `paused with ${(gate.remainingMs / 1000).toFixed(1)}s left`;
  }
  if (!gate.deadlineUnixMs) {
    return '';
  }
  const left = Math.max(0, gate.deadlineUnixMs - Date.now());
  return `auto-approves in ${(left / 1000).toFixed(1)}s`;
}

function nearBottom(node: HTMLElement): boolean {
  return node.scrollHeight - node.scrollTop - node.clientHeight < 80;
}

function el(doc: Document, tag: string, className: string): HTMLElement {
  const node = doc.createElement(tag);
  node.className = className;
  return node;
}

function button(doc: Document, label: string, onClick?: () => void): HTMLButtonElement {
  const b = doc.createElement('button');
  b.type = 'button';
  b.textContent = label;
  if (onClick) {
    b.addEventListener('click', onClick);
  }
  return b;
}

/** Which stylesheet to ask the server for. */
export function themeOf(doc: Document): Theme {
  return doc.body.classList.contains('vscode-light') ? 'light' : 'dark';
}

// Unstyled-but-usable, and every color a --vscode-* variable so the panel is at
// least not visually foreign while it waits for a real design.
const BASE_CSS = `
.acy-shell { display: flex; flex-direction: column; gap: 8px; height: 100vh; box-sizing: border-box; padding: 8px;
  font-family: var(--vscode-font-family); font-size: var(--vscode-font-size);
  color: var(--vscode-foreground); background: var(--vscode-editor-background); }
.acy-header { display: flex; justify-content: space-between; gap: 12px; align-items: baseline; }
.acy-title { font-weight: 600; }
.acy-conn { font-size: 0.85em; color: var(--vscode-descriptionForeground); }
.acy-conn--reconnecting, .acy-conn--stopped { color: var(--vscode-errorForeground); }
.acy-transcript { flex: 1 1 auto; overflow-y: auto; border: 1px solid var(--vscode-panel-border);
  padding: 8px; background: var(--vscode-editor-background); }
.acy-entry-slot { margin-bottom: 10px; }
.acy-entry__title { font-weight: 600; color: var(--vscode-descriptionForeground); }
.acy-entry pre, .acy-entry-slot pre { white-space: pre-wrap; word-break: break-word; margin: 4px 0;
  font-family: var(--vscode-editor-font-family); font-size: var(--vscode-editor-font-size);
  background: var(--vscode-textCodeBlock-background); padding: 6px; overflow-x: auto; }
.acy-entry--you { color: var(--vscode-textLink-foreground); }
.acy-entry--warn, .acy-entry--toolErr { color: var(--vscode-errorForeground); }
.acy-entry--meta, .acy-entry--turn, .acy-entry--thinking { color: var(--vscode-descriptionForeground); }
.acy-panel { border: 1px solid var(--vscode-panel-border); padding: 8px; }
.acy-panel-head { font-weight: 600; margin-bottom: 4px; }
.acy-ask { border-color: var(--vscode-focusBorder, var(--vscode-panel-border)); }
.acy-ask-question { margin-bottom: 6px; white-space: pre-wrap; }
.acy-ask-options { display: flex; flex-direction: column; }
.acy-ask-option { display: flex; align-items: flex-start; gap: 8px;
  border-top: 1px solid var(--vscode-panel-border); padding: 6px 0; }
.acy-ask-option-text { flex: 1 1 auto; min-width: 0; }
.acy-ask-label { font-weight: 600; }
.acy-ask-desc { color: var(--vscode-descriptionForeground); white-space: pre-wrap; }
.acy-ask-check { margin-top: 3px; accent-color: var(--vscode-button-background); }
.acy-ask-when { color: var(--vscode-descriptionForeground); min-height: 1.2em; margin: 4px 0; }
.acy-gate { border-top: 1px solid var(--vscode-panel-border); padding: 6px 0; }
.acy-gate-args { font-family: var(--vscode-editor-font-family); white-space: pre-wrap; word-break: break-all; }
.acy-gate-when { color: var(--vscode-descriptionForeground); }
.acy-queue-list { margin: 0; padding-left: 18px; }
.acy-session { display: flex; align-items: flex-start; gap: 8px; border-top: 1px solid var(--vscode-panel-border); padding: 6px 0; }
.acy-session--selected { outline: 1px solid var(--vscode-focusBorder); }
.acy-session-title { flex: 1 1 auto; font-weight: 600; }
.acy-session-detail { color: var(--vscode-descriptionForeground); }
.acy-hint, .acy-notice { font-size: 0.9em; color: var(--vscode-descriptionForeground); min-height: 1.2em; }
.acy-composer { display: flex; flex-direction: column; gap: 6px; }
.acy-input { width: 100%; box-sizing: border-box; resize: vertical;
  font-family: var(--vscode-font-family); font-size: var(--vscode-font-size);
  color: var(--vscode-input-foreground); background: var(--vscode-input-background);
  border: 1px solid var(--vscode-input-border, var(--vscode-panel-border)); padding: 6px; }
.acy-buttons { display: flex; gap: 6px; flex-wrap: wrap; }
button { font-family: inherit; font-size: inherit; padding: 4px 10px; cursor: pointer;
  color: var(--vscode-button-foreground); background: var(--vscode-button-background); border: none; }
button:hover:enabled { background: var(--vscode-button-hoverBackground); }
button:disabled { opacity: 0.5; cursor: default; }
`;
