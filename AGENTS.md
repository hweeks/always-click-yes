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

A Go TUI (Cobra + Bubble Tea + Lipgloss) that supervises a **Claude Code** session
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
  routing + keys), `view.go` (header/footer/gate panel + help/resume/ask overlays),
  `render.go` (structured transcript entries → lipgloss, incl. `clampBlock` line-capped
  gutter blocks), `phase.go` (phase machine, STATUS-sentinel completion loop + manual
  done-check fallback),
  `commands.go` (slash commands + resume picker), `ask.go` (AskUserQuestion panel),
  `gate.go` (countdown).
- `internal/session` — reads claude's `~/.claude/projects/<slug>/*.jsonl` transcripts:
  `List` for the `/resume` picker, `Replay` to turn one back into `[]driver.Event` for the
  transcript view. Injected as `Config.Sessions` / `Config.Replay`, so tests supply fakes.
- `internal/state` — acy's own snapshot per session (phase, plan, rounds, cost): the part
  of a run claude's transcript does not record. Atomic JSON under
  `$ACY_STATE_DIR` (else `<user config dir>/acy/sessions/<id>.json`). Injected as
  `Config.LoadState` / `Config.SaveState`.
- `internal/e2e` — the live end-to-end suite: drives the real supervisor (real gate, real
  hook, real claude, real state files) headlessly, on your subscription. `ACY_LIVE=1` only;
  it can never run in CI.
- `internal/cli` — Cobra commands: `run` (the TUI) and the hidden `hook`.
- `internal/alog` — process-wide debug logger (off until `Open`); the `--log` flag drives it.
- `vscode/` — the VS Code extension, a deliberate thin launcher: it resolves the acy binary
  (`acy.binaryPath` setting → `bin/acy` bundled in a platform build → PATH) and runs it *as*
  the terminal shell (no user shell, no quoting, the terminal dies with the supervisor).
  Run settings travel in `.acy.json`, never flags, so CLI and extension can't disagree. The
  decision logic lives vscode-free in `src/launch.ts` / `src/config.ts` and is tested with
  plain `node --test` (`npm test` in `vscode/`); `npm run package` builds an installable
  `.vsix`. One supervisor terminal per window — a second run reveals, never relaunches.

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
- **The viewport's default keymap steals typing.** bubbles `viewport.DefaultKeyMap` binds
  `j/k/d/u/f/b` and space to scrolling, and `Update` forwards key events to both the input and
  the viewport — so typing those letters scrolled the transcript. `transcriptKeyMap()` in
  `update.go` restricts scrolling to the arrows and `PgUp`/`PgDn`. Don't hand the viewport an
  unrestricted keymap.

## Commands

```sh
make run                     # build the latest acy and dogfood it on this repo
go build -o acy .            # build (= make build)
go test ./...                # unit tests (no network; = make test)
go test -race ./...          # what CI runs (= make race)
ACY_LIVE=1 go test ./...     # + live tests: real `claude`, spends a few cents (= make live)
golangci-lint run ./...      # lint (config in .golangci.yml, standard set; = make lint)
gofmt -l .                   # must be empty (= make fmt)
```

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
