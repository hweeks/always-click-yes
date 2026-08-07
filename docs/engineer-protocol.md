# The engineer wire protocol

`internal/engineerwire` is the contract for "arch mode": an **architect** acy
process drives one or more detached **engineer** acy processes over NDJSON —
one JSON object per line, stdout/stdin locally, the same bytes over an ssh
pipe remotely. Nothing here is transport-specific; a local subprocess and a
remote engineer speak the identical protocol.

This document describes the message set, the seq/replay contract, and
protocol version negotiation. It does not describe a CLI or a daemon —
neither exists yet. The Go types live in `internal/engineerwire`; this is
their spec.

## Message set

Every line is a JSON object with a `type` field naming which message it is.
There are two independent directions on the wire, and a message never
crosses from one to the other.

### Inbound: architect → engineer

| type | fields |
|---|---|
| `spec` | `ticket, title, brief, success, base_branch, branch, model, child_model, child_effort, budget_usd, deadman_hours` |
| `answer` | `question_id, text` |
| `cancel` | `reason` |

`spec` is the one task assignment an engineer process is spawned with — the
whole of what it knows about its job. There is no earlier conversation to
fall back on, which is why `brief` and `success` exist as their own fields
rather than folded into an ambient system prompt.

`answer` resolves exactly one outstanding `question` (matched by
`question_id`); `cancel` tells the engineer to stop, with `reason` recorded
for the log.

Inbound messages are **not journaled**. They arrive on the engineer's stdin
and are consumed once; there is nothing here for a reconnecting architect to
replay, because the architect is the one who sent them and already knows
what it sent.

### Outbound: engineer → architect

Every outbound message carries two fields no inbound message has:

- `seq` (int64) — starts at 1, strictly increasing, one sequence per engineer
  process for its entire life.
- `at` (RFC3339) — when the engineer wrote the line.

| type | fields (beyond seq/at) |
|---|---|
| `hello` | `engineer_id, protocol_version, acy_version, host, pid` |
| `event` | `kind, text, cost_usd, tokens` |
| `question` | `question_id, questions` |
| `result` | `outcome, summary, branch, pr_url, cost_usd, tokens, files, verification` |

`hello` is always the first thing an engineer sends and is therefore always
`seq: 1`. `protocol_version` is the wire protocol's major version (currently
`1`) — see [Protocol version negotiation](#protocol-version-negotiation).

`event.kind` is one of `phase`, `task_started`, `task_report`, `cost`, `log`.
`event` is the narration channel: what phase the engineer is in, that it
started or reported on a sub-task, a cost checkpoint, or a free-text log
line.

`question.questions` is the **same JSON shape** as the `AskUserQuestion`
schema in `internal/mcp/protocol.go` (`askSchema`) — an array of `{question,
header, multiSelect, options: [{label, description}]}`. An architect can
render a `question` with the exact same UI it already has for
`AskUserQuestion`, with no translation layer in between.

`result` is the engineer's final report. Nothing follows it — an engineer
process that has sent a `result` is done and exits.

`result.verification` is machine-collected: the commands acy's own code ran
in the worktree after the session's own verdict, never the model's report of
having run them. Each entry's `status` is one of:

- `passed` — ran, exit 0
- `failed` — ran, non-zero exit
- `skipped` — the binary isn't installed on this host, a fact, not a failure
- `timeout` — the per-command deadline elapsed
- `error` — couldn't be launched or run for any other reason

Each entry's `output` is capped, so a consumer should treat it as bounded
evidence, not a full log: an oversized capture keeps the head and tail and
marks `truncated: true` rather than growing without limit.

`tokens` fields on `event` and `result` reuse `state.Tokens`
(`internal/state/state.go`): `input`, `output`, `cache_create`, `cache_read`.

## The seq/replay contract

An engineer process can be long-running and detached — the architect
watching it may disconnect and reconnect, or a second `attach` may want to
watch the same run. Every outbound message is therefore also persisted to a
`Journal` (see below) as it is sent, keyed by its `seq`.

The contract:

- **Replay from seq N is byte-precise and lossless.** `ReplayFrom(n)` returns
  every message with `seq >= n`, in order, reconstructed from exactly the
  bytes that were written — not a summary, not a "since you've been gone"
  digest.
- **`hello` is always seq 1.** An architect that resumes with `fromSeq: 1`
  always sees the engineer's identity and protocol version first, exactly as
  a fresh attach would.
- **Seq has no gaps and no resets** for the life of one engineer process. A
  crash and restart of the *architect* changes nothing about the engineer's
  sequence; a new engineer process starts its own sequence at 1.
- **Inbound messages are not part of this contract.** They are not journaled
  and have no seq — see above.

## Protocol version negotiation

`hello.protocol_version` reports the wire protocol's **major** version. An
architect that receives a `hello` reporting a different major version
**refuses to attach**: it has no guarantee it can read a shape it wasn't
built for, and guessing at a mismatched version is how a partially-understood
run gets silently misreported. There is no negotiation handshake beyond
this — an engineer speaks one version, and an architect either understands
it or declines the connection.

## The journal

`Journal` (`internal/engineerwire/journal.go`) is the on-disk record of one
engineer's outbound stream, `journal.ndjson` in a directory the caller
chooses.

- **One writer, many readers.** The engineer process (or whatever wraps it)
  is the only thing that calls `Append`. Any number of processes — the live
  architect, a later `attach`, a second observer — can call `ReplayFrom` or
  `Follow` concurrently; they read the file directly and take no lock.
- **`Append`** assigns the next `seq` and the current time, then writes the
  complete line in a single `Write` call. That single-call write is load
  bearing: it is what lets a reader tell a genuinely truncated line (the
  process died mid-write) apart from real corruption elsewhere in the file.
- **`Open`** recovers `lastSeq` by scanning the existing file. If the file
  ends in an incomplete line — no trailing newline, because the writer died
  before finishing that one `Write` — `Open` drops it, both from the
  recovered state and from the file on disk (truncating it away). Dropping it
  from disk, not just from memory, matters: the next `Append` writes with no
  leading newline of its own, so leaving the fragment in place would glue it
  to the next line and corrupt that one too.
- **`Follow(ctx, fromSeq)`** returns a channel that first delivers the
  replay from `fromSeq`, then polls the file every ~150ms for newly appended
  lines until `ctx` is done. Polling, not a filesystem watch, is deliberate:
  the follower is normally a *different process* from the writer (the
  detached engineer keeps running; the attach comes and goes), so there is no
  in-process channel to fan out on, and the repo takes on no new dependency
  (no fsnotify) to bridge that gap — the same tradeoff already made for the
  hand-rolled UUID in `internal/orchestrator/dispatch.go`.

## What is deliberately not here

No CLI flag parses a `Spec` off the command line. No process spawns an
engineer over ssh. No git branch gets created or pushed. This package is the
wire types, the journal, and this document — the ground the rest of arch
mode gets built on.
