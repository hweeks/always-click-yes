# always-click-yes

[![CI](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml/badge.svg)](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml)

A terminal supervisor for long-running [Claude Code](https://claude.com/claude-code)
tasks. You plan a task interactively, **arm** it, and `always-click-yes` approves
each permission prompt after a short, interruptible countdown — then, when the run
goes idle, it asks Claude whether the plan is complete and loops until it is.

It exists to solve one problem: *sitting at the keyboard pressing "yes" for the
length of a long task.*

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
                          "are we done?"  (you press Enter to send)
                                │  STATUS: DONE ──▶ COMPLETE
                                └▶ STATUS: CONTINUE ──▶ back to AUTO-RUN
```

Two processes back the two phases: planning runs in `--permission-mode plan`
(nothing executes); arming resumes the **same session** with the hook wired in and
the default (gated) permission mode.

## Install

**Prerequisites:** Go 1.26+ and the [Claude Code](https://claude.com/claude-code) CLI
(`claude`) on your `PATH` and authenticated (`claude auth`).

**With `go install`** (puts `acy` in `$(go env GOPATH)/bin` — add that to your `PATH`):

```sh
go install github.com/hweeks/always-click-yes@latest
mv "$(go env GOPATH)/bin/always-click-yes" "$(go env GOPATH)/bin/acy"  # optional: shorter name
```

**From source:**

```sh
git clone https://github.com/hweeks/always-click-yes.git
cd always-click-yes
go build -o acy .
# optionally: sudo mv acy /usr/local/bin/
```

## Run

```sh
acy run                    # in the project directory you want Claude to work in
acy run --model opus --countdown 20s
```

## Keys

| Phase | Key | Action |
|-------|-----|--------|
| Plan | type + `Enter` | talk to Claude, build the plan |
| Plan | `Ctrl+G` | **arm** — start auto-run on the current session |
| Auto-run (gate pending) | `s` | stop / veto this tool |
| Auto-run (gate pending) | `a` | approve this tool now |
| Auto-run (gate pending) | `p` | pause / resume the countdown |
| Auto-run (working) | `Esc` | interject — interrupt the turn, then type to redirect |
| Idle | `Enter` | send the preloaded "are we done?" check |
| anywhere | `Ctrl+C` | quit |

When Claude presents a plan (via `ExitPlanMode`) it's shown in a boxed
**📋 PROPOSED PLAN** with a `▶ Press Ctrl+G to arm` prompt — that keypress is how
you "accept" the plan and start the auto-run.

## Flags

- `--model` — model alias/name (default: Claude's default)
- `--countdown` — auto-approve delay per gated tool (default `30s`)
- `--claude-bin` — path to the `claude` binary (default `claude`)
- `--log` — debug log file (default `acy-debug.log`); captures the full stream —
  every event received (`RX`), every message sent (`TX`), gate decisions, and phase
  transitions. Set to `""` to disable. `tail -f acy-debug.log` to watch it live.

## Layout

- `internal/driver` — the `claude` subprocess: stream-json args, stdin message
  injection, NDJSON event decoding, and `Interrupt()`.
- `internal/gate` — the permission bridge: a unix-socket server (supervisor) and
  the `hook` client, with allow/deny decisions.
- `internal/config` — generates the `--settings` file that registers the hook.
- `internal/ui` — the Bubble Tea model: transcript, countdown, phase machine,
  done-check loop.
- `internal/cli` — Cobra commands (`run`, hidden `hook`).

## Tests

```sh
go test ./...                 # unit tests (no network)
ACY_LIVE=1 go test ./...      # also runs the live tests that spend a few cents:
                              #   driver stream, gate hook chain, plan->run handoff, interrupt
```
