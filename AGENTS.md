# AGENTS.md — context for AI agents working on always-click-yes

Read this first. It captures what took real probing to learn so you don't have to
rediscover it.

## What this is

A Go TUI (Cobra + Bubble Tea + Lipgloss) that supervises a **Claude Code** session
for long-running tasks. Flow:

1. **PLAN** — you chat with Claude (`--permission-mode plan`, no hooks) to build a plan.
2. **Ctrl+G arms it** — a fresh `claude` process **resumes the same session** with the
   PreToolUse hook wired in and `--permission-mode default`, and gets a kickoff message.
3. **AUTO-RUN** — every gated tool triggers a countdown; it auto-approves after N seconds
   unless you veto (`s`), approve now (`a`), or pause (`p`). `Esc` interrupts a running turn.
4. **Idle** → an **independent one-shot `claude` session** (fresh session, no hooks, tools
   disabled — `internal/judge`) is handed the approved plan + the working session's last
   message and returns `STATUS: DONE` / `STATUS: CONTINUE`. DONE → COMPLETE; CONTINUE
   auto-nudges the working session (up to `maxAutoRounds`) so the run finishes hands-free.
   If the judge errors, times out, or is inconclusive, it falls back to the manual
   preloaded "are we done?" check (press Enter to send it into the working session).

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
  gutter blocks), `phase.go` (phase machine, judge dispatch + done-check fallback),
  `commands.go` (slash commands + resume picker), `ask.go` (AskUserQuestion panel),
  `gate.go` (countdown).
- `internal/session` — lists resumable sessions for `/resume` from claude's
  `~/.claude/projects/<slug>/*.jsonl` transcripts (slug = cwd with `/`→`-`). Injected as
  `Config.Sessions`, so tests supply a fake.
- `internal/judge` — runs an independent one-shot `claude` session (via `driver`, tools
  disabled) that judges plan completion from the plan + last message. Injected into the UI
  as `Config.Judge`, so tests swap in a fake verdict.
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
- **`AskUserQuestion`** arrives as an assistant `tool_use` block (input:
  `{questions:[{header,question,multiSelect,options:[{label,description}]}]}`) and the turn
  **blocks** — no `result` event — until you answer it. Answer via `driver.SendToolResult`,
  which injects a user message whose content is a `tool_result` block referencing the
  `tool_use_id`. Note: this can't be spiked from inside an agent harness (a nested `claude`
  inherits an altered/deferred tool registry and won't offer `AskUserQuestion`); verify from a
  real terminal. Detection is name-based and harmless if the tool never appears.
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
- **The viewport's default keymap steals typing.** bubbles `viewport.DefaultKeyMap` binds
  `j/k/d/u/f/b` and space to scrolling, and `Update` forwards key events to both the input and
  the viewport — so typing those letters scrolled the transcript. `transcriptKeyMap()` in
  `update.go` restricts scrolling to the arrows and `PgUp`/`PgDn`. Don't hand the viewport an
  unrestricted keymap.

## Commands

```sh
go build -o acy .            # build
go test ./...                # unit tests (no network)
go test -race ./...          # what CI runs
ACY_LIVE=1 go test ./...     # + live tests: real `claude`, spends a few cents
golangci-lint run ./...      # lint (config in .golangci.yml, standard set)
gofmt -l .                   # must be empty
```

Live tests need the `claude` CLI on PATH and auth. CI skips them.

## Conventions

- Keep the debug log useful: new subsystems should `alog.Printf`/`alog.Raw` their key events.
- UI transcript is structured `entry` values rendered in `render.go` — add an `ekind` rather
  than pre-styling strings, so entries re-render correctly on resize.
- End commit messages with the Co-Authored-By trailer.
