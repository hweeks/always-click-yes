# always-click-yes

[![CI](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml/badge.svg)](https://github.com/hweeks/always-click-yes/actions/workflows/ci.yml)

A terminal supervisor for long-running [Claude Code](https://claude.com/claude-code)
tasks. You plan a task interactively, **arm** it, and `always-click-yes` approves
each permission prompt after a short, interruptible countdown. The session you talk
to can read your codebase but cannot change it: it delegates each piece of work to a
**fresh `claude` process** that does the job, reports back in a few hundred tokens,
and then disappears. When the work is done it hands you back a normal chat with the
session, to vet what it built.

It exists to solve two problems: *sitting at the keyboard pressing "yes" for the
length of a long task*, and *paying for that task twice a minute.*

## Why delegation

One real run, measured from acy's own debug log:

| | tokens |
|---|---|
| cache_read | **8,697,690** |
| cache_creation | 454,900 |
| output | 75,011 |
| **cost** | **$16.04** |

98% of the token volume was re-reading a context that only ever grew. A single
session carrying an entire job re-presents every file it has ever read on every turn
that follows — so the bill scales with the *square* of the work, not the work.

So the conversation you hold stops accumulating the work. The supervising session
keeps your chat and one compact report per task; the reading, editing and testing
happen in child processes whose context evaporates when they exit. `/tokens` shows
the split, which is the point: the parent's line should stay flat while the
children's climbs.

## What this actually is

This is an experiment in total trust, and you should read it as one.

The bet: a human makes contact exactly twice — at the plan, and at the end — and in
between the machine runs unattended. No approvals, no spot-checks, no one reading the
diff as it lands. Every decision in the middle is delegated, **including the decision
about whether the work is done**: the supervising session declares the run finished by
calling a tool, and the human vet moves to the end, where COMPLETE drops you into a chat
with that session to check its work.

(Two earlier designs are worth naming, because both failed in instructive ways. The first
spawned an independent judge session per idle check so the worker could not self-certify —
honest, and it burned tokens like a furnace: a fresh uncached context every check. The
second had the working session end every reply with a `STATUS:` line that acy read, and
nudged it onward up to ten times — cheaper per check, but each nudge re-billed the whole
accumulated conversation, and the substring match would fire if a reply merely *mentioned*
the sentinel. Now the session ends the run with a tool call, which cannot be accidentally
matched and cannot be missed — only not made, and the answer to that is a human, not
another billed turn.)

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

The countdown is interruptible and the veto key is `Ctrl+X`. It is the most important key
on the board and you will almost never press it. That is the whole finding.

## How it works

`always-click-yes` drives `claude` in stream-json mode (the same channel the
official SDKs use) and renders the conversation itself with
[Bubble Tea](https://github.com/charmbracelet/bubbletea) v2 + Lipgloss v2. Per-tool
approval is done with a `PreToolUse` hook: the tool wants to run → the hook blocks
→ the TUI shows a countdown → it auto-approves (or you veto).

```
  SUPERVISING SESSION  (one process, --tools Read,Grep,Glob)
  ├─ PLAN ──▶ (Ctrl+G arms, in place — nothing relaunches) ──▶ AUTO-RUN
  │
  ├─ Dispatch{task} ─────▶ CHILD PROCESS  (fresh session, full toolset)
  │                        edits, runs tests, reads 150k of context
  │  ◀── report ~300 tok ──┘ …then exits, and all of that context is gone
  │                          (each gated tool: 30s countdown ──▶ auto-approve)
  ├─ Dispatch{next task} ─▶ …
  │
  └─ Finish ──▶ COMPLETE ──▶ chat & vet the work
```

The session you talk to has three tools — `Read`, `Grep`, `Glob` — and that is not a
permission setting: `Write`, `Edit` and `Bash` are not in its registry at all, which is
a guarantee no prompt can talk its way past. It is also why its system prompt never says
"do not implement": there is nothing to implement with.

Each child is a separate `claude` process with its own session id, the same `PreToolUse`
gate, a `--json-schema` its report is validated against, and an optional spend ceiling.
It does the work unwatched and returns a structured report — outcome, summary, files
changed, checks actually run. That report is the only thing that enters your
conversation. Whatever the child read to produce it dies with the process.

Arming does not relaunch anything; it flips a phase on the session already in front of
you. Dispatch simply stops refusing.

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

### `acy serve` — the same run, without a terminal

```sh
acy serve                  # → {"url":"http://127.0.0.1:54321","token":"8f3c…"}
acy serve --port 7777 --model opus
```

`serve` drives the **identical supervisor** `run` does — the same gate, the same
PreToolUse hook, the same dispatched children, the same `.acy.json` and the same
run flags — and puts it on HTTP instead of on a screen: a frame projection, an
action endpoint, and a Server-Sent Events stream a client renders from. It is for
front ends that are not a terminal; the [VS Code panel](#vs-code) is the one that
exists. `acy run` and the TUI are unchanged and unaffected.

It binds **127.0.0.1 only**, requires a bearer token on every `/api/` request,
and prints one line of JSON to stdout as soon as the listener is up — nothing
else ever precedes it there — so a parent process can parse it and connect. The
routes, status codes, SSE framing and CORS rules are specified in
[`docs/webui-protocol.md`](docs/webui-protocol.md).

## VS Code

The `vscode/` extension gives you the same supervisor two ways.

**In a terminal — the default.** **ACY: Plan & Run**, **ACY: Continue Last Run**,
and a status-bar button run `acy` as an integrated terminal's shell, so you get
the TUI verbatim, with one supervisor terminal per window (a second run reveals
it, never double-launches).

**In a panel.** **ACY: Open Panel** starts `acy serve` — the identical
supervisor, headless — and renders it in a webview: the same transcript, gates,
countdowns, queue and composer, over HTTP rather than a terminal. One panel and
one supervisor per workspace folder; closing the tab stops it.

The panel works, and it is not the default yet: `acy.useTerminal` defaults to
`true` because the panel is still wearing placeholder styling while its visual
design is settled. Turn that setting off and **ACY: Plan & Run** opens the panel
instead. Nothing about the terminal path changes either way.

Install from the VS Code Marketplace (`ext install hweeks.always-click-yes`),
which hands you the package built for your platform.
Each release also attaches those `.vsix` packages with the acy binary bundled
(`darwin-arm64`, `darwin-x64`, `linux-x64`, `linux-arm64`, `win32-x64` — the
last experimental and untested at runtime) plus a `universal` one that uses
`acy` from your `PATH`, installable via **Extensions: Install from VSIX…**. See
[`vscode/README.md`](vscode/README.md).

## Configuration file: `.acy.json`

A project can pin its run settings in a `.acy.json` at its root, so a bare
`acy run` — or the VS Code extension, which passes no flags at all — needs no
arguments:

```json
{
  "model": "opus",
  "claudeBin": "claude",
  "countdown": "20s",
  "log": "acy-debug.log",
  "maxLines": 15,
  "planTools": ["Read", "Grep", "Glob"],
  "provider": "anthropic",
  "childModel": "sonnet",
  "childEffort": "medium",
  "taskBudget": 2.50,
  "useApiKey": false
}
```

### Other model providers

`acy` always keeps **Claude Code** as the runtime and supervisor protocol. It
can, however, direct Claude Code to a different model backend. For hosted
OpenAI-compatible providers, acy starts a private, loopback-only
[LiteLLM](https://docs.litellm.ai/) sidecar that translates Claude Code's
Anthropic Messages requests. For a local vLLM server, it connects directly to
vLLM's Messages endpoint.

Install LiteLLM once for hosted providers:

```sh
pipx install 'litellm[proxy]'
```

Then put the **non-secret** selection in `.acy.json` and export the provider
key in the shell that launches acy. Do not put keys in `.acy.json`: it is meant
to be committed with the project.

| `provider` | Required environment variable | Default model |
|---|---|---|
| `anthropic` | Claude login, or `ANTHROPIC_API_KEY` with `useApiKey: true` | Claude Code's default |
| `openai` | `OPENAI_API_KEY` | `gpt-4.1` |
| `cerebras` | `CEREBRAS_API_KEY` | `llama-3.3-70b` |
| `fireworks` | `FIREWORKS_API_KEY` | `accounts/fireworks/models/llama-v3p3-70b-instruct` |
| `openrouter` | `OPENROUTER_API_KEY` | `anthropic/claude-sonnet-4` |
| `vllm` | none by default | supply your local model name |

An OpenAI example:

```json
{
  "provider": "openai",
  "model": "gpt-4.1",
  "childModel": "gpt-4.1",
  "gatewayBin": "litellm"
}
```

`model` selects the read-only supervising session; `childModel` selects the
disposable workers that edit and run tests. acy adds both aliases to the local
LiteLLM config, so they may differ. If `childModel` is omitted for a hosted
provider, acy uses `model` rather than the normal Claude `sonnet` default.
`gatewayBin` is optional and defaults to `litellm` on `PATH`.

The equivalent command-line form is:

```sh
OPENAI_API_KEY=... acy run --provider openai --model gpt-4.1 --child-model gpt-4.1
CEREBRAS_API_KEY=... acy run --provider cerebras
FIREWORKS_API_KEY=... acy run --provider fireworks
OPENROUTER_API_KEY=... acy run --provider openrouter
```

For vLLM, run an Anthropic-Messages-compatible vLLM server locally and configure
its endpoint explicitly when it is not on the default port:

```json
{
  "provider": "vllm",
  "gatewayUrl": "http://127.0.0.1:8000",
  "model": "your-tool-capable-model",
  "childModel": "your-tool-capable-model"
}
```

`gatewayUrl` defaults to `http://127.0.0.1:8000`. It is only used by `vllm`;
hosted providers get a fresh random loopback port each run. In the LiteLLM path,
the upstream key exists only in the sidecar process. Claude Code and its child
processes receive a random local gateway token instead, so a child with Bash
cannot read `OPENAI_API_KEY`, `CEREBRAS_API_KEY`, `FIREWORKS_API_KEY`, or
`OPENROUTER_API_KEY` from its environment.

Non-Anthropic providers are experimental. Start with a small supervised task
and confirm streaming and tool calls work for the exact model before trusting an
unattended editing run. The project skill
[`acy-provider-setup`](.claude/skills/acy-provider-setup/SKILL.md) provides the
same setup and troubleshooting checklist for an agent working in the repository.

Every key is optional and maps to the flag of the same meaning; `countdown` is
a Go duration string, and an explicit `"log": ""` disables logging. Precedence
is defaults < `.acy.json` < explicit flags. Parsing is strict on purpose: an
unknown key, a bare-number duration, or malformed JSON aborts the run rather
than silently falling back to defaults the project tried to override. The
transcript's opening lines say which file the settings came from.

## Arch mode (experimental)

`acy arch` is `acy run` for a whole fleet of machines instead of one checkout.
The parent session becomes the **architect**: it still only reads the
codebase (`Read`/`Grep`/`Glob`), but once armed it delegates whole tickets to
**engineers** — full `acy` instances running unattended on the hosts your
fleet config names — instead of local children. Each engineer plans its own
subtasks in its own git worktree and finishes by opening a PR; the architect's
loop is launch up to capacity, `Await` the next event, react, repeat.

It requires a `"fleet"` section in `.acy.json`:

```json
{
  "fleet": {
    "baseBranch": "main",
    "engineerModel": "sonnet",
    "engineerBudgetUSD": 15,
    "hosts": [
      { "name": "local" },
      { "name": "box2", "ssh": "you@box2.example.com", "repoPath": "/home/you/proj", "maxEngineers": 2 }
    ]
  }
}
```

A host with no `ssh` runs engineers on this machine; anything else reaches the
target over ssh. Check every configured host before trusting a run to it:

```sh
acy fleet doctor              # ssh, acy, claude, gh, git and state-dir health, per host
acy arch                      # plan, arm, and the fleet takes it from there
acy engineer tail <id>        # replay + follow one engineer's journal by hand
```

**Read this before pointing it at real hosts.** An engineer is an unattended
agent with `Bash` on whatever machine it runs, auto-approving its own tool
calls on the same countdown `acy run` uses — nothing is watching it between
launch and its result or a question. Detached, it keeps working with nobody
connected at all; a `deadmanHours` ceiling is what bounds an orphaned engineer
if nobody ever comes back for it. Only point `hosts` at machines and repos
you'd trust an unsupervised process on, and reach them with **key-only SSH in
`BatchMode`** — no interactive password or host-key prompt an engineer could
get stuck behind unattended.

The full operator's guide — host preparation, every fleet config field,
budgets, and resuming after a crash — is
[`docs/arch-mode.md`](docs/arch-mode.md). The wire format between the
architect and its engineers is specced in
[`docs/engineer-protocol.md`](docs/engineer-protocol.md).

## Local development

Working on `acy` itself? The Makefile keeps the dogfood loop one command away:

```sh
make run     # build the latest acy from this checkout and launch it, right here
```

That builds a fresh binary from your working tree and starts it supervising the repo
you're standing in — which for this project is the point: `acy` is developed by running
`acy` on itself (see `AGENTS.md`). Other targets: `make build` (just the binary),
`make arch` (same dogfood loop, in arch mode — requires a "fleet" section in `.acy.json`),
`make test` (unit tests), `make race` (what CI runs), `make live` (the paid live suite,
`ACY_LIVE=1`), `make lint`, and `make clean`.

## Keys

| Phase | Key | Action |
|-------|-----|--------|
| Plan | type + `Enter` | talk to Claude, build the plan |
| anywhere | `Ctrl+J`, `Alt+Enter` | insert a newline without sending |
| anywhere | `Shift+Enter` | newline — *only* in a Kitty-protocol terminal (see below) |
| anywhere | paste | a multi-line paste lands whole and never sends |
| anywhere | drag a file in | attaches it as an absolute path reference |
| Plan | `Ctrl+G` | **arm** — start auto-run on the current session |
| Auto-run (gate pending) | `Ctrl+X` | stop / veto this tool |
| Auto-run (gate pending) | `Ctrl+Y` | approve this tool now |
| Auto-run (gate pending) | `Ctrl+R` | pause / resume the countdown |
| Auto-run (gate pending) | anything else | goes to the message box, as usual |
| Auto-run (working) | `Esc` | interject — interrupt the turn, then type to redirect |
| Auto-run (working) | type + `Enter` | queue the message; it goes out when the turn ends |
| Auto-run (nothing running) | type + `Enter` | the run just waits — nothing is sent on its own; type to continue, or `/done` to finish it |
| Complete | type + `Enter` | chat with the session to vet the work |
| anywhere | `↑`/`↓`, `PgUp`/`PgDn` | scroll the transcript |
| anywhere | `Ctrl+C` | quit |

Scrolling is bound to the arrows and page keys only, so typing a message never
scrolls the transcript out from under you. The message box grows as you type (up
to 8 rows) and the transcript gives up the space; past that it scrolls internally,
so the message itself has **no length limit** — a whole plan document, typed or
pasted, keeps its paragraphs.

Dragging a file onto the terminal — or pasting a path — attaches it as a clean
absolute path in the message, instead of the escaped or quoted string the terminal
actually typed; a `📎 … attached` line under the box confirms it. Nothing is read or
uploaded: the path *is* the payload, and Claude opens it with its own `Read` tool if
it needs to (which is also why a screenshot path just works). The paths are plain
editable text — backspace them, type around them. A paste that isn't entirely file
references is inserted verbatim, as before.

`Shift+Enter` is a newline only where the terminal can tell it apart from a plain
`Enter`, which needs the Kitty keyboard protocol — Ghostty, kitty, WezTerm, and
iTerm2 3.5+. `acy` asks for it on every run, but a terminal that doesn't speak it
reports `Shift+Enter` as a bare carriage return, indistinguishable from `Enter`,
and the message simply sends. `Ctrl+J` and `Alt+Enter` work everywhere; prefer
them if you're not sure.

The gate controls are chords rather than bare letters because the countdown panel
sits *above* the message box rather than replacing it — in an armed run something
is nearly always counting down, and a bare `a` would approve a tool every time you
typed the word "and". `Esc` is the one key a pending gate does swallow: the hook
that raised the countdown is still blocked waiting for an answer, so answer it
(`Ctrl+Y` / `Ctrl+X`) and interject after.

### Typing while Claude works

You don't have to wait for a turn to end to say something. `Enter` while the
session is busy — a turn in flight, a gate counting down, a delegated task
running — **queues** the message instead of sending it: it appears dimmed in the
transcript, the header gains a `2 queued` marker, and a small panel above the
message box lists what's waiting. The queue goes out by itself the moment the
session next falls idle.

It goes out as **one turn**, with the messages joined by a blank line, however
many are waiting. That isn't tidiness — a turn re-bills the entire accumulated
context (the measurement under [Why delegation](#why-delegation) that this whole
design exists to shrink),
so sending three queued messages separately would pay for the conversation three
times to deliver text Claude reads in one pass anyway.

`Esc` composes with it for free: interrupt the turn, and the queued message is
sent as the redirect the moment the aborted turn reports back.

`/queue` lists what's waiting and `/queue clear` drops it. The queue is **never
saved** — it's transient intent, and a message surviving a crash to be delivered
into whatever phase the run comes back in is worse than one that was lost. If the
session ends with messages still queued, they're printed back into the transcript
so you can copy them out.

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
| `/queue` | list the messages queued while Claude works, in full (see [Typing while Claude works](#typing-while-claude-works)) |
| `/queue clear` | drop the queued messages, unsent |
| `/clear` | clear the transcript view |
| `/log` | show the debug-log path |
| `/tokens` | the token ledger: current context size, cache reads and cost, split parent vs children. This is the number to watch — the parent's line should stay flat while the children's climbs |
| `/tasks` | the delegated-task ledger: one row per task with its outcome, cost and cache reads. `/tokens` says what the run spent; this says what it spent it *on* |
| `/fleet` | arch mode only: the architect's engineer ledger — state, host, outcome and cost per engineer |
| `/tickets` | arch mode only: the architect's ticket board — every ticket's status, branch, PR and brief |
| `/done` | end the run by hand, if the session stopped without calling `Finish` |
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
armed the run, it comes back **armed**: no keys, no re-approval, no re-planning. It gets
exactly one prompt — take stock, then carry on — rather than restarting the task.

A task caught mid-flight by the crash is named rather than silently retried. Its child
process was killed mid-edit, so its work may be half-applied, and that is the one place a
hands-free tool should ask instead of guessing: the session is told which tasks never
reported and decides whether to re-dispatch them.

That works because the two halves are restored from two places. The conversation is
Claude's: `acy` replays the transcript Claude already keeps under `~/.claude/projects`.
Everything else — which phase you were in, the plan being worked to, the task ledger, the
tokens and cost by spender — is `acy`'s own, and it isn't in Claude's file, so `acy` keeps a small
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
- `--plan-tools` — the built-in `--tools` registry for the **supervising session**, in
  both phases (default `Read,Grep,Glob`). This is the registry, not an allowlist:
  anything left out cannot be called at all, which is what keeps the session you talk to
  incapable of changing your code. Dispatched children always get the full set.
- `--child-model` — model for dispatched tasks (default: same as `--model`). Often the
  single biggest saving available, since children do the bulk of the tokens.
- `--child-effort` — reasoning effort for dispatched tasks (`low`…`max`).
- `--task-budget` — spend ceiling in USD for one dispatched task (default: none). A
  runaway child stops instead of running until you happen to look.
- `--use-api-key` — bill `ANTHROPIC_API_KEY` instead of your claude.ai login. By
  default the key is **stripped** from the environment `claude` runs in: a key
  merely sitting in your shell silently takes precedence over the login, and
  headless `claude -p` never shows the interactive "use this API key?" prompt, so
  every run would quietly bill the API account. The header and the final tally say
  which account paid (`subscription` vs `API`), read from claude's own init event.
- `--log` — debug log file (default `acy-debug.log`); captures the full stream —
  every event received (`RX`), every message sent (`TX`), gate decisions, and phase
  transitions. Set to `""` to disable. `tail -f acy-debug.log` to watch it live.

Every flag above works on **`acy serve`** as well — the two commands share one
flag registration, so they cannot drift — and `serve` adds two of its own:

- `--port` — TCP port to listen on (default `0`: the kernel picks a free one).
  The host is not a setting; it is always `127.0.0.1`.
- `--token` — the bearer token every `/api/` request must carry. Empty (the
  default) mints a fresh 256-bit one and prints it on the stdout endpoint line.

## Layout

- `internal/driver` — the `claude` subprocess: stream-json args, stdin message
  injection, NDJSON event decoding, and `Interrupt()`.
- `internal/gate` — the permission bridge: a unix-socket server (supervisor) and
  the `hook` client, with allow/deny decisions.
- `internal/config` — generates the `--settings` file that registers the hook.
- `internal/orchestrator` — the disposable children: task queue, child lifecycle,
  the report schema, and the ledger.
- `internal/ui` — the Bubble Tea v2 model: transcript, countdown, phase machine,
  delegation, the message queue, pasted-path attachment, slash commands, and the
  resume / AskUserQuestion pickers. Also the two front-end seams both UIs share —
  `Frame` (the run as a JSON value), `Action` (one semantic command, which the
  terminal's own keys raise too), and the presentation decisions behind both.
- `internal/session` — reads claude's `~/.claude/projects/<slug>/*.jsonl` transcripts:
  lists them for the `/resume` picker, and replays one back into the transcript view.
- `internal/state` — `acy`'s own snapshot of a run (phase, plan, task ledger, tokens,
  cost) — the part of a session claude's transcript doesn't record.
- `internal/hub` — the headless runtime: one `ui.Model`, one goroutine, and the
  frame stream everything that isn't the TUI drives the run through. It emits a
  frame only when the bytes change, so an idle run is silent.
- `internal/htmlrender` — transcript entries as sanitized HTML for the webview,
  plus the dark/light syntax stylesheets. Only produced for a served run; `acy
  run` never pays for it.
- `internal/server` — the HTTP front door behind `acy serve`: frames, actions,
  SSE, the highlight stylesheet, a bearer token and a deliberately narrow CORS
  surface.
- `internal/e2e` — the live end-to-end suite (see below).
- `internal/cli` — Cobra commands (`run`, `serve`, hidden `hook`).
- `vscode/` — the VS Code extension: the terminal launcher, the `acy serve`
  panel, and the webview client that renders its frames.

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
| `TestE2EVetoBlocksATool` | `Ctrl+X` actually stops a tool. The most important key on the board, and the one you'll never press |
| `TestE2EResumeAnArmedRunAfterACrash` | kill an armed run mid-flight, `--continue`, and watch it finish unattended |
| `TestE2EResumeACompletedRunLandsInPlan` | a finished run comes back as a chat, with its cost intact |
| `TestE2EResumeASessionWithNoSnapshot` | a session `acy` never drove still resumes |

The narrower live tests (`ACY_LIVE=1` in `internal/driver`, `internal/gate`,
`internal/ui`) probe single seams: the stream-json wire format, the hook chain, and
whether `AskUserQuestion` has become real yet.

## License

[WTFPL v2](LICENSE) — you just do what the fuck you want to. It ships in the release
archives and in the extension package, so whatever you install already carries it.
