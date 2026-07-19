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
for long-running tasks. Flow:

1. **PLAN** — you chat with Claude over a read-only `--tools` registry to build a plan.
2. **Ctrl+G arms it** — a fresh `claude` process **resumes the same session** with the
   PreToolUse hook wired in and `--permission-mode default`, and gets a kickoff message.
3. **AUTO-RUN** — every gated tool triggers a countdown; it auto-approves after N seconds
   unless you veto (`s`), approve now (`a`), or pause (`p`). `Esc` interrupts a running turn.
4. **Idle** → the auto-run system prompt has the working session end **every reply** with
   `STATUS: DONE` / `STATUS: CONTINUE`, and each turn end reads that sentinel from the
   turn's own text (`parseVerdict` in `internal/ui/phase.go`). DONE → COMPLETE, which is a
   normal chat again — the user vets the work in the session that did it. CONTINUE (or no
   sentinel) auto-nudges the **same session** (up to `maxAutoRounds`) so the run finishes
   hands-free; the resumed context is prompt-cached, so each check costs a single cheap
   turn rather than a fresh judge process. Past the cap it falls back to the manual
   preloaded "are we done?" check (press Enter to send it into the working session).
   (An independent per-check judge session existed once — `internal/judge`, removed — and
   was the tool's biggest token sink: every idle check paid a full uncached context.)

## Architecture (package map)

- `internal/driver` — owns the long-lived `claude` subprocess. Builds stream-json args,
  injects user messages on stdin, decodes the NDJSON event stream (`events.go`), and can
  `Interrupt()`. Logs raw RX/TX to `alog`.
- `internal/gate` — the permission bridge. `server.go` listens on a unix socket; the `hook`
  subcommand (`client.go`) connects, forwards the tool request, and blocks for an
  allow/deny `Decision`. `Pending.Resolve` answers it.
- `internal/config` — generates the `--settings` JSON that registers the PreToolUse hook
  pointing at `<self> hook --socket <path>` (the binary is its own hook).
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

## Hard-won facts about `claude` stream-json (verified live, ~v2.1.207)

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
- **`onDriverReady` clears `turnText` — except for a resumed auto-run.** That is the one launch
  where the replay deliberately left the final assistant turn there, because it is what the
  completion check reads for the STATUS sentinel. Clearing it would erase a replayed
  `STATUS: DONE` and nudge a finished run back into motion.
- **A resume takes its phase immediately, not when the driver lands.** Launching claude takes
  a second or two; until the phase moves, a restored run looks like a plan session with a
  session id — exactly what `Ctrl+G` arms from. Arming in that window launches a *second*
  process for the same session and kicks off work that is already half done.
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
feat: let the working session report completion   -> minor bump (0.2.0)
fix: strip ANTHROPIC_API_KEY from the child    -> patch bump (0.1.1)
feat!: rename --countdown to --delay           -> major bump (see below)
docs|test|chore|refactor|perf: ...             -> no release on its own
```

A `!` after the type (or a `BREAKING CHANGE:` footer) marks a breaking change. While the
project is pre-1.0 that bumps the *minor*, not the major.

Releases are automatic: merging to `main` makes release-please open or update a
`chore(release): x.y.z` PR. Nothing ships until you merge that PR — doing so tags the
commit, writes `CHANGELOG.md`, and `.github/workflows/release.yml` attaches the
cross-compiled binaries. Don't tag by hand; `.release-please-manifest.json` tracks the
current version and hand-tagging desyncs it.
