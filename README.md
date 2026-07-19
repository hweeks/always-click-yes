# always-click-yes

[![CI](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml/badge.svg)](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml)

A terminal supervisor for long-running [Claude Code](https://claude.com/claude-code)
tasks. You plan a task interactively, **arm** it, and `always-click-yes` approves
each permission prompt after a short, interruptible countdown — then, every time the
run goes idle, it asks the **working session itself** whether the plan is complete
and nudges it back to work until it says so. When it does, you're dropped into a
normal chat with the session to vet what it built.

It exists to solve one problem: *sitting at the keyboard pressing "yes" for the
length of a long task.*

## What this actually is

This is an experiment in total trust, and you should read it as one.

The bet: a human makes contact exactly twice — at the plan, and at the end — and in
between the machine runs unattended. No approvals, no spot-checks, no one reading the
diff as it lands. Every decision in the middle is delegated, **including the decision
about whether the work is done**: the model that did the work grades its own homework,
by ending every reply with a `STATUS:` line that `acy` reads. (An earlier version spawned
an independent judge session per idle check so the worker couldn't self-certify — it was
more honest and burned tokens like a furnace, a fresh uncached context per check. The
trade made here is explicit: self-grading in the session that already has the context,
and the human vet moves to the end, where COMPLETE drops you into a chat with the session
to check its work.)

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
  └─ session captured           ▼ idle: read the turn's STATUS line
                                │  STATUS: DONE ──▶ COMPLETE ──▶ chat & vet the work
                                └▶ STATUS: CONTINUE (or none) ──▶ nudge, back to AUTO-RUN
```

During auto-run the system prompt has the working session end **every reply** with a
sentinel line: `STATUS: DONE` when every step of the plan is complete, `STATUS: CONTINUE`
when work remains. Each time the run goes idle, `acy` reads that line from the turn it
just watched. `DONE` ends at COMPLETE and the composer becomes a normal chat — same
session, full context — for you to vet the result. `CONTINUE` (or a missing line) sends a
nudge back into the **same session**, which is nearly free by comparison: the resumed
context is prompt-cached, and there is no second process to feed the plan to. After 10
auto-nudges without a `DONE`, the loop stops driving itself and preloads a manual
"are we done?" check you send with `Enter`.

Planning and auto-run are backed by two launches of the same session: planning runs over
a read-only tool registry (nothing that can write is even loaded); arming resumes it with
the hook wired in and the default (gated) permission mode.

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

## Local development

Working on `acy` itself? The Makefile keeps the dogfood loop one command away:

```sh
make run     # build the latest acy from this checkout and launch it, right here
```

That builds a fresh binary from your working tree and starts it supervising the repo
you're standing in — which for this project is the point: `acy` is developed by running
`acy` on itself (see `AGENTS.md`). Other targets: `make build` (just the binary),
`make test` (unit tests), `make race` (what CI runs), `make live` (the paid live suite,
`ACY_LIVE=1`), `make lint`, and `make clean`.

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
| Idle (round cap hit) | `Enter` | send the preloaded manual "are we done?" check |
| Complete | type + `Enter` | chat with the session to vet the work |
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
| `/resume [id]` | resume a prior session for this repo — a picker if no id, direct if given. Restores the transcript **and the run**: a session you armed comes back armed and keeps going (see [Resuming](#resuming)) |
| `/model <name>` | set the model for the next launched/resumed session |
| `/clear` | clear the transcript view |
| `/log` | show the debug-log path |
| `/quit` | quit (same as `Ctrl+C`) |

## Resuming

A long unattended run is exactly the thing you can't babysit — so it's exactly the thing
that gets interrupted. Close the terminal, sleep the laptop, hit `Ctrl+C` by accident, and
the work is stranded halfway.

```sh
acy --continue          # pick the run back up where it stopped
acy --resume <id>       # or name the session
```

You come back to the transcript you were looking at, in the phase you were in. If you'd
armed the run, it comes back **armed**: no keys, no re-approval, no re-planning. It doesn't
restart the task — it reads the last thing the session said (a replayed `STATUS: DONE`
completes on the spot) and otherwise asks the session where things stand, which picks the
work back up.

That works because the two halves are restored from two places. The conversation is
Claude's: `acy` replays the transcript Claude already keeps under `~/.claude/projects`.
Everything else — which phase you were in, the plan being worked to, the auto-round count,
the running cost — is `acy`'s own, and it isn't in Claude's file, so `acy` keeps a small
snapshot per session (a few hundred bytes, written atomically at every transition) under
`$ACY_STATE_DIR`, defaulting to your OS config dir (`~/Library/Application Support/acy` on
macOS, `~/.config/acy` on Linux).

Sessions `acy` never supervised — a bare `claude` run — still resume; you just get the
conversation back, with nothing to restore. The `/resume` picker marks the difference:
rows `acy` drove show `[AUTO-RUN · 3 rounds · $1.23]`.

Two things worth knowing. The transcript view is capped at the most recent 200 entries
(Claude still has the whole thing — only the re-render is bounded, and the elided head
says so). And a finished run resumes as a **chat**, not an auto-run: there's nothing left
to drive, but the cost carries over, and `Ctrl+G` re-arms if you want more.

## Flags

- `--resume <id>` / `-c`, `--continue` — restore a prior run: its transcript, phase, plan,
  round count and cost. `--continue` takes the most recent run **`acy` supervised** in this
  directory, so it can never land on a stray `claude` session. See [Resuming](#resuming).
- `--model` — model alias/name (default: Claude's default)
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
- `internal/ui` — the Bubble Tea model: transcript, countdown, phase machine, the
  STATUS-sentinel completion loop + manual done-check fallback, slash commands, and
  the resume / AskUserQuestion pickers.
- `internal/session` — reads claude's `~/.claude/projects/<slug>/*.jsonl` transcripts:
  lists them for the `/resume` picker, and replays one back into the transcript view.
- `internal/state` — `acy`'s own snapshot of a run (phase, plan, rounds, cost) — the
  part of a session claude's transcript doesn't record.
- `internal/e2e` — the live end-to-end suite (see below).
- `internal/cli` — Cobra commands (`run`, hidden `hook`).

## Tests

```sh
go test ./...                 # unit tests — no network, no spend, no claude
```

### The live suite

Unit tests can only prove `acy` agrees with itself. The live suite proves it agrees with
Claude — it drives the **real** supervisor (real gate socket, real `PreToolUse` hook
subprocess, real `claude` sessions, real state files) with no terminal, and asserts on
things a model can't talk its way out of: a file exists on disk, a phase changed, a prompt
was or wasn't sent.

```sh
ACY_LIVE=1 go test ./... -timeout 30m            # everything, including the e2e suite
ACY_LIVE=1 go test ./internal/e2e/ -v -timeout 20m   # just the end-to-end scenarios
ACY_LIVE=1 go test ./internal/e2e/ -run TestE2EResume -v -timeout 15m
```

It **runs on your subscription and spends real money** (cents, not dollars) and takes real
minutes, which is why it is opt-in and can never run in CI. Every test gets a scratch
project directory and a scratch snapshot directory, so it can't touch your work.

What it covers:

| Test | What it proves |
|------|----------------|
| `TestE2EPlanArmAutoApproveComplete` | the whole premise: plan → arm → tools auto-approve → work done → the session says DONE, with a file on disk as the evidence |
| `TestE2EVetoBlocksATool` | `s` actually stops a tool. The most important key on the board, and the one you'll never press |
| `TestE2EResumeAnArmedRunAfterACrash` | kill an armed run mid-flight, `--continue`, and watch it finish unattended |
| `TestE2EResumeACompletedRunLandsInPlan` | a finished run comes back as a chat, with its cost intact |
| `TestE2EResumeASessionWithNoSnapshot` | a session `acy` never drove still resumes |

The narrower live tests (`ACY_LIVE=1` in `internal/driver`, `internal/gate`,
`internal/ui`) probe single seams: the stream-json wire format, the hook chain, and
whether `AskUserQuestion` has become real yet.
