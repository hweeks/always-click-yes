# AGENTS.md — context for AI agents working on always-click-yes

Read this first. It captures what took real probing to learn so you don't have to
rediscover it.

## Use the tool to build the tool

Work on this repo **through `acy` itself**. Plan the change interactively, `Ctrl+G` to arm
it, and let the countdown approve its own way to the finish:

```sh
make run                          # from the repo root (= go build -o acy . && ./acy run)
```

This is not ceremony. It is the only honest test the project has. `acy` exists to be
trusted unsupervised, and a change that is comfortable to ship only because a human sat
there approving each step has not actually been tested — it has been chaperoned. Dogfooding
is also the fastest way these bugs surface: the gate deadlocks, the phantom keystrokes, the
judge that grades its own homework, the API key that silently bills the wrong account were
all found by running `acy` on `acy` and watching it misbehave in real time.

So: if you are an agent working here, you are both the author and the subject. Build the
change with the tool, and when the tool does something stupid to you, that is the bug —
fix it rather than working around it by hand.

## What this is

A Go TUI (Cobra + Bubble Tea v2 + Lipgloss v2) that supervises a **Claude Code** session
for long-running tasks. It is built around one measurement.

### The measurement everything follows from

One real run of acy on this repo, read out of its own `acy-debug.log`:

| | tokens |
|---|---|
| cache_read | **8,697,690** |
| cache_creation | 454,900 |
| output | 75,011 |
| input (uncached) | 188 |
| **cost** | **$16.04** |

98% of the token volume was **re-reading a context that only ever grew**. The model
was not writing too much; a single session was carrying every file it had ever read
into every turn that followed. That is what the architecture below exists to stop.

### Flow

1. **PLAN** — you chat with the supervising session over a `Read, Grep, Glob` registry.
   It can understand the codebase and it cannot change it.
2. **Ctrl+G arms it** — this *flips a phase on the session already running*. It does
   not launch anything. (It used to `--resume` a second process; that meant a second
   system prompt and a second cache warm-up for no gain.)
3. **AUTO-RUN** — the session delegates. `mcp__acy__Dispatch` hands one task to a
   **fresh `claude` child process** with the full toolset, and blocks until it reports.
   The child works, returns a `--json-schema`-validated report, and exits — taking its
   entire context with it. The parent's conversation grows by one ~300-token report.
4. **Done** — the session calls `mcp__acy__Finish`. There is **no completion loop and
   no auto-nudge**: a run that goes quiet is a question for a human, not a reason to
   spend another full-context turn asking "are you done yet?".

### Why the parent cannot write

Not a permission setting. `Write`, `Edit`, `Bash` and `Task` are **not in its
`--tools` registry at all**, which is a guarantee no prompt can talk its way past.
It is also why the system prompt no longer says "do not implement" — there is
nothing to implement with. The prompts shrank ~350 words → ~110 (parent) and
~180 → ~85 (child) on exactly this principle: state what is not discoverable,
and let the interface enforce the rest.

## Architecture (package map)

- `internal/driver` — owns the long-lived `claude` subprocess. Builds stream-json args,
  injects user messages on stdin, decodes the NDJSON event stream (`events.go`), and can
  `Interrupt()`. Logs raw RX/TX to `alog`.
- `internal/gate` — the permission bridge. `server.go` listens on a unix socket; the `hook`
  subcommand (`client.go`) connects, forwards the tool request, and blocks for an
  allow/deny `Decision`. `Pending.Resolve` answers it.
- `internal/config` — generates the `--settings` JSON that registers the PreToolUse hook
  pointing at `<self> hook --socket <path>` (the binary is its own hook). Also loads the
  project's `.acy.json` (`file.go`): strict parsing (unknown keys and bare-number durations
  are errors), precedence defaults < file < explicit flags, decided per flag via pflag's
  `Changed` (`applyFileConfig` in `cli/run.go`).
- `internal/orchestrator` — owns the disposable children. `Task`/`Child`/`Spawn`,
  a queue (`limit = 1`), the ledger, and the report schema. A leaf package: it
  imports `driver`/`mcp`/`alog` and nothing of the UI. Children deliberately do
  **not** go through `ui.Model.drv` — that stays the parent-only singleton with
  its generation counter.
- `internal/ui` — the Bubble Tea model. `model.go` (state + ingest), `update.go` (event
  routing + keys), `view.go` (header/footer, stacked gate and queue panels, help/resume/ask
  overlays), `render.go` (structured transcript entries → lipgloss, incl. `clampBlock`
  line-capped gutter blocks), `phase.go` (phase machine plus the message queue —
  `sendInput`/`flushQueue`), `paste.go` (a dragged-in path becomes an absolute path
  reference), `commands.go` (slash commands + resume picker), `ask.go` (AskUserQuestion
  panel), `gate.go` (countdown), `dispatch.go` (the `Dispatcher` seam onto the
  orchestrator), `fleet.go` (the architect's fleet-tool handlers and the `/fleet`
  report), `tickets.go` (the ticket-tool handlers and the `/tickets` report). It also
  holds the two front-end seams — `frame.go` (the read seam),
  `action.go` (the write seam) and `present.go` (the presentation decisions both front
  ends share). See [The seam](#the-seam-one-model-two-front-ends).
- `internal/htmlrender` — transcript entries as HTML, for the webview. A leaf package
  that takes primitives (`kind, title, body, raw, lang`) and **must not import
  `internal/ui`** — ui imports it, so the other direction is a cycle. goldmark for
  markdown (never `WithUnsafe`), chroma's *class-based* formatter for code (the
  webview's CSP forbids `unsafe-inline`, so color lives in `StyleSheet` and an entry
  carries none), bluemonday over the result as a second line of defence. Every body it
  renders is untrusted — model output and raw tool results — which is what the
  adversarial tests are for. The dark theme is pinned to the same `dracula` as
  `chromaTheme`, so the terminal and the webview highlight identically. Rendered once at
  ingest and only when `ui.Config.RenderHTML` is set: `acy run` never reads it and must
  not pay for it.
- `internal/hub` — the headless runtime: one `ui.Model`, one goroutine, a loop over
  `Update` and the `tea.Cmd`s it returns. `Update` is a pure function of (model, msg) and
  needs no TTY, so this is everything `tea.NewProgram` gives the terminal, minus the
  terminal. Anything that is not the TUI drives the model through it — the live e2e
  harness today, the HTTP server behind the webview next. It also owns frame delivery:
  after every `Update` it marshals `Model.Frame()` once, and **emits nothing if the bytes
  are unchanged**. That is what keeps an idle run silent (the model ticks every 120ms and
  `Frame` deliberately carries no clock), and each subscriber's mailbox is one deep — a new
  frame replaces an undelivered one, so a slow client can miss the middle of a story but
  never its ending.
- `internal/server` — the HTTP transport in front of the Hub, and what `acy serve`
  starts: `GET /api/frame`, `GET /api/events` (SSE, `id:` = the hub's rev), `POST
  /api/action`, `GET /api/sessions`, `GET /api/highlight.css`, plus an
  unauthenticated `/healthz`. Loopback-only listener, a 256-bit bearer token on
  every `/api/*` route (constant-time compared), and a CORS surface kept as small
  as it can be while still working: the grant is *reflected* for
  `vscode-webview://…` and never `*`, because a webview really is cross-origin and
  its `Authorization` header really does trigger a preflight someone has to answer.
  A refused action is **200 with the refusal**, not a 4xx — the run moves on its
  own, so "that gate is already gone" is a domain answer. Spec:
  `docs/webui-protocol.md`.
- `internal/session` — reads claude's `~/.claude/projects/<slug>/*.jsonl` transcripts:
  `List` for the `/resume` picker, `Replay` to turn one back into `[]driver.Event` for the
  transcript view. Injected as `Config.Sessions` / `Config.Replay`, so tests supply fakes.
- `internal/engineerwire` — the NDJSON wire contract between an architect and its detached
  engineers, and the seq-numbered journal both sides read: `types.go` (the message structs —
  `Spec`/`Answer`/`Cancel` inbound, `Hello`/`Event`/`Question`/`Result` outbound), `codec.go`
  (marshal/decode by envelope `type`), `journal.go` (`Open`/`Append`/`ReplayFrom`/`Follow`).
  The message shapes and the seq/replay contract are already fully specced in
  `docs/engineer-protocol.md` — read that before this package's comments, not instead of it.
  Deliberately no CLI, no process spawning, no git: "the ground the rest of arch mode gets
  built on."
- `internal/gitops` — the deterministic git/gh layer behind an engineer: `EnsureWorktree` /
  `RemoveWorktree` (an isolated worktree on a fresh branch off the base), `CommitsAhead`
  (did the run actually commit anything), `Push`, `BranchName`, `CreatePR`. Every git/gh
  invocation here is a fixed argv the package itself chooses — nothing in it is model-driven.
  A `Runner` seam (`func(ctx, dir, name string, args ...string) (string, error)`) threads
  through every call so tests fake `git`/`gh` instead of running them.
- `internal/verify` — runs the fleet-configured `verifyCommands` in an engineer's worktree
  as evidence collected by acy itself, never the model's own claim of having run them.
  `Run` parses each command with `strings.Fields`, no shell involved; strips `ACY_LIVE` from
  the environment before running anything; bounds each command by the configured
  per-command timeout; caps captured output at 8KiB; and classifies each command as
  passed/failed/skipped (binary not found)/timeout/error. A `Runner` seam mirrors
  `gitops.Runner` so tests fake the process instead of running one.
- `internal/engineer` — the headless runtime that drives one ticket end to end: `core.go`
  builds the brief and steers a supervisor through PLAN/AUTO-RUN, `ask.go` escalates every
  `AskUserQuestion` up the journal as a `Question` and blocks for an answer (15-minute
  timeout fallback), `drive.go` polls for the model's own `Finish` and only then —
  deterministically, in Go, never left to the model — checks `gitops.CommitsAhead`, pushes,
  and opens the PR. It does not own the supervisor process itself (`internal/supervisor`) or
  process detachment (`internal/engineerd`). Before pushing/opening the PR, `finalize` also
  runs the fleet-configured verify commands via `internal/verify`, folds the result into
  `Result.Verification` and a digest appended to the summary and PR body, and — if a check
  actually failed (not just skipped or timed out) — flips the outcome to `"failed"` while
  still pushing the branch and opening the PR.
- `internal/engineerd` — daemon plumbing for one detached engineer: state-dir layout
  (`dir.go`), the `control.sock` Unix socket carrying inbound `Answer`/`Cancel` (`control.go`),
  `Attach` (`attach.go` — replay-then-follow the journal one way, forward control messages
  the other), and `RunDetachedTarget` (`run.go` — the re-exec entrypoint the `setsid`'d child
  actually runs). Detachment itself (`Setsid: true`) is `internal/cli`'s job, not this
  package's — this package assumes it has already happened.
- `internal/fleet` — everything about running engineers on named hosts: `transport.go` /
  `local.go` / `ssh.go` (exec `acy engineer start`/`attach` directly, or over a hard-wired
  `BatchMode=yes` ssh), `follow.go` (reattach forever with backoff across a dropped
  connection), `prwatch.go` (poll `gh pr list` for `acy/`-headed PRs), `doctor.go` (the
  seven-check host health probe behind `acy fleet doctor`), `manager.go` (the orchestration
  core: per-host capacity, the fleet-wide run-budget ceiling, PR-cap backpressure). It owns
  no ticket state (`internal/tickets`) and decides nothing about *what* work to dispatch —
  that judgment call is the architect's alone.
- `internal/tickets` — a deterministic markdown ticket board at `.acy/tickets/<id>-<slug>.md`
  **inside the repo being worked on**, not acy's state dir, so the ledger travels with the
  clone and the PR diff. Hand-rolled frontmatter (no YAML dependency), a flat five-status set
  (`todo`/`in-progress`/`in-review`/`merged`/`blocked`) with no enforced transitions,
  `UpdateFields` for partial updates that never clobber a branch/PR another call already
  recorded. It runs no git beyond add/commit/push of its own directory and never talks to
  GitHub itself: detecting a merge is `internal/fleet`'s `PRWatcher`, and turning that
  detection into a ticket update is the architect's own `UpdateTicket` call — prompt-driven,
  not code-driven. `StackChains` (the walker over `stack_on`) lives here rather than in
  `internal/ui` so `flow.go`'s `Mermaid`/`ASCII` and `internal/ui/tickets.go`'s board
  rendering share one implementation instead of two that could disagree, and `Store.Put`
  writes `.acy/tickets/flow.mmd` *inside* `.acy/tickets` deliberately, so it free-rides the
  store's own git add/commit of that directory rather than needing separate handling.
- `internal/supervisor` — the constructor (`NewSupervisor`) extracted out of `internal/cli`
  that wires the gate server, hook/MCP config files, the MCP bridge, the launcher/spawner
  closures and the orchestrator into one running supervisor: the shared foundation `acy run`,
  `acy serve` and `acy arch` all build on, none of which may import each other.
  `Flags.Fleet` / `Flags.Tickets` / `Flags.ArchMode` are arch mode's only forks into it —
  they pick `mcp.RoleArchitect` / `ui.ArchSystemPromptFor` over the parent role and are nil/false
  for a plain run.
- `internal/state` — acy's own snapshot per session (phase, plan, rounds, cost): the part
  of a run claude's transcript does not record. Atomic JSON under
  `$ACY_STATE_DIR` (else `<user config dir>/acy/sessions/<id>.json`). Injected as
  `Config.LoadState` / `Config.SaveState`.
- `internal/e2e` — the live end-to-end suite: drives the real supervisor (real gate, real
  hook, real claude, real state files) headlessly, on your subscription. `ACY_LIVE=1` only;
  it can never run in CI.
- `internal/cli` — Cobra commands: `run` (the TUI), `serve` (the same supervisor
  headless, over HTTP), and the hidden `hook`. `run` and `serve` share one flag
  registration (`addRunFlags`) so their run settings cannot drift, and each keeps
  its own pflag instances so `applyFileConfig`'s `.acy.json` overlay — which keys
  on cobra's `Changed` — still tells a defaulted flag from an explicit one. `arch`
  (`arch.go`) shares that same flag registration and adds none of its own; it just
  requires a `"fleet"` section in `.acy.json` and wires `fleet.Manager` /
  `tickets.Store` through `supervisor.Flags`. `fleet doctor` (`fleet.go`) runs
  `fleet.Doctor` per configured host. `engineer` (`engineer.go` — `start`, the
  hidden `__run`, `attach`, `tail`) is the hidden subcommand a fleet host actually
  runs; the `setsid` detach call itself (`engineer_detach_unix.go`) lives here, not
  in `internal/engineerd`.
- `internal/alog` — process-wide debug logger (off until `Open`); the `--log` flag drives it.
- `vscode/` — the VS Code extension, which now has **two** front ends over the same
  supervisor.
  - **The terminal launcher**, and still the default. It resolves the acy binary
    (`acy.binaryPath` setting → `bin/acy` bundled in a platform build → PATH) and runs it
    *as* the terminal shell (no user shell, no quoting, the terminal dies with the
    supervisor). One supervisor terminal per window — a second run reveals, never
    relaunches.
  - **The panel** (`acy.openPanel`, `src/panel.ts`): `src/serve.ts` spawns `acy serve` as
    an ordinary child process, reads the one endpoint line off its stdout
    (`src/endpoint.ts`), forwards stderr to the `acy` output channel, and hands the
    webview a URL and a token. **One `acy serve` and one panel per workspace folder**,
    keyed by folder path and idempotent while the spawn is still in flight, because two
    supervisors on one project is this repo's classic footgun — the same reason the
    terminal reveals rather than relaunches. The panel owns the supervisor's lifetime:
    closing the tab stops it, and `deactivate` kills it. (The *terminal* is deliberately
    left alive across an extension reload; a served supervisor cannot be, because it is
    the extension host's own child with no window of its own — leaving it would orphan a
    claude session nobody can see or stop.) The URL goes through `vscode.env.asExternalUri`
    so a Remote-SSH or Codespaces window forwards the port instead of handing the webview
    somebody else's `127.0.0.1`, and that same origin is what the CSP is pinned to.
  - **`acy.useTerminal` chooses between them, and defaults to `true`.** `acy.run` opens the
    terminal exactly as it always has; the switch exists so the flip is a one-line change
    once the panel's visual design lands. The status bar (`acy.start`) asks which.
  - Both paths share `resolveLaunchable` in `src/extension.ts` — binary resolution, the
    chmod repair for a bundled binary VS Code unpacked without its execute bit, and the
    `PATH`-prepending for a `claude` found in a well-known install dir. Neither front end
    puts a shell in between, so that is one problem twice, and a second copy would drift.
    `src/claude.ts` discovers claude (`.acy.json` `claudeBin` → the `acy.defaults.claudeBin`
    setting → `PATH` → well-known install dirs) and warns at startup when there is none —
    acy has nothing to supervise without it, so the alternative is a run that dies on its
    first turn.
  - Run settings travel in `.acy.json`, never flags, so CLI and extension can't disagree.
  - `webview/` is the client: `transport.ts` (fetch the frame, hold the SSE stream open,
    reconnect with backoff, POST actions), `protocol.ts` (the `Frame`/`Action` types,
    mirroring `internal/ui`), `render.ts` (a deliberately plain placeholder awaiting the
    design mock — replaceable wholesale, and nothing outside it depends on its shape),
    `main.ts` (the entry point). `esbuild.mjs` builds **two** bundles because there are two
    runtimes: `dist/extension.js` (node, cjs, `vscode` external) and
    `webview/dist/webview.js` (browser, **IIFE** — the webview's CSP is `default-src
    'none'` with a per-load nonce, so it must be one plain script with no imports at load
    time). `.vscodeignore` ships `dist/` and `webview/dist/*.js` and excludes both source
    trees and every `.map`.
  - The decision logic lives vscode-free in `src/launch.ts` / `src/config.ts` /
    `src/claude.ts` / `src/endpoint.ts` and is tested with plain `node --test` (`npm test`
    in `vscode/`), which also compiles and drives `webview/transport.ts` directly — that is
    the only way any of it is exercised without a browser.
    `src/test/serve.integration.test.ts` is the one test that proves the plumbing: it
    builds the real Go binary, starts a real `acy serve`, and drives it through the real
    `transport.ts`. It **skips if `go build` cannot run**, which is why the `vscode` CI job
    sets up Go — without a toolchain it passes while proving nothing.
    `npm run package` builds an installable `.vsix`.

## The seam: one model, two front ends

acy now renders in a terminal and in a VS Code webview. **There is still exactly one
`ui.Model` and one set of presentation decisions.** That is the property this section
exists to protect, because it is cheap to break by accident and expensive to notice: the
two front ends do not run at the same time, so a divergence shows up as "the panel is
wrong" weeks after the change that caused it.

The pieces, and where each decision belongs:

- **`internal/ui/frame.go` — the read seam.** `Model.Frame()` is a pure JSON projection of
  the whole run. It is a read: nothing in it mutates and nothing in it consults the clock.
- **`internal/ui/action.go` — the write seam.** `Action`/`ActionResult` is a semantic
  vocabulary — "allow this gate", "arm", "submit this text" — and `applyAction` is the
  single implementation of each. **The terminal's own key chords raise these same
  actions**: `handleGateKey` does not resolve a gate, it raises `GateAllow`; `Ctrl+G` does
  not arm, it raises `Arm`. The keyboard is a client, not a second door. There is
  deliberately **no "send a keystroke" endpoint**, because a synthesised `Ctrl+Y` means
  "whatever the terminal was looking at" and a webview is not looking at the terminal.
- **`internal/ui/present.go` — the presentation decisions.** Composer hints, help content,
  panel phrasing. Content lives here; styling stays in `view.go`. Nothing in `present.go`
  imports lipgloss or knows a color exists.
- **`docs/webui-protocol.md`** is the written contract over `frame.go` and `action.go`.
  Change one, change both.

The rules a future agent will otherwise break:

- **A UI fix belongs in `present.go` or in the frame — never in the webview's TypeScript.**
  If the panel says the wrong thing, the terminal is about to say the wrong thing too. Fix
  it once, in Go, and let both front ends read it.
- **The webview holds no product strings.** Hints, statuses, refusal reasons and transcript
  bodies all arrive in the frame or in an `ActionResult`. `webview/render.ts` is a
  placeholder awaiting a design mock and will be replaced wholesale; anything you write
  into it is something you are volunteering to write twice.
- **The webview renders no markdown and highlights no code.** Every entry arrives as a
  sanitized HTML fragment in `entry.html` from `internal/htmlrender`. A second markdown
  stack in the client would be a second implementation of the transcript — and its CSP
  forbids `unsafe-inline` anyway, which is why styling travels separately as
  `GET /api/highlight.css`.
- **Gates are answered by `toolUseId`, never by position.** Gates auto-approve on their own
  countdown, so between a client rendering a list and its request landing, the head of the
  queue may be a different tool entirely. An id that names nothing resolves **nothing at
  all** and comes back rejected — there is no fallback to the front of the queue, because
  that fallback is exactly how you approve a tool nobody looked at. The open `Ask` is keyed
  by question index for the same reason.
- **`Frame` deliberately contains no "now".** Change detection in `internal/hub` compares
  the marshalled bytes, so a clock anywhere in the frame would make every one of the
  model's 120 ms ticks look like news and push eight frames a second at an idle run
  forever. Countdowns travel as an **absolute deadline** (`deadlineUnixMs`), or as a frozen
  `remainingMs` once `paused` is set — exactly one of the two is ever non-zero — and the
  client animates from its own clock. `turnStartUnixMs` is absolute for the same reason.
  Do not add a `now`, a `renderedAt`, a sequence stamp, or anything else that changes on
  its own.
- **Every action validates itself**, even where `update.go`'s key routing already guards
  the same thing. An HTTP caller does not press keys, so it never passes through that
  routing; the guard has to live where the behaviour does. (The `interject` refusal while a
  gate is pending is the load-bearing one: the PreToolUse hook is blocked on the gate
  socket, and interrupting the turn out from under it deadlocks — over HTTP just as easily
  as with Esc.)
- **A refused action is a domain answer, not an error.** `{"accepted":false,"reason":…}`
  with a **200**; the run moves on by itself, so "that gate is already gone" is news, not a
  malformed request. Only a kind this build has no vocabulary for is a 4xx.

## Arch mode

`acy arch` runs a whole fleet instead of one checkout. The architect still only reads
(`Read`/`Grep`/`Glob`, same restriction as the parent), but once armed it delegates whole
tickets to **engineers** — full, unattended `acy` instances on hosts named in `.acy.json`'s
`fleet` section — instead of local `Dispatch` children. Operator-facing detail is in
[`docs/arch-mode.md`](docs/arch-mode.md); the wire format is in
[`docs/engineer-protocol.md`](docs/engineer-protocol.md). What follows is what actually took
real probing to learn, so a future agent doesn't have to relearn it by breaking it first.

- **An engineer's lifetime is not the ssh/attach connection.** `acy engineer start`
  re-execs itself with `SysProcAttr{Setsid: true}` (`internal/cli/engineer_detach_unix.go`)
  *before* the fleet transport — local exec or ssh — ever attaches to it. `setsid(2)` makes
  the child its own session leader with no controlling terminal, so there is nothing for a
  SIGHUP to travel through when the ssh connection (or the architect itself) dies. The
  engineer keeps working; the only thing that changes is who's watching it.
  `TestE2EArchResumeRecoversEngineer` is the live proof: kill the architect mid-flight and
  the detached engineer still finishes its ticket and opens the PR with nobody attached at
  all.
- **The journal is the source of truth, not the connection.** Every engineer writes
  `Hello`/`Event`/`Question`/`Result` to a seq-numbered, append-only journal
  (`internal/engineerwire/journal.go`) before anything reads it live. `ReplayFrom(n)`
  reconstructs history byte-for-byte and `Follow` polls for what comes after. **`Hello` is
  always seq 1** — not a special case, but a structural guarantee: `Append` assigns
  `lastSeq+1`, and `Hello` is simply the first thing an engineer ever appends. An architect
  reattaching after a crash, or an operator running `acy engineer tail <id>`, never needs
  the live process — it needs the journal.
- **`Journal.Open`'s torn-tail truncation has to be judged from one read, not two.**
  `scanJournal` returns `validSize` (bytes through the last complete, decodable line) and
  `rawSize` (total bytes actually read) from the *same* pass over the file, and `Open`
  truncates only when `rawSize > validSize` from that single snapshot. The first version
  compared `scanJournal`'s read against a second, later `os.Stat` — and a live engineer's
  perfectly valid append landing in the gap between those two reads looked exactly like the
  first writer's own torn tail, and got silently truncated away. That's a real production
  path, not a test artifact: an architect's `Attach` opens a journal a live engineer may
  still be writing to. Fixed in `2b53279`; if you ever see two reads feeding one truncation
  decision, that's this bug again.
- **macOS's ~104-byte `sockaddr_un` limit has already bitten this feature twice.** An
  engineer's control socket lives at `$ACY_STATE_DIR/engineers/<id>/control.sock`, and
  macOS's per-user `$TMPDIR` alone runs about 49 bytes before the engineer-id path even
  joins it — comfortably over budget with any descriptive directory name in the mix. It
  first sank `internal/engineerd`'s own unit tests (fixed with a short, test-name-independent
  `os.MkdirTemp("", "engd")` leaf instead of `t.TempDir()`), then sank `internal/e2e`'s live
  engineer test the same way: `ListenControl` failed before `core.Run` (and its `sendHello`)
  ever ran, so the attach side saw nothing and just timed out. Fixed by anchoring test state
  dirs at **`/tmp`** directly, not `t.TempDir()` and not `$TMPDIR`. Any new test that hands
  an engineer a state dir needs the same anchor.
- **THE RECURRING BUG: a new architect MCP tool has to be added in three places, or it
  fails live with "unknown tool".** `internal/mcp/protocol.go` defines it — the constant,
  the schema, the description. `internal/cli/mcp.go`'s stdio-forwarding switch has to route
  it to the supervisor, or the call comes back `unknown tool: %s` at runtime — this has
  already been forgotten twice: once for the entire fleet-tool batch (`LaunchEngineer`,
  `Await`, `AnswerEngineer`, `FleetStatus` all shipped unroutable, fixed in `3fa376a`), and
  again when the ticket tools landed (`ReadTickets`/`UpdateTicket` needed the same wiring in
  `b39297d`). And `internal/ui` needs two separate touches of its own — the fleet-tool name
  map and banner switch in `model.go`, and the actual dispatch switch in `update.go` — so
  "three places" is really four edits under three names. Adding a tool and stopping after
  `protocol.go` is indistinguishable from success until the model calls it live and gets
  refused.
- **Engineer briefs must never tell the child to push a branch or open a PR.** `drive.go`'s
  `finalize` does both, deterministically, in Go, after the model calls `Finish`: it checks
  `gitops.CommitsAhead` and only then pushes and calls `gitops.CreatePR`. Left unsaid, a
  capable child reaches for `gh pr create` on its own initiative — normal etiquette for a
  coding agent — and `drive`'s own PR ends up a duplicate. A real run produced **4
  `pr-create` calls for 2 tickets** before the brief text (`core.go`) was fixed (`4c11c51`)
  to say outright: commit your work locally, but do not push the branch or open a PR
  yourself — that happens automatically once you call `Finish`.
- **Merge and default-branch protection is three separate mechanisms, not one, and only
  two of them are load-bearing.** `internal/tickets/commit.go`'s `Store.Commit` resolves
  the checked-out branch (`git rev-parse --abbrev-ref HEAD`) after committing the ticket
  board locally, and skips `git push origin HEAD` — returning `tickets.ErrPushSkipped`,
  handled as success-with-a-note by `internal/ui/tickets.go` — whenever that branch is
  `main`, `master`, or the store's `BaseBranch` (`fleet.baseBranch`, wired in
  `internal/cli/arch.go`) (`37697a7`). `internal/ui/guard.go`'s `mergeGuardVerdict`,
  consulted by `gate.go`'s `enqueue` before any countdown is even raised, denies outright
  — never counts down — any `gh pr merge`, any `gh api` call against a `/merges`
  endpoint, and any `git push` whose refspec resolves to a protected branch
  (`Config.Trunk` union `main`/`master`) (`352172a`). And both system prompts
  (`ArchSystemPromptFor` in `phase.go`, `briefText` in `internal/engineer/core.go`) now
  say outright that acy and an engineer must never merge, never push to the default
  branch, and never run `gh pr merge` (`4a4ed01`). Read `guard.go`'s own doc comment
  before trusting the middle one too far: it is pure string matching over a Bash command
  that has not run yet, not a sandbox — a base64-encoded or aliased command walks right
  past it. What is actually load-bearing is structural: the supervising session's
  `--tools` registry has no `Bash` in it at all, and `internal/gitops` only ever pushes
  an engineer's own branch, deterministically, in Go, never through a model — and
  because a remote engineer runs this same `ui.Model` via `internal/supervisor`, it
  inherits all three for free, with no second copy to keep in sync.
- **A third-party MCP server merged into `--mcp-config` doesn't weaken
  `--strict-mcp-config`, because acy is still the sole author of that one file.**
  `internal/config.WriteMCPConfig`'s new variadic `extra ...ExtraMCPServer` parameter
  lets `internal/supervisor.jiraExtraServers` fold a project's configured Jira server
  in alongside acy's own `mcpServers` entry, but only into the **architect's** own
  config (`f.ArchMode && f.Jira != nil`, gated in `supervisor.go`) — never a plain
  run's, and never a dispatched engineer's, which is built from its own flags and
  never sees `jiraExtraServers` at all. `driver.Options.StrictMCP` stays `true` for
  this session regardless, and correctly so: what `--strict-mcp-config` actually
  guarantees is "no server the model's own user configured on this machine sneaks
  into the registry," not "no server besides acy's own." That guarantee still holds
  with Jira merged in, because acy itself chose to write it into the one
  `--mcp-config` file being pointed at — the model never gets a config of its own to
  smuggle a server through.
- **A fleet host must be authenticated for *non-interactive* ssh, which is not the same
  as being authenticated for you.** The real check is `ssh -o BatchMode=yes <host> --
  '<wrapper>; claude -p "reply with exactly: PROBE_OK"'` and `gh api user` over that same
  connection — not `claude auth status` on a terminal you're sitting at. Observed live:
  host `studio` is used interactively every day and still returned `Not logged in ·
  Please run /login` and an HTTP 401 from `gh` over a `BatchMode` ssh session, with
  neither `CLAUDE_CODE_OAUTH_TOKEN` nor `GH_TOKEN` present in that non-interactive
  environment. An engineer launched on a host in this state can't run `claude` and can't
  open its own PR. The durable fix is exporting a `claude setup-token` OAuth token and a
  `GH_TOKEN` from the host's rc file, which the fleet wrapper already sources for every
  remote command.
- **`.acy.json` is snapshotted at construction.** `fleet.NewManager`
  (`internal/fleet/manager.go`) copies `cfg.Hosts` into `hostsByName` when the supervisor
  starts, so editing `.acy.json` mid-run changes nothing for the session already running —
  `acy arch` has to be restarted to pick up a host-list change. The ticket board on disk
  (`internal/tickets`) is what carries a run across that restart, not the config file.
- **`rcWrap` no longer assumes zsh.** The shell an `rc` file sources through is now
  derived from the rc file's own basename (`shellForRc`, `internal/fleet/remotepath.go`),
  overridable with a host's `shell` key, falling back to `sh` when the basename doesn't
  match a known shell family. It used to hardcode `zsh -c`, which failed outright on a
  bash-only host: observed live on Ubuntu host `spark`, where every fleet command died
  with `zsh: command not found` (exit 127) — and `acy fleet doctor` misreported that as an
  unreachable host rather than a broken rc wrapper, because a bare-ssh probe and the
  rc-wrapped probe weren't distinguished. Doctor now probes bare ssh first and only
  escalates to the full `rc`-wrapped command when that succeeds, so a broken wrapper is
  diagnosed as exactly that instead of masquerading as an unreachable host.
- **Live e2e arch tests take 10–25 minutes; a Bash tool call caps around 600 seconds.**
  `TestE2EArchRunsEngineersInParallel` and its siblings are real `claude` sessions on real
  (or simulated) hosts and cannot be rushed. Backgrounding one and walking away has already
  lost three sessions their runs — the shell or the harness reclaims the process before the
  test ever reports. Tee the run to a log file and poll it with short, separate commands
  instead of trusting a single long-running call to come back:
  ```sh
  ACY_LIVE=1 go test ./internal/e2e/ -run TestE2EArchResumeRecoversEngineer -v -timeout 20m \
    > /tmp/arch-e2e.log 2>&1 &
  # then, repeatedly, in fresh short calls:
  tail -n 40 /tmp/arch-e2e.log
  ```
  Never background a live e2e run and consider it handled — poll it, don't fire-and-forget
  it; it's a paid Claude session part-way to a real PR, not a fire-and-forget shell job.
- **`gh-stack`'s real contract with this package is its exit code, never its stderr text.**
  `classify` (`internal/gitops/stack.go`) branches only on `ExitCode(err)` against the
  fixed table above it — 2/3/4/6/7/8/9 each map to a sentinel, everything else wraps
  verbatim — and the doc comment above that table says outright why: stderr wording is
  for humans and can change across gh-stack releases, while the exit code is the one thing
  a preview CLI has committed to keeping stable.
- **`gh stack view` without `--json` pages through `less -R` and hangs a non-TTY process
  forever.** `StackView` (`internal/gitops/stack.go`) only ever calls it with `--json`, and
  the package doc comment at the top of the file names this explicitly as the mistake a
  future caller is likely to make.
- **`gh stack modify`, `gh stack switch`, and bare `gh stack submit` (no `--auto`) are
  interactive full-screen TUIs and this package never calls any of them.** Same doc comment
  in `internal/gitops/stack.go` lists all three; every exported function in the file only
  ever shells out to `link`, `view --json`, `rebase`, `push`, `sync`, and `init` for exactly
  this reason.
- **`gh stack link` needs no local tracking — it only reads/writes GitHub's own view of a
  PR's base branch — which is what lets `StackKeeper.Link` run it synchronously on the hot
  path.** `Link`'s own comment (`internal/fleet/stackkeeper.go`) is explicit that this is
  the *only* stack operation safe to run directly in `k.dir`, the operator's own working
  tree: it is called from inside `forwardPRWatcher`'s own goroutine (`internal/fleet/manager.go`)
  on every PR open, and a caller on that path owns no checkout of its own to run anything
  else in.
- **The PR cap counts stack roots, not every open `acy/*` PR, and it is recomputed fresh on
  every poll rather than tracked as state.** `prSnapshot.isRoot` (`internal/fleet/prwatch.go`)
  derives "root" from `baseRefName` reported live by `gh pr list`, and `OpenCount` /
  `StackedCount` both read through it rather than through anything `Manager` maintains
  itself. `isRoot`'s own comment explains why that matters beyond correctness: a resumed
  `Manager` has no memory of which PRs a since-vanished engineer stacked, but GitHub's own
  base-branch report is still authoritative, so a resume needs zero extra bookkeeping to get
  the cap right again.
- **Nothing that needs a checkout may ever run in the operator's own working tree — except
  `StackKeeper.Link` (see above), which needs none.** `AssembleStack`'s `assembleWorktree`
  (`internal/ui/assemble.go`) mints a disposable `git worktree add --detach` scratch dir per
  call, and `StackKeeper.Sync` (`internal/fleet/stackkeeper.go`) keeps one long-lived
  deterministic worktree for repeated background syncs — deliberately two different
  worktrees, per `assembleWorktree`'s own comment, so Assemble's one-shot
  init/rebase/push/link sequence can never race a background `Sync` sharing the same
  checkout.
- **Force-pushing an engineer's branch during assembly is safe because the engineer is
  already finished, and `--force-with-lease` is only the backstop.** `StackPush`
  (`internal/gitops/stack.go`) pushes every branch in the stack with a per-branch
  `--force-with-lease`; `looksLikeLeaseRejection` in `internal/ui/assemble.go` exists
  specifically to catch the case that backstop is for — a human's own local commits landing
  on an `acy/*` branch after this run last observed it — and escalates rather than retrying,
  because retrying blind would force-push over whatever they just added.
- **A stack rebase conflict is always escalated to a human, never auto-resolved, and `gh
  stack init` enabling `git rerere` is what makes resolving the same conflict twice cheap.**
  `StackAssemble`'s doc comment (`internal/gitops/stack.go`) says so directly: `ErrStackConflict`
  (exit code 3) means stop and put a human in the loop, and because `init` turns on rerere,
  a conflict a human resolves once here auto-resolves itself the next time the identical
  rebase runs — the second `gh stack rebase` pass replays the recorded resolution instead of
  asking again.

## Hard-won facts about `claude` stream-json (verified live, v2.1.207–2.1.220)

- Flags: `-p --input-format stream-json --output-format stream-json --verbose`. Output
  stream-json **requires** `--verbose`. Hook lifecycle needs `--include-hook-events`.
- Inject a user turn as one stdin line: `{"type":"user","message":{"role":"user","content":"..."}}`.
- **`init` is not emitted until the first user message** — so there's no session id (and
  Ctrl+G can't arm) until you've sent something in PLAN. Status starts as "planning", not
  "connecting".
- The `result` event is the idle/done signal (`stop_reason`, `terminal_reason:"completed"`,
  `total_cost_usd`, `permission_denials[]`).
- **Permission gating uses a PreToolUse hook, NOT the SDK control protocol.** In raw
  stream-json `default` mode, tools auto-run with no prompt; the hook re-introduces the gate.
  It receives `{tool_name,tool_input,tool_use_id,session_id,cwd,...}` on stdin, **blocks
  claude until it prints** `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow|deny|ask",...}}`.
  The countdown lives inside that block.
- **In plan mode, edits never execute even if the hook allows them** — plan mode blocks them.
  That's why arming spawns a new `default`-mode process via `--resume <session_id>` (same cwd).
  `--resume` carries the plan context (verified).
- The plan itself arrives inside the `ExitPlanMode` tool call's `plan` field — render it,
  don't truncate it.
- **Interrupt**: write `{"type":"control_request","request_id":"<id>","request":{"subtype":"interrupt"}}`
  on stdin (capability `interrupt_receipt_v1`); the turn ends `terminal_reason:"aborted_streaming"`.
- **`AskUserQuestion` and `ExitPlanMode` do not exist in `-p` mode.** Measured against 2.1.207
  (`internal/ui/ask_live_test.go` probes this live): the `system/init` event advertises a fixed
  30-tool registry — `Task, Bash, CronCreate, …, ToolSearch, WebFetch, WebSearch, Workflow,
  Write` — and **neither tool is in it**, under every `--permission-mode`, with or without
  `--allowedTools`, with or without a scrubbed environment. They appear to be interactive-TUI
  tools. An earlier note here blamed a nested-agent-harness tool registry; that was wrong — a
  clean shell behaves identically. Consequences:
  - `--plan-tools`' `AskUserQuestion` default (`cli/run.go`) is inert: you cannot allowlist a
    tool that isn't in the registry.
  - The ask panel (`ui/ask.go`) is **unreachable in production** — claude never emits the
    `tool_use`. Its code and tests are correct and stay exercised offline against
    `ui/testdata/ask_tool_use.json`, but nothing triggers them at runtime.
  - `m.planReady` / the boxed plan entry are unreachable for the same reason; the plan still
    arrives, just as ordinary assistant text. `Ctrl+G` is unaffected (it only needs a session id).
  - The route that would actually work is an **MCP tool** (`mcp__acy__…`): MCP tools *are* added
    to the registry. `baseToolName` (`ui/model.go`) already strips the `mcp__<server>__` prefix
    so the panel and the gate bypass would pick one up unchanged.
  - The assumed wire shape — turn **blocks** with no `result` event until you answer, answered
    via `driver.SendToolResult` (a user message carrying a `tool_result` block referencing the
    `tool_use_id`) — is therefore **still unverified**. Detection is name-based and harmless
    while the tool never appears.
- **`-p` sessions are persisted like any other**, at `~/.claude/projects/<slug>/<id>.jsonl`.
  The records are a *superset of the stream-json event* — `{type:"user"|"assistant",
  message:{role,content:[…]}}` with the same `text`/`thinking`/`tool_use`/`tool_result`
  blocks — which is why `session.Replay` unmarshals straight into `driver.Message` and acy
  has exactly one content parser. Sub-agent records carry `isSidechain:true`. There are **no
  `result` records** (so cost and turn boundaries can't be replayed — that's what
  `internal/state` is for) and **no `init` records** (so a replay can't clobber the live
  session id).
- **`--resume` keeps the session id in `-p` mode; it does not fork.** Verified against real
  transcripts: an armed run appends to the same jsonl the plan phase wrote. `--fork-session`
  is the opt-in that changes it. acy still tombstones a changed id (`state.SupersededBy`) in
  case that ever stops being true.
- **claude never echoes an injected user turn back on the stream.** Every live `user` event
  is a `tool_result`; the prompts acy sends are simply not in the output. They *are* in the
  transcript — so a replay must render user text itself (`ui.ingestReplay`), or you get
  Claude's answers with none of the questions.
- **The project-dir slug is not just `/`→`-`.** claude resolves symlinks first (`/var/…` is
  stored as `-private-var-…`) and maps *every* character outside `[A-Za-z0-9-]` to a dash
  (`/tmp/my.dotted_dir` → `-tmp-my-dotted-dir`). Guessing this wrong silently finds no
  sessions, so `session.Replay` also falls back to globbing every project dir for the id.
- **The `result` event carries a full `usage` object** and acy ignored it for months.
  `input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens`, plus per-model `modelUsage` with `costUSD` and
  `contextWindow`. Decode it; cost in dollars alone cannot tell you *why* a run was
  expensive.
- **`usage` is per TURN; `modelUsage` is per PROCESS and cumulative.** They are
  accumulated in opposite ways — usage ADDS, modelUsage (like `total_cost_usd`)
  is ASSIGNED — and getting it backwards is silent, because both keep producing
  plausible numbers. Proven in `internal/driver/testdata/result_events.jsonl`, five
  real turns spanning a `--resume`: per-turn cache reads 474322 / 306083 / 251393
  sum exactly to that process's cumulative 1031798, and then turn 4 **resets** the
  cumulative to its own value because a new process started — *while the session id
  never changes*. A session id therefore cannot tell you where a process began.
- **`--json-schema '<schema>'` works with `--output-format stream-json`**, and the
  `result` event then carries BOTH `result` (the JSON as a string) and a parsed
  `structured_output` object. This is what makes a child's report unfakeable, and it
  is what replaced the old STATUS sentinel (whose substring match would fire if a
  reply merely *mentioned* `STATUS: DONE`).
- **`--tools ""` gives a ~3.6k-token context floor.** Whether it also suppresses MCP
  tools is **unverified** — a one-shot probe showed the MCP server still `pending` at
  init, which is a probe artifact, not an answer. The parent uses `Read,Grep,Glob`,
  which sidesteps it. Verify before ever relying on `--tools ""`.
- **`system/init` reports MCP servers as `status:"pending"`.** The registry snapshot
  in that event is taken *before* they connect, so init's `tools` array is not the
  final word on what the model can call. (This is probably what the old note about a
  "fixed 30-tool registry" was really seeing.)
- Useful flags acy now relies on: `--json-schema`, `--max-budget-usd`, `--session-id`,
  `--effort`, and `--exclude-dynamic-system-prompt-sections` (moves per-machine
  sections out of the system prompt so many short children share one cache entry —
  free, and it composes with `--append-system-prompt`).
- **`--permission-mode default` is no longer in claude's documented choices**
  (2.1.220 lists `acceptEdits, auto, bypassPermissions, manual, dontAsk, plan`) but
  is still accepted. Watch it.
- No official Go SDK; Claude Code is Node/TypeScript.

## The TUI, on Bubble Tea v2

The UI is on `charm.land/bubbletea/v2`, `charm.land/bubbles/v2` and
`charm.land/lipgloss/v2`. glamour and chroma stay on their current majors on purpose:
they only ever hand back ANSI strings, so nothing about them is v1-vs-v2 — mixing them
in costs nothing and porting them buys nothing.

What changed that the code actually depends on:

- **Keys arrive as `tea.KeyPressMsg`**, not `tea.KeyMsg`. There is a matching
  `KeyReleaseMsg`; acy never wants it.
- **`View()` returns a `tea.View`, not a string.** The terminal modes that used to be
  `tea.NewProgram` options are fields on the frame you hand back each render. Alt-screen is
  the only one acy sets, so it travels on the model (`Config.AltScreen`).
- **A bracketed paste is a `tea.PasteMsg`**, not a burst of key runes. That is why a pasted
  document can never be read as an Enter press, and why any new interception in `update`
  must key on the message *type* rather than on "is there text".
- **v2 always negotiates Kitty keyboard disambiguation** — `keyboardEnhancementsFlags`
  starts at flag 1 unconditionally, no program option involved. That is the *only* reason
  `shift+enter` is a distinguishable key at all, and only in terminals that speak the
  protocol; elsewhere it arrives as a bare CR and simply sends. Hence `alt+enter` and
  `ctrl+j` as the portable newline bindings. `tea.View.KeyboardEnhancements` requests the
  *extra* features (key repeat, release events, alternate keycodes) — acy wants none.

Two bubbles traps in the composer:

- **`textarea.MaxHeight` is not a visible-height cap.** `atContentLimit` refuses
  `InsertNewline` once the value holds `MaxHeight` **logical** lines, so setting it to
  `maxInputRows` meant the ninth newline in a message silently did nothing and a pasted plan
  document came back out as a run-on sentence. acy sets `MaxHeight = 0` (no limit) and
  governs the *visible* height in `layout()`, which clamps to `maxInputRows` and lets the
  textarea scroll internally past that.
- **v2's `SetHeight` repositions the internal view to chase the cursor**, and only ever
  scrolls down. A box sized to the text alone is one row short whenever the cursor sits just
  past a line that exactly fills the width, so the shrink at the end of a keystroke would
  scroll away the text you just typed and never bring it back. `composerCursorRows`
  (`update.go`) asks the textarea how many rows the cursor needs — only it knows about that
  phantom next row — so the shrink never has to scroll at all.

### Gates no longer own the keyboard

The countdown panel **stacks above** the composer instead of replacing it, and the gate
actions are chords: **`ctrl+y` allow, `ctrl+x` stop, `ctrl+r` pause**. They used to be bare
`a`/`s`/`p`, which was only safe while the gate owned the screen — next to a live text box,
typing the word "and" would approve a tool and then pause the queue. In an armed run
practically every child tool call raises a gate, so a panel that stood in for the composer
left the user with no way to type for most of the run. Everything `handleGateKey` does not
claim falls through to the composer and the viewport.

**Esc is deliberately suppressed while a gate is pending.** The PreToolUse hook that raised
it is blocked on the gate socket waiting for a decision; interrupting the turn out from
under it is an unanswered-hook deadlock path. Answer the gate (`^Y`/`^X`) first, interject
after.

### The message queue

Enter during a busy session **queues** rather than dropping (`sendInput`, `phase.go`). It
used to refuse on `m.processing` and say nothing about it, which in an armed run meant the
key was simply dead. `busy()` is three things: a turn in flight, a gate waiting to be
answered (the turn that raised it is still open), or a dispatched task the parent is blocked
on.

- **The whole queue leaves as ONE turn**, joined by a blank line — never one turn each. A
  turn re-bills the entire accumulated context (the measurement at the top of this file is
  what that costs), so N sends would pay for the conversation N times over to deliver text
  the model reads in one pass regardless.
- **It is never persisted.** `snapshot()` does not carry it, on purpose: a message surviving
  a crash and being delivered into a different phase is worse than one that was lost. If the
  stream closes with messages held, `reportUnsentQueue` prints them back into the transcript
  so they can be copied out. `/queue` lists them, `/queue clear` drops them.
- **`flushQueue` is fully self-guarded** — non-empty queue, live driver, not ended, not busy
  — so a call site never adds conditions of its own. It fires from two places: the
  `eventMsg` case *after* `onTurnEnd` (which is what decides whether the turn really ended),
  and the `childMsg` case. The second one is not redundant: Esc with a task running cancels
  the dispatches and interrupts the parent, so the parent's aborted turn reports while the
  child is still shutting down and that flush is correctly refused for an active dispatch.
  The child then reports into an already-idle parent and **no further driver event is
  coming** — without the child-side call the queued redirect sits there forever, with an
  empty composer and no key that releases it.
- Esc/interject needs no send path of its own: the aborted turn's `result` (or the last
  child's completion) lands in one of those two sites and the queued text goes out as the
  redirect.

### Pasted paths

A dragged-in file becomes an **absolute path reference in the composer, never inlined
contents** (`paste.go`). The supervising session has `Read`, so the path is the whole
payload — inlining the file would spend exactly the tokens acy exists to save, and images
work for free because claude's own `Read` handles them. A paste is claimed only if it is
*entirely* file references (shell-escaped or quoted, as the terminal writes a drag, and each
one must stat); anything else falls through and the textarea inserts it verbatim. Bare words
are never paths — a token needs a separator or a leading `~`, or a sentence mentioning
`Makefile` would turn into a path depending on the working directory.

## Gotchas we already hit (don't reintroduce)

- **Never put a `strings.Builder` (or any struct with an internal self-pointer / mutex) as a
  value field in the Bubble Tea `Model`.** Bubble Tea copies the Model by value every Update;
  a copied `strings.Builder` dangles its `unsafe` self-pointer and the GC crashes with
  `fatal error: found pointer to free object`. `turnText` is a plain `string` for this reason.
- Drain every child pipe. `readStderr` must consume stderr (we send it to `alog`) or the
  child blocks once the pipe buffer fills.
- Driver swaps between phases are tracked by a generation counter (`gen`); stale events from
  a stopped driver are ignored, so stopping the old driver doesn't look like the session ending.
- **Anything that swaps the session must stop the old driver *and* bump `gen` in the same
  breath.** `applyResume` (resume.go) does; it has to. `/resume` is reachable mid-turn, and a
  `result` from the abandoned session arriving a second later still carries the current
  generation — it would bank the old session's cost into the restored run's tally and stamp
  the old phase over the restored snapshot.
- **A resumed run must clear `ended` / `processing`.** They describe a session that no
  longer exists. `sendInput` refuses to send while `ended` or `processing` is set, so
  resuming after "session ended" would otherwise leave the composer permanently dead.
- **`onDriverReady` clears `turnText`, always.** It used to make an exception for a
  resumed auto-run, because the completion check read that text for a STATUS
  sentinel. There is no such check any more; a resumed run is picked back up by an
  explicit prompt instead.
- **A resume takes its phase immediately, not when the driver lands.** Launching claude takes
  a second or two; until the phase moves, a restored run looks like a plan session with a
  session id — exactly what `Ctrl+G` arms from. Arming in that window launches a *second*
  process for the same session and kicks off work that is already half done.
- **The gate's bypass keys on ORIGIN, not phase.** It used to be
  `if m.phase == PhasePlan && tool != "Bash"` → allow, which was sound only because
  the plan registry had no Write or Edit. A dispatched child carries the *full*
  registry and shares the same gate socket, so phase stops identifying who is
  asking: keying on it would wave through every child edit with no countdown, in
  the phase where nobody is watching. `readOnlyParentTools` is an allowlist so a
  tool nobody anticipated counts down rather than sailing through. Note this had
  **zero test coverage** before — every existing gate test used `Bash`, which was
  excluded under both rules.
- **A child inherits `--mcp-config`.** Without the `--role parent|child` split it
  would gain `Dispatch` too and could spawn grandchildren without limit. Two config
  files are written per run, differing only in that flag.
- **`mcp.Serve` is strictly serial** — `handle` runs inline in the read loop, so a
  second `tools/call` is not read off stdin until the first answers. One task at a
  time is therefore a property of the transport, not a rule someone has to remember.
  Raising the concurrency limit requires making `Serve` concurrent first.
- **Stop the child before resolving the parent's blocked call.** Resolving first
  tells the parent the task is over while the process may still be alive and writing
  to the working tree.
- **Always resolve a cancelled dispatch.** The `acy mcp` process waiting on the
  socket belongs to the *parent's* process group, so killing the child does not
  release it — a missed `Resolve` hangs the parent's turn forever.
- **Deleting the nudge loop deletes crash recovery if you are not careful.** The
  loop's job was two things wearing one coat: re-asking "are you done?" after every
  idle turn (pure waste, gone) and picking a *restored* run back up (a real product
  promise — `TestE2EResumeAnArmedRunAfterACrash`). A resumed AUTO-RUN gets exactly
  one prompt, sent because a human explicitly asked to resume.
- **The viewport's default keymap steals typing.** bubbles/v2 `viewport.DefaultKeyMap()`
  binds `j/k/d/u/f/b` and space to scrolling, and `Update` forwards key events to both the
  input and the viewport — so typing those letters scrolled the transcript.
  `transcriptKeyMap()` in `update.go` restricts scrolling to the arrows and `PgUp`/`PgDn`.
  Don't hand the viewport an unrestricted keymap.
- **A hand-run multi-argument ssh probe silently lies instead of erroring.** `ssh host --
  zsh -c 'ls -l path'` gets joined into one space-separated string before the remote
  side ever sees it, so `zsh -c` takes only its first word as the script and the rest
  become `$0`/`$1` it never reads — `ls` runs with no arguments and lists `$HOME`
  instead of failing loudly. `stat`, `command -v`, `type`, and a bare multi-word `echo`
  all misbehave the same way, and a command placed after a `;` in the same string is
  unaffected (the remote login shell parses that part directly), which is what makes it
  so easy to misdiagnose live. Pass the remote command as one argument instead —
  `ssh host 'zsh -lc "ls -l path"'` — and canary any hand-run probe with
  `ssh host 'zsh -lc "echo A B C"'`: it must print `A B C`, not `A`. acy's own transport
  (`quoteArgv`/`sshBatchArgs` in `internal/fleet`) already composes commands this way;
  this only bites a human typing a probe by hand.

## Commands

```sh
make run                     # build the latest acy and dogfood it on this repo
make arch                    # same dogfood loop, but arch mode (= go build -o acy . && ./acy arch)
go build -o acy .            # build (= make build)
./acy run                    # the TUI
./acy serve                  # the same supervisor, headless over HTTP (prints {"url","token"})
./acy serve --port 7777      # ...on a fixed port; the host is always 127.0.0.1
./acy arch                   # plan -> arm -> a fleet of engineers, one PR per ticket
                              #   (requires a "fleet" section in .acy.json)
./acy fleet doctor           # ssh/acy/claude/gh/go/git/state-dir health, per configured host
./acy engineer tail <id>     # replay + follow one engineer's journal, human-readable
go test ./...                # unit tests (no network; = make test)
go test -race ./...          # what CI runs (= make race)
ACY_LIVE=1 go test ./...     # + live tests: real `claude`, spends a few cents (= make live)
ACY_LIVE=1 go test ./internal/e2e/ -run TestE2EArch -v -timeout 25m   # arch/fleet e2e only;
                              #   10-25m each — see "Arch mode" above before backgrounding this
golangci-lint run ./...      # lint (config in .golangci.yml, standard set; = make lint)
gofmt -l .                   # must be empty (= make fmt)
```

`serve` writes exactly one line to stdout — the URL and the bearer token — and nothing ever
precedes it there, so a parent process can parse it. To poke at a running one by hand:

```sh
curl -s -H "Authorization: Bearer $TOKEN" "$URL/api/frame" | jq .phase
curl -sN -H "Authorization: Bearer $TOKEN" "$URL/api/events"          # SSE, stays open
```

And in `vscode/` (Node 22; the same sequence the `vscode` CI job runs):

```sh
npm ci
npm run lint                 # eslint over src/ and webview/
npm run typecheck            # tsc over both trees — the webview has its own tsconfig
npm test                     # node --test; includes the live acy serve integration test
npm run compile              # both esbuild bundles: dist/ and webview/dist/
npm run package              # compile + vsce package → an installable .vsix
npm run watch                # both bundles, rebuilt on save
```

`npm test` builds the Go binary and drives a real `acy serve`, so it needs a Go toolchain —
without one that test **skips** and the client/server contract goes unproven. It says so
when it skips; read the output rather than the exit code.

Live tests need the `claude` CLI on PATH and auth. CI skips them.

## Conventions

- Keep the debug log useful: new subsystems should `alog.Printf`/`alog.Raw` their key events.
- UI transcript is structured `entry` values rendered in `render.go` — add an `ekind` rather
  than pre-styling strings, so entries re-render correctly on resize.
- End commit messages with the Co-Authored-By trailer. **CI enforces this** — the
  `ai-attribution` job fails a PR if any non-merge, non-bot commit lacks a
  `Co-Authored-By: Claude ...` line. Contributions here are AI-assisted by policy and the
  trailer is the record of it.

## Commits and releases

Commit subjects **must** follow [Conventional Commits](https://www.conventionalcommits.org)
— release-please parses them to decide the next version, and a subject it can't parse
contributes nothing to the changelog:

```
feat: let the working session report completion   -> minor bump (1.1.0)
fix: strip ANTHROPIC_API_KEY from the child    -> patch bump (1.0.1)
feat!: rename --countdown to --delay           -> MAJOR bump (2.0.0)
docs|test|chore|refactor|perf: ...             -> no release on its own
```

A `!` after the type (or a `BREAKING CHANGE:` footer) marks a breaking change, and from
1.0.0 onward that bumps the **major**. Plain semver, no pre-1.0 exemption — so a `feat!:`
is a decision to ship the next major, not a free bump. If a change is breaking but you do
not want a major yet, the answer is to make it non-breaking, not to mislabel it.

**The release job needs a repo setting, not just workflow permissions.** Settings →
Actions → General → Workflow permissions → *Allow GitHub Actions to create and approve
pull requests* must be on. The `release-please` job already declares
`pull-requests: write`, but that grants the token a scope the repo-level toggle can still
veto — and when it does, the action fails with "GitHub Actions is not permitted to create
or approve pull requests" *after* silently force-pushing its release branch, so the branch
looks up to date while no PR exists. Every Release run from 2026-07-13 to 2026-07-26
failed this way before anyone noticed, which is why no release had ever been cut.

Releases are automatic: merging to `main` makes release-please open or update a
`chore(release): x.y.z` PR. Nothing ships until you merge that PR — doing so tags the
commit, writes `CHANGELOG.md`, and `.github/workflows/release.yml` attaches the
cross-compiled binaries. Don't tag by hand; `.release-please-manifest.json` tracks the
current version and hand-tagging desyncs it.

A release carries three artifact families: tarballs for darwin/linux (amd64+arm64), a zip
for windows/amd64 (compiles cleanly behind the existing build tags, but the gate's unix
sockets are unverified there — treat it as experimental), and `.vsix` packages for the VS
Code extension — one per platform target with the matching binary bundled at `bin/`, plus
a `universal` one with no binary. The vsix version is stamped from the tag at package time
(`npm version` in the workflow), the same philosophy as the binaries' ldflags stamp:
`vscode/package.json` stays at `0.0.0` in git and release-please never touches it. CI
cross-compiles `GOOS=windows` on every PR so a broken windows build can't first surface
inside a release job.

A release also **publishes those `.vsix` packages to the VS Code Marketplace**
(`publish-marketplace`, same workflow). It downloads the assets the `vsix` job just
uploaded instead of re-packaging, so the Marketplace and the Release page serve the same
bytes, and it pushes every target in one `vsce publish --packagePath …` — the flag is
variadic, and a single call means a version lands whole rather than half-published.

That job depends on one-time human setup; none of it can be done from CI:

1. Register the publisher **`hweeks`** at <https://marketplace.visualstudio.com/manage>,
   with that exact ID — it must match `publisher` in `vscode/package.json`.
2. Mint an Azure DevOps PAT scoped to **All accessible organizations** with
   **Marketplace → Manage**. Scoping it to a single organization is the easy mistake and
   the expensive one: it returns 401 with no useful message.
3. Check it with `npx vsce verify-pat hweeks`.
4. Store it as the repo secret **`VSCE_PAT`**.

Until that secret exists the publish job prints a `::notice::` and exits 0 — the release
still succeeds, it just doesn't reach the Marketplace. The guard lives in the step body
because a `secrets` reference is not allowed in a job-level `if:`.

The project is licensed **WTFPL v2**. The `LICENSE` file lives twice — once at the repo
root, once at `vscode/LICENSE` — because `vsce` looks for it in the *extension* root, not
the repo root, and the copy is what puts the license on the Marketplace listing. Keep them
identical, and keep `"license": "WTFPL"` in `vscode/package.json` (a valid SPDX id; vsce
warns without it and the listing reads it). Packaging skips no license check: if
`vsce package` starts complaining about the license, the copy has gone missing — silencing
the check would only hide it.
