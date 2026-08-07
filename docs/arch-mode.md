# Arch mode: running a fleet of engineers

`acy arch` is `acy run` for a whole fleet of machines instead of one checkout. This is
the operator's guide — what to prepare before you point it at real hosts, what the
config means, and what a run actually looks like. For the wire format between the
architect and its engineers, see [`docs/engineer-protocol.md`](engineer-protocol.md).
For what took real probing to get right, see [`AGENTS.md`](../AGENTS.md)'s "Arch mode"
section — this document assumes that's correct and doesn't repeat it.

## What arch mode is

A plain `acy run` has one supervising session that reads your codebase and delegates
tasks to disposable local children. `acy arch` scales that up one level: the
supervising session becomes the **architect**, and instead of dispatching a local
child for each task, it launches a whole **engineer** — a full, unattended `acy`
instance — for each ticket. An engineer gets its own git worktree and branch, plans
its own subtasks the same way a plain `acy run` would, and finishes by opening a PR.
The architect's job shrinks to exactly what a human orchestrating several engineers
by hand would do: decide the tickets, launch them up to capacity, react to results
and questions, and keep the board honest.

Nothing here is simulated. An engineer is a real `acy engineer` process running on a
real host, driving a real `claude` session, with the same PreToolUse gate and
countdown a plain `acy run` has — just with nobody local watching it.

## Preparing a host

Every host you point `hosts` at should be one you'd trust an unattended process on —
read the [trust paragraph](#the-trust-paragraph) before you configure a single one.
Concretely, each host needs:

- **A dedicated or disposable machine, or at least a dedicated checkout.** An engineer
  runs `git`, `gh`, and arbitrary shell in a worktree of its own, but it shares the
  host's `claude` auth, `gh` auth, and everything else on the box. Don't point it at a
  machine that also holds work you can't afford an unattended agent to touch.
  `internal/gitops` scopes what it does to the worktree it creates, not to what the
  engineer's own shell commands can reach.
- **Key-only SSH, in `BatchMode`.** `internal/fleet/ssh.go` hard-wires
  `-o BatchMode=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=4` into every ssh
  invocation — this is not configurable, on purpose. An interactive password or
  host-key prompt is exactly the kind of thing an unattended engineer would hang
  behind forever, so the fleet transport refuses to offer one a chance to appear. Get
  your key onto the host and accept its host key by hand (`ssh -o BatchMode=yes
  you@host true` should succeed silently) before you ever put it in `.acy.json`.
- **A clone of the repo already on the host**, at the path you'll put in
  `hosts[].repoPath`. Arch mode doesn't clone anything for you.
- **`claude` authenticated on that host.** However you normally authenticate (login or
  `ANTHROPIC_API_KEY`), it has to already work there — an engineer inherits whatever
  the host's `claude` sees.
- **`gh` authenticated, scoped to the repo the engineers will open PRs against.** The
  engineer's own deterministic `gitops.CreatePR` call is the only thing that ever
  opens a PR (see AGENTS.md on why the *model* is never told to), but it still runs as
  whatever `gh` identity the host has.
- **An `acy` binary on the host, version-matched to the one driving the architect.**
  `acy fleet doctor` checks this and warns (not fails) on a mismatch — a skewed
  version is the kind of thing that's fine until the wire protocol changes underneath
  it.

Run `acy fleet doctor` against every host before trusting a real run to it:

```sh
acy fleet doctor            # table output, one row per host
acy fleet doctor --json     # machine-readable, same checks
```

It runs eight checks per host, in order, and stops early if `ssh` itself fails (nothing
past that point can succeed anyway): `ssh` reachability, the `acy` binary's presence
and version, `claude auth status` (or a bare PATH check if that subcommand doesn't
exist), `gh auth status`, whether a Go toolchain is on the host (and its version, if
so — see below), the git worktree and `origin` reachability, whether
`$ACY_STATE_DIR/engineers` (or its OS-default equivalent) is actually writable on
that host, and whether the `gh-stack` extension is usable (see below). Fix everything
doctor flags before running `acy arch` for real — a host that fails silently mid-run
is a stuck engineer nobody is watching.

The Go-toolchain check is informational, not a gate: it reports `OK` either way, with
a Detail naming the version when one is found or explaining it's absent when it
isn't. A host with only a prebuilt `acy` binary and no compiler is a perfectly good
fleet member day-to-day — the toolchain only matters as a fallback for the scenario
below, and a host lacking it shouldn't read as broken.

The `gh-stack` check is informational too, and for the same reason: a repository that
hasn't enabled the stacked-PRs preview, or a machine with the extension not installed
at all, is still a perfectly good fleet for ordinary, flat, non-stacked PRs. It's also
the one check in this list that isn't about the host in that row at all — `gh stack`
is only ever run by the architect, on its own local machine, so this check always
probes the machine running `acy fleet doctor` itself, regardless of which host's row
it's reported under.

When a host sets `rc`, every one of these checks — the `ssh` check included — runs
behind the same `zsh -c 'source <rc>; ...'` wrap the engineer transport uses, so a
missing `zsh` or an otherwise broken invocation fails the `ssh` check by name, with
the shell's own stderr, instead of showing up as a mystery downstream in `claude` or
`gh`.

## Provisioning a fleet host

`.acy.json` is untracked (see `.acy.json.example` at the repo root for a starting
point with the exact `fleet` keys), so a fresh clone has no `acy` binary on a new host
and nothing installs one for you. What actually worked provisioning two real hosts:

- **Don't rely on `gh release download`.** A GitHub Actions outage meant v1.6.0 shipped
  with no release assets at all — the release existed, the binaries didn't. Build from
  the host's own clone of the repo instead of assuming a release artifact exists.
- **Don't use `go install github.com/hweeks/always-click-yes@latest`.** It resolves
  through `proxy.golang.org`, not this repo directly, and during the same outage that
  either 404'd or silently installed a stale previous tag with no error indicating it
  wasn't current. Build from the clone.
- **Build with the version stamp**, or `acy --version` reports `(devel)` and `acy fleet
  doctor` warns of a version mismatch even when the binary is actually current:

  ```sh
  go build -trimpath -ldflags "-s -w -X github.com/hweeks/always-click-yes/internal/version.stamped=$(git describe --tags --always)" -o <acyBin> .
  ```

- **No Go toolchain on the host? Cross-compile and scp from the architect's machine.**
  This is how host `spark` (Ubuntu aarch64, no `zsh` and no Go toolchain at all) was
  provisioned: `GOOS=linux GOARCH=arm64 go build ...` locally, `scp` the resulting
  binary to the path `hosts[].acyBin` points at, then `chmod +x` it on the host.
- **Go is usually not on the PATH a non-interactive ssh gets.** The same starved-PATH
  problem `hosts[].path` exists for hits `go` itself, not just `claude`/`gh` — find it by
  absolute path (`which go` in an interactive shell on the host, or a well-known
  location like `/usr/local/go/bin/go` or `~/go/bin/go`) rather than assuming a bare
  `go build` will resolve on a fleet host. Host `studio` (macOS arm64) is the concrete
  case: its Go lives at `/opt/homebrew/bin/go`, invisible to a non-interactive ssh
  session unless you invoke it by that absolute path or list the directory in the
  host's `path` array in `.acy.json`.

### A trap when probing a host by hand

Diagnosing a host yourself, outside `acy fleet doctor`, has a sharp edge: ssh joins its
trailing arguments with a bare space before the remote side ever sees them, even when
you've written what looks like an explicit wrapper. So this:

```sh
ssh -o BatchMode=yes studio -- zsh -c 'ls -l /opt/homebrew/bin/go'
```

reaches the remote host as `zsh -c ls -l /opt/homebrew/bin/go` — `zsh -c` takes only its
first word (`ls`) as the script, and `-l` / the path become `$0` / `$1`, arguments the
script never reads. `ls` therefore runs with no arguments and lists `$HOME`. It does not
error — it returns plausible, wrong output. The same shape made `stat` report on stdin
instead of the path given, and made `command -v`, `type`, and a bare multi-word `echo`
misbehave identically. What makes it so easy to misdiagnose live: a command placed
*after* a `;` in the same string is unaffected, because the remote login shell parses
that part directly — so half a hand-typed probe can be right while the other half
silently lies, in the same command string.

The fix is to pass the whole remote command as **one** argument, so ssh has nothing left
to join:

```sh
ssh -o BatchMode=yes studio 'zsh -lc "ls -l /opt/homebrew/bin/go"'
```

**Canary**: before trusting any hand-run probe, run
`ssh -o BatchMode=yes <host> 'zsh -lc "echo A B C"'` and confirm it prints `A B C`, not
just `A` — if it prints `A`, everything after the first word is landing in `$0`/`$1`
instead of the script.

acy's own transport never hits this: `quoteArgv` and `sshBatchArgs` (both in
`internal/fleet`) compose the remote command as one pre-quoted string before ssh ever
sees it. This trap only bites a probe you type yourself.

## The fleet config

Arch mode requires a `"fleet"` section in `.acy.json`. Every field is optional except
`hosts[].name`; nothing below runs at all if `"fleet"` is absent. `.acy.json` itself is
untracked (it holds your real ssh targets and paths) — copy `.acy.json.example` from the
repo root to get every key name right on a fresh clone.

```json
{
  "fleet": {
    "baseBranch": "main",
    "prCap": 4,
    "engineerModel": "sonnet",
    "engineerChildModel": "sonnet",
    "engineerEffort": "medium",
    "engineerBudgetUSD": 15,
    "runBudgetUSD": 200,
    "deadmanHours": 24,
    "ticketCommit": "direct",
    "stackMode": "ask",
    "verifyCommands": ["go build ./...", "go test -race ./...", "gofmt -l .", "golangci-lint run ./..."],
    "verifyTimeoutSeconds": 900,
    "hosts": [
      { "name": "local" },
      {
        "name": "box2",
        "ssh": "you@box2.example.com",
        "repoPath": "/home/you/proj",
        "maxEngineers": 2,
        "acyBin": "acy",
        "path": ["/opt/homebrew/bin", "/home/you/.local/bin"],
        "rc": "~/.zshrc"
      }
    ]
  }
}
```

- **`baseBranch`** (default `"main"`) — the branch every engineer's worktree and PR
  target.
- **`prCap`** (default `4`) — how many `acy/`-headed PRs may be open at once, across
  the whole fleet, before `LaunchEngineer` refuses and the architect has to `Await` a
  merge first. This is the fleet's backpressure valve — see
  [PR-cap backpressure](#pr-cap-backpressure).
- **`engineerModel`** / **`engineerChildModel`** / **`engineerEffort`** — the model an
  engineer's own supervising session uses, the model its own dispatched children use,
  and the reasoning effort for those children. Same knobs as `acy run --model` /
  `--child-model` / `--child-effort`, just applied per-engineer instead of per-run.
- **`engineerBudgetUSD`** — a spend ceiling for one engineer (default: unlimited).
  Clamped down further by whatever's left under `runBudgetUSD` — see
  [Budgets](#budgets).
- **`runBudgetUSD`** — a spend ceiling for the *whole fleet*, summed across every
  engineer that's ever launched this run (default: unlimited). `LaunchEngineer` itself
  refuses once this is exhausted.
- **`deadmanHours`** (default `24`) — the hard ceiling on one engineer's own runtime,
  regardless of what the architect does. See [the trust paragraph](#the-trust-paragraph)
  — this is what bounds an orphan.
- **`ticketCommit`** (default `"direct"`, or `"none"`) — whether the ticket board
  (`.acy/tickets/*.md` in the repo) is committed and pushed as it changes, or left as
  local, uncommitted state.
- **`stackMode`** (default `"ask"`, or `"off"` / `"chain"`) — whether an engineer's work
  lands as a stack of dependent branches/PRs. `"off"` means never stack, never ask.
  `"ask"` means the architect asks the human during planning whether this run's work
  should land as stacks — and `"ask"` degrades to `"off"` automatically when the
  `gh-stack` extension or the repo's preview enablement is missing (a later ticket
  implements this specific downgrade behavior, but the contract is documented here).
  `"chain"` means stack by default without asking.
- **`verifyCommands`** (default `["go build ./...", "go test -race ./...", "gofmt -l .",
  "golangci-lint run ./..."]`) — commands run in an engineer's worktree after it
  finishes, as evidence collected by acy itself rather than the model's own claim of
  having run tests. An explicit `[]` disables verification entirely. Each entry is
  whitespace-split into argv directly — no shell is involved, so pipes, redirects,
  globs, and quoted arguments don't work the way they would on a command line. A
  command whose binary isn't found is recorded as `skipped` rather than failing the
  run. `ACY_LIVE` (and anything else prefixed `ACY_LIVE`) is stripped from the
  environment before any command runs: `ACY_LIVE=1` gates `internal/e2e`'s live suite,
  which spends real money on a real `claude` session, and a verify command that
  inherited it could silently kick off a live run of its own.
- **`verifyTimeoutSeconds`** (default `900`, i.e. 15 minutes) — the wall-clock ceiling
  for each command in `verifyCommands`. Must be greater than zero.

  If any configured check actually ran and exited non-zero (`failed`), the engineer's
  final outcome is overridden to `"failed"` regardless of what the model itself
  reported — but the branch is still pushed and the PR still opened either way, so the
  failure is something a reviewer can see rather than something that silently
  disappears. A `skipped` (missing binary) or `timeout` status does *not* override the
  outcome — those are facts about the host, not a verdict on the work. Either way, the
  verification digest (`Verification (run by acy in the worktree, not reported by the
  session): ...`) is appended to both the engineer's `Result.Summary` and the PR body
  it opens, so a reviewer sees it without needing to inspect the raw journal.
- **`hosts`** — the machines engineers may run on. A host with no `ssh` runs engineers
  locally, as direct child processes of the architect's own host; anything with `ssh`
  reaches the target over the hard-wired `BatchMode` ssh described above.
  - **`name`** — required, unique. What `FleetStatus` and the ticket board refer to it as.
  - **`ssh`** — an ssh target (`user@host`); omit for the local host.
  - **`repoPath`** — the clone's path on that host. Required if `ssh` is set; defaults
    to the current project directory for a local host.
  - **`maxEngineers`** (default `1`) — how many engineers may run concurrently on this
    one host. Fleet-wide concurrency is just the sum across hosts — there's no
    separate fleet-wide cap beyond that.
  - **`acyBin`** (default `"acy"`) — how to invoke `acy` on that host, if it's not on
    `PATH` under the usual name.
  - **`path`** — extra directories to prepend to `PATH` on that host, absolute only (a
    relative or `~` entry is rejected at load time, since it never expands where this
    runs). A non-interactive `ssh host cmd` hands the remote command a minimal PATH —
    typically just `/usr/bin:/bin` — which is not where `claude` or `gh` actually live on
    plenty of real machines (`~/.local/bin`, `/opt/homebrew/bin`, an nvm/asdf shim
    directory). Without `path`, that starved PATH is what both `acy fleet doctor`'s checks
    and the detached engineer daemon itself see — and the daemon's own children (`claude`,
    `gh`, `git`) inherit that same environment, so a missing entry here breaks a real run,
    not just a diagnostic.
  - **`rc`** — a shell rc file to source before every remote command on this host, e.g.
    `"~/.zshrc"`. Must start with `"~/"` or `"/"` — and unlike `path` above, a leading `"~"`
    is exactly the point rather than a mistake to reject: `rc` is never spliced into a
    command directly, it is only ever handed to the remote `zsh` as the argument of a
    `source` call, so it's the remote shell that expands the tilde, not `acy`.
    **Prefer `rc` over `path` on real hosts.** `path` only ever fixes `PATH`; plenty of
    real machines have `claude` or `gh` working only because the login shell's rc also
    wires up auth env vars, nvm/asdf shims, or other state a bare PATH extension can't
    replicate. When `rc` is set, every remote invocation — the engineer transport's
    `start`/`attach` argv and every `acy fleet doctor` check command — runs as `zsh -c
    'source <rc> >/dev/null 2>&1; <command>'`, composed after any `path` preamble, so the
    two settings stack rather than conflict. Setting both is normal and harmless: `path` is
    a cheap belt-and-braces default, `rc` is what actually fixes a host where PATH alone
    wasn't enough.
  - **`shell`** — overrides which shell `rc` is sourced through, e.g. `"bash"` or `"fish"`.
    Empty (the default) derives it from `rc`'s basename: `.zshrc`/`.zprofile`/`.zshenv` mean
    `zsh`, `.bashrc`/`.bash_profile`/`.profile` mean `bash`, anything unrecognised falls back
    to `sh`. Set this when a host's rc file doesn't follow that naming, rather than fighting
    the derivation — a Linux host whose rc is sourced through `bash` needs no `shell` key at
    all, since `.bashrc` already derives it.

## Running `acy arch`

```sh
acy arch
```

opens the same TUI a plain `acy run` does, in PLAN phase, with the architect's
system prompt in place of the parent's. The flow:

1. **Plan.** You talk to the architect the way you'd talk to a plain `acy run`'s
   parent session — it can read the codebase (`Read`/`Grep`/`Glob`) and nothing else.
   Work out what needs doing.
2. **Tickets.** Once you approve the plan, the architect turns it into board entries —
   one `CreateTicket` call per PR-sized unit of work — *before* launching anything.
   Each ticket's brief has to stand completely alone, the same way a `Dispatch`
   instruction does: the engineer that eventually runs it starts with no memory of
   this conversation. A ticket can optionally name a `depends_on` (must merge first)
   or a `stack_on` (its branch may sit on that ticket's still-open PR instead of
   waiting for a merge) — at most one ticket may claim a given `stack_on` parent.
   This is board bookkeeping, not the mechanism itself: `LaunchEngineer`'s own
   `stack_on` argument (step 4) is what actually stacks the branch when the ticket
   is launched. When `fleet.stackMode` is `"ask"`, the architect puts the stacking
   choice to you first, as a standard planning question — one review surface and one
   clean landing on trunk, versus tickets that must run in order rather than in
   parallel — before creating any tickets; `"chain"` stacks by default without
   asking, and `"off"` never stacks.
3. **Arm (`Ctrl+G`).** Flips the session into AUTO-RUN, same keystroke as a plain run.
   The architect gets one kickoff prompt: launch engineers for the first tickets up
   to capacity, then `Await`.
4. **Launch / `Await` loop.** This is the architect's main loop, and it's a loop, not
   a queue it drains once: launch up to whatever capacity `hosts[].maxEngineers` and
   `prCap` allow, then block on `Await` for the next fleet event — an engineer's
   result, an escalated question, a PR merge or close, or a reconnect notice after a
   dropped connection — react to it, and loop. `LaunchEngineer` takes its own optional
   `stack_on` argument — separate from the ticket board's `stack_on` field in step 2,
   though normally set to the same parent id — naming a parent ticket, which stacks
   the new engineer's branch on that ticket's still-open PR instead of trunk, so the
   child can start as soon as the parent's PR opens rather than waiting for it to
   merge. `FleetStatus` gives it (and you, via `/fleet`) a non-blocking snapshot of
   every engineer's state, host, branch, PR and cost without waiting for the next
   event.
5. **PR-cap backpressure.** <a name="pr-cap-backpressure"></a> Once `prCap` PRs are
   open, `LaunchEngineer` refuses with a message telling the architect to `Await`
   merges first. This is deliberate: it's the difference between a fleet that keeps
   working ahead of what a human can review, and one that piles up PRs faster than
   anyone can merge them. Raise `prCap` if you actually have the review bandwidth for
   it, not as a way around the refusal.
6. **Merge-driven ticket updates.** `internal/fleet`'s `PRWatcher` polls `gh pr list`
   and turns a merged or closed `acy/`-headed PR into a fleet event, but nothing in
   `fleet` or `ui` writes the ticket board on its own — the architect's system prompt
   tells it to call `UpdateTicket` at every transition (launch → in-progress → PR
   opened → in-review → merged, or blocked with a note), so the board is prompt-driven
   state, not code-driven state. `/tickets` shows you the same board the architect
   reads.
7. **Finish.** The architect calls `Finish` once every ticket is merged or otherwise
   accounted for (blocked with a note is accounted for; silently unmentioned isn't).

## Resuming after a crash

Nothing about a crash here is unusual by arch mode's standards — it's the normal
case a fleet has to survive, not an edge case. Two independent things get restored:

- **The architect's own session and board** resume exactly the way a plain `acy run`
  does: `acy --continue` (or `--resume <id>`) restores the transcript, phase, plan and
  cost from `acy`'s own state snapshot, the same as any other run.
- **Each engineer's progress** is never lost, because it was never only in the
  architect's head to begin with — it's in that engineer's own journal
  (`internal/engineerwire`), on whichever host it's running on, independent of
  whether the architect is even alive to watch it. A resumed architect re-attaches to
  every engineer the ledger remembers, replays each journal from wherever it left
  off, and picks the loop back up — it does not re-launch anything the ledger already
  has a record of. This is what `TestE2EArchResumeRecoversEngineer` proves: kill the
  architect mid-flight, and its detached engineer finishes the ticket and opens the PR
  with nobody attached to it the whole time; a resumed architect only has to notice
  the `Result` sitting in the journal.

If the architect crashes *and* you never resume it, the engineers it launched don't
notice or care — they keep running until they finish, hit their own budget, or hit
`deadmanHours`. That's a feature: an unattended engineer's job doesn't depend on an
unattended architect staying alive to supervise it.

## Budgets

Two independent ceilings, checked in order:

- **`fleet.engineerBudgetUSD`** bounds what any one engineer may spend. It's further
  clamped to whatever's left under the fleet-wide ceiling at launch time, so a
  generous per-engineer budget can't outbid a tighter fleet-wide one.
- **`fleet.runBudgetUSD`** bounds the *fleet*, summed across every engineer launched
  this run. `LaunchEngineer` itself refuses once this is exhausted, with a message
  telling the architect to ask a human to raise it or `Finish` — not to retry on its
  own.

Neither has a default ceiling (both are unlimited unless you set them). Set
`runBudgetUSD` before a real run the same way you'd think about `--task-budget` on a
plain `acy run` — it's the number that keeps a fleet that's misbehaving from being a
number you only find out about later.

## The trust paragraph

An engineer is an unattended agent with `Bash` on whatever machine it runs, auto-
approving its own tool calls on exactly the countdown `acy run` uses. Nothing is
watching it between the moment the architect launches it and the moment it reports
back a result or a question. Detached, it keeps working with no ssh connection, no
attach, and no architect at all — that's not a bug in the detachment model, that's
the entire point of it, and it means the usual instinct to "just go check on it" does
not apply the way it would to a process you can see. The `deadmanHours` ceiling is
what actually bounds an orphaned engineer if nobody ever comes back for it; without
it, a stuck or looping engineer runs until its own task budget or the host itself
stops it.

Point `hosts` only at machines and repos you would trust an unsupervised process on,
for exactly the same reason `acy run` itself is worth sitting with: this tool is
built to find out what happens when you stop pretending a human needs to watch every
step, and to be honest about the failure modes when it does, rather than quietly
cleaning them up before anyone notices. A fleet of engineers is that bet multiplied
by however many hosts you configure.
