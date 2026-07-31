// The wire types, transcribed from docs/webui-protocol.md.
//
// internal/ui/frame.go and internal/ui/action.go are the sources of truth; this
// is the client's reading of them. Nothing here derives anything — a field that
// is not in the Go struct does not belong in this file, and a client that wants
// a value the Frame does not carry should get it added there rather than
// computed twice.

export interface Hint {
  text: string;
  kind: 'gate' | 'working' | 'busy' | 'planReady' | 'plan' | 'complete' | 'default' | string;
}

export interface Tokens {
  input: number;
  output: number;
  cacheCreate: number;
  cacheRead: number;
}

export interface Ledger {
  parent: Tokens;
  child: Tokens;
  total: Tokens;
  context: number;
  contextWindow: number;
}

export interface Cost {
  parent: number;
  child: number;
  total: number;
}

export type EntryKind =
  | 'meta'
  | 'you'
  | 'claude'
  | 'thinking'
  | 'tool'
  | 'toolOK'
  | 'toolErr'
  | 'plan'
  | 'turn'
  | 'complete'
  | 'good'
  | 'warn'
  | 'queued';

export interface Entry {
  seq: number;
  kind: EntryKind | string;
  title: string;
  body: string;
  raw: string;
  lang: string;
  /**
   * The entry as a sanitized HTML fragment, rendered by internal/htmlrender.
   * The client renders no markdown and highlights no code: this is what it
   * inserts. Empty only for a run that was never asked for it, which `acy serve`
   * always is.
   */
  html: string;
  task: string;
}

export interface Gate {
  toolUseId: string;
  tool: string;
  task: string;
  args: string;
  /** When this auto-approves. Zero while the run is paused. */
  deadlineUnixMs: number;
  /** The frozen remainder. Zero unless the run is paused. */
  remainingMs: number;
}

export interface AskOption {
  label: string;
  description: string;
  selected: boolean;
}

export interface Ask {
  header: string;
  question: string;
  index: number;
  total: number;
  multiSelect: boolean;
  cursor: number;
  options: AskOption[];
  deadlineUnixMs: number;
}

export interface Task {
  id: string;
  title: string;
  outcome: string;
  cost: number;
  tokens: Tokens;
  running: boolean;
}

export interface SessionRow {
  id: string;
  modTimeUnixMs: number;
  summary: string;
  label: string;
  selected: boolean;
}

export interface Frame {
  phase: string;
  status: string;
  hint: Hint;
  sessionId: string;
  model: string;
  billing: string;
  ended: boolean;
  busy: boolean;
  processing: boolean;
  planReady: boolean;
  paused: boolean;
  showHelp: boolean;
  picking: boolean;
  cooldownUntilUnixMs: number;
  turnStartUnixMs: number;
  cost: Cost;
  tokens: Ledger;
  dispatches: number;
  entries: Entry[];
  queue: string[];
  gates: Gate[];
  ask: Ask | null;
  tasks: Task[];
  picker: SessionRow[];
  interruptedTasks: string[];
  logPath: string;
  configPath: string;
  cwd: string;
}

export type ActionKind =
  | 'submit'
  | 'arm'
  | 'interject'
  | 'gateAllow'
  | 'gateDeny'
  | 'gatePause'
  | 'askAnswer'
  | 'askSkip'
  | 'resume'
  | 'pickerClose'
  | 'setModel'
  | 'clear'
  | 'done'
  | 'queueClear'
  | 'quit';

/**
 * One flat shape with a kind, matching ui.Action: fields belonging to other
 * kinds are ignored by the server, never inspected.
 */
export interface Action {
  kind: ActionKind;
  text?: string;
  toolUseId?: string;
  paused?: boolean;
  questionIndex?: number;
  optionIndices?: number[];
  sessionId?: string;
  name?: string;
  summary?: string;
}

/**
 * Every action is answered exactly once, and `reason` is populated in both
 * cases — "sent" and "queued until the session falls idle" are different
 * outcomes of the same accepted submit.
 */
export interface ActionResult {
  accepted: boolean;
  reason: string;
}

export type Theme = 'dark' | 'light';

/** What the extension host embeds in the document. */
export interface Bootstrap {
  url: string;
  token: string;
  nonce: string;
  folder: string;
}
