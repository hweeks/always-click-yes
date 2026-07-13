# always-click-yes

[![CI](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml/badge.svg)](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml)

A terminal supervisor for long-running [Claude Code](https://claude.com/claude-code)
tasks. You plan a task interactively, **arm** it, and `always-click-yes` approves
each permission prompt after a short, interruptible countdown — then, when the run
goes idle, an **independent Claude session** judges whether the plan is complete and
the run loops itself to the finish, hands-free.

It exists to solve one problem: *sitting at the keyboard pressing "yes" for the
length of a long task.*

## What this actually is

This is an experiment in total trust, and you should read it as one.

The bet: a human makes contact exactly once — at the plan — and after that the machine
runs unattended to the end. No approvals, no spot-checks, no one reading the diff as it
lands. The only human judgment in the loop is spent up front, on *what* to build. Every
decision after that is delegated, including the decision about whether the work is done,
which is why the completion judge is a **separate Claude session**: the model that did the
work does not get to grade its own homework. That's not a safety feature so much as an
acknowledgment that we removed the person who used to do the grading.

Call the output what it is. Unsupervised codegen at volume is slop, and this tool is a
slop pump with a countdown timer. It is built to find out what happens when you stop
pretending otherwise and just let the thing run — and to be honest about the failure
modes when it does, rather than quietly cleaning them up before anyone notices.

It is also a sincere accelerant. It really will hand you back the hours you currently
spend pressing `y`, and you really can go do something else with them. That is the pitch,
and it is worth sitting with how good the pitch sounds, because it is the same pitch
pointed at a future where the only thing software engineering is understood to produce is
the relentless release of new features — at any human cost. This tool is what that future
feels like from the inside: pleasant, fast, and entirely hands-off. Whether that reads as
a promise or a warning is left, deliberately, to the reader.

The countdown is interruptible and the veto key is `s`. It is the most important key on
the board and you will almost never press it. That is the whole finding.

## How it works

`always-click-yes` drives `claude` in stream-json mode (the same channel the
official SDKs use) and renders the conversation itself with
[Bubble Tea](https://github.com/charmbracelet/bubbletea) + Lipgloss. Per-tool
approval is done with a `PreToolUse` hook: the tool wants to run → the hook blocks
→ the TUI shows a countdown → it auto-approves (or you veto).

```
PLAN ──▶ (Ctrl+G arms) ──▶ AUTO-RUN ──▶ per-tool 30s countdown ──▶ auto-approve
  │  interactive                │  claude works        ▲   │(veto / pause / allow)
  │  chat & plan                │                      └───┘
  └─ session captured           ▼ idle
                    independent judge session
                    (plan + last message → verdict)
                                │  STATUS: DONE ──▶ COMPLETE
                                └▶ STATUS: CONTINUE ──▶ nudge back to AUTO-RUN
```

When the run goes idle, a separate one-shot `claude` session — not the one that did
the work, so it can't grade its own homework — reads the approved plan and the
working session's final message and returns a verdict. `DONE` ends at COMPLETE;
`CONTINUE` automatically nudges the working session to keep going (up to a round
cap). If that judge errors, times out, or is unclear, it falls back to a manual
"are we done?" check you send with `Enter`.

The two main phases are backed by two processes: planning runs in
`--permission-mode plan` (nothing executes); arming resumes the **same session**
with the hook wired in and the default (gated) permission mode. Each idle check
spawns one more short-lived judge process.

## Install

**Prerequisites:** Go 1.26+ and the [Claude Code](https://claude.com/claude-code) CLI
(`claude`) on your `PATH` and authenticated (`claude auth`).

**Prebuilt binary** — grab the tarball for your platform from the
[latest release](https://github.com/hweeks/always-click-yes/releases/latest):

```sh
tar xzf acy_v*_darwin_arm64.tar.gz
sudo mv acy /usr/local/bin/
```

**With `go install`** (puts `acy` in `$(go env GOPATH)/bin` — add that to your `PATH`):

```sh
go install github.com/hweeks/always-click-yes@latest   # newest tagged release
go install github.com/hweeks/always-click-yes@v0.1.0   # or pin a version
mv "$(go env GOPATH)/bin/always-click-yes" "$(go env GOPATH)/bin/acy"  # optional: shorter name
```

**From source:**

```sh
git clone https://github.com/hweeks/always-click-yes.git
cd always-click-yes
go build -o acy .
# optionally: sudo mv acy /usr/local/bin/
```

`acy --version` reports what you're running — a tag (`v0.1.0`) for a released build, or a
`v0.0.0-<date>-<sha>` pseudo-version (with `+dirty` for uncommitted changes) when built
from a checkout. Include it in bug reports.

## Run

```sh
acy run                    # in the project directory you want Claude to work in
acy run --model opus --countdown 20s
```

## Keys

| Phase | Key | Action |
|-------|-----|--------|
| Plan | type + `Enter` | talk to Claude, build the plan |
| Plan | `Ctrl+J` | insert a newline without sending |
| Plan | `Ctrl+G` | **arm** — start auto-run on the current session |
| Auto-run (gate pending) | `s` | stop / veto this tool |
| Auto-run (gate pending) | `a` | approve this tool now |
| Auto-run (gate pending) | `p` | pause / resume the countdown |
| Auto-run (working) | `Esc` | interject — interrupt the turn, then type to redirect |
| Idle (judge fell back) | `Enter` | send the preloaded manual "are we done?" check |
| anywhere | `↑`/`↓`, `PgUp`/`PgDn` | scroll the transcript |
| anywhere | `Ctrl+C` | quit |

Scrolling is bound to the arrows and page keys only, so typing a message never
scrolls the transcript out from under you. The message box grows as you type (up
to 8 rows) and the transcript gives up the space.

When Claude presents a plan (via `ExitPlanMode`) it's shown in a boxed
**📋 PROPOSED PLAN** with a `▶ Press Ctrl+G to arm` prompt — that keypress is how
you "accept" the plan and start the auto-run.

When Claude asks a multiple-choice question (via `AskUserQuestion`), an inline
picker appears — `↑`/`↓` to move, `Space` to toggle (multi-select), `Enter` to
answer, `Esc` to skip — and the answer is sent straight back into the turn.

> **⚠ Both of the above are currently dormant.** Measured against `claude` 2.1.207:
> the `-p` (headless) mode `acy` drives does **not** offer `AskUserQuestion` or
> `ExitPlanMode` — its `system/init` tool registry contains neither, in any
> `--permission-mode`, with or without `--allowedTools`. So Claude never emits the
> tool call, the boxed plan never renders, and the question picker never opens.
> The plan still arrives — as ordinary assistant text — and `Ctrl+G` still arms.
> `internal/ui/ask_live_test.go` probes this live and will start passing if it
> changes. Making the picker real means exposing the question as an **MCP** tool
> (`mcp__acy__…`), which *does* land in the registry; the UI already handles
> MCP-prefixed names.

## Slash commands

Type these in the message box (they're handled by `acy`, not forwarded to Claude):

| Command | Action |
|---------|--------|
| `/help` | show the command + key reference overlay |
| `/resume [id]` | resume a prior session for this repo — a picker if no id, direct if given; lands back in the chat so you can keep planning, then `Ctrl+G` to arm |
| `/model <name>` | set the model for the next launched/resumed session |
| `/clear` | clear the transcript view |
| `/log` | show the debug-log path |
| `/quit` | quit (same as `Ctrl+C`) |

## Flags

- `--model` — model alias/name (default: Claude's default)
- `--judge-model` — model for the independent completion judge (default: `--model`)
- `--countdown` — auto-approve delay per gated tool (default `30s`)
- `--max-lines` — max lines shown per tool call/result/thinking block before a
  `… +N more lines` footer (default `10`)
- `--claude-bin` — path to the `claude` binary (default `claude`)
- `--plan-tools` — tools pre-approved during **plan mode** via `--allowedTools`, as exact
  names (default `Monitor,AskUserQuestion`). Plan mode refuses non-read-only tools and has
  no gate wired in, so a tool that isn't listed here simply never runs while you're
  planning. Use exact tool names, MCP ones included (e.g. `mcp__<server>__Monitor`).
  `AskUserQuestion` is kept in the default set for the day it works, but it is **inert
  today** — `claude -p` has no such tool to allowlist (see the note above).
- `--use-api-key` — bill `ANTHROPIC_API_KEY` instead of your claude.ai login. By
  default the key is **stripped** from the environment `claude` runs in: a key
  merely sitting in your shell silently takes precedence over the login, and
  headless `claude -p` never shows the interactive "use this API key?" prompt, so
  every run would quietly bill the API account. The header and the final tally say
  which account paid (`subscription` vs `API`), read from claude's own init event.
- `--log` — debug log file (default `acy-debug.log`); captures the full stream —
  every event received (`RX`), every message sent (`TX`), gate decisions, and phase
  transitions. Set to `""` to disable. `tail -f acy-debug.log` to watch it live.

## Layout

- `internal/driver` — the `claude` subprocess: stream-json args, stdin message
  injection, NDJSON event decoding, and `Interrupt()`.
- `internal/gate` — the permission bridge: a unix-socket server (supervisor) and
  the `hook` client, with allow/deny decisions.
- `internal/config` — generates the `--settings` file that registers the hook.
- `internal/judge` — the independent one-shot completion judge (plan + last
  message → `STATUS: DONE`/`CONTINUE`).
- `internal/ui` — the Bubble Tea model: transcript, countdown, phase machine,
  judge dispatch + manual done-check fallback, slash commands, and the resume /
  AskUserQuestion pickers.
- `internal/session` — lists resumable sessions for the `/resume` picker by
  reading claude's `~/.claude/projects/<slug>/*.jsonl` transcripts.
- `internal/cli` — Cobra commands (`run`, hidden `hook`).

## Tests

```sh
go test ./...                 # unit tests (no network)
ACY_LIVE=1 go test ./...      # also runs the live tests that spend a few cents:
                              #   driver stream, gate hook chain, plan->run handoff, interrupt
```
