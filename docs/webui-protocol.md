# The web UI protocol — `Frame` and `Action`

`acy`'s state lives in unexported fields of `ui.Model`, which the Bubble Tea TUI
renders directly. A second front end (an HTTP server feeding a VS Code webview)
cannot reach in there, so the model has two seams:

- **`ui.Frame`** — the read seam. A pure value describing the whole run,
  produced by `func (m ui.Model) Frame() ui.Frame` and marshalled with
  `encoding/json`.
- **`ui.Action`** — the write seam. A semantic instruction delivered as a
  `ui.ActionMsg` on the Bubble Tea message loop, acknowledged with a
  `ui.ActionResult`.

The sources of truth are `internal/ui/frame.go` and `internal/ui/action.go`.
This file is the contract that the server, the webview and the next milestone
read; change one, change both.

Actions are the *only* way in. There is no "send a keystroke" endpoint, on
purpose: a synthesised Ctrl+Y means "whatever the terminal was looking at", and
a client is not looking at the terminal. Every terminal key handler in
`update.go` raises the same `Action` a webview would, so there is exactly one
implementation of each behaviour and it is the one both front ends exercise.

## Two rules that are not negotiable

**Gates are identified by `toolUseId`, never by index.** Gates auto-approve on
their own countdown, so between a client rendering "gate 0" and the request
arriving, the head of the queue may already be a different tool — index-based
targeting would approve the wrong one. `toolUseId` comes from the PreToolUse
hook and names exactly one tool call. A `gateAllow`/`gateDeny` whose
`toolUseId` matches no pending gate resolves **nothing at all** and comes back
rejected; there is deliberately no fallback to the head of the queue, because
that fallback is precisely how you approve a tool nobody looked at.

**Countdowns travel as an absolute deadline, and there is no `now` in a frame.**
`deadlineUnixMs` says *when* a gate auto-approves; the client counts down against
its own clock. Nothing in `Frame` carries the current time, because the UI ticks
every 120 ms and a later milestone detects change by comparing frames — a
timestamp anywhere in here would make every tick look like news and push eight
frames a second forever.

## Delivery: how a client gets its frames

Both seams are driven by `internal/hub`, the headless runtime that owns the
model and the goroutine applying messages to it. `Hub.Do(ui.Action)` performs an
action and hands back its `ActionResult`; `Hub.Subscribe()` returns a stream of
`hub.Update{Rev int, JSON []byte}` — the frame already marshalled, because the
Hub had to marshal it to know whether anything changed.

Three properties a client can rely on:

- **A frame is emitted only when the bytes change.** The model ticks every 120ms
  and `Frame` carries no clock, so an idle run marshals to identical bytes and
  the Hub sends nothing at all. This is the same property the `now`-free rule
  above exists to make possible; it is enforced there and relied on here.
- **`rev` counts distinct frames, from 1.** It does not advance while a run sits
  idle. A client that missed frames sees `rev` jump, which is the only way it can
  tell — there is no replay.
- **Each subscriber holds one frame, never a queue.** A frame that has not been
  read yet is replaced by a newer one, so a slow client can miss intermediate
  states and can never miss the current one. A subscriber that stops reading
  entirely slows nothing down.

A new subscriber is primed with the current frame, so a webview opened halfway
through a run renders immediately.

## The HTTP transport

`internal/server` puts the two seams on the wire, and `acy serve` starts it. The
supervisor underneath is the one `acy run` builds — same gate socket, same
PreToolUse hook, same dispatched children — driven by `internal/hub` instead of
by a terminal.

### Starting it

```sh
acy serve                       # 127.0.0.1, a port the kernel picks
acy serve --port 7777 --token "$MY_TOKEN"
```

`serve` takes **every run setting `run` takes** (`--model`, `--countdown`,
`--claude-bin`, `--log`, `--max-lines`, `--plan-tools`, `--use-api-key`,
`--child-model`, `--child-effort`, `--task-budget`, `--resume`, `--continue`)
from the same registration, plus `--port` and `--token`. The project's
`.acy.json` overlays them identically.

It prints **exactly one line to stdout**, as soon as the listener is up and
before anything else:

```json
{"url":"http://127.0.0.1:54321","token":"8f3c…"}
```

That line is the contract with whatever launched acy: parse it, and connect.
Nothing else is ever written to stdout — the debug log is a file, and errors go
to stderr — so a parent process can read one line and stop.

`serve` sets `ui.Config.RenderHTML`, which is what fills `entry.html`. Nothing
else does, so a client of any other launch path sees empty `html` fields.

### Routes

| method | path | auth | answer |
|---|---|---|---|
| `GET` | `/healthz` | **none** | `200 {"status":"ok"}` — liveness, and deliberately nothing about the run |
| `GET` | `/api/frame` | bearer | `200` the current `Frame` as JSON, plus an `X-Acy-Rev` header |
| `GET` | `/api/events` | bearer | `200 text/event-stream` — frames as they change |
| `POST` | `/api/action` | bearer | `200` the `ActionResult`, accepted **or refused** |
| `GET` | `/api/sessions` | bearer | `200 SessionRow[]` — the resume picker's rows |
| `GET` | `/api/highlight.css?theme=dark\|light` | bearer | `200 text/css` — `htmlrender.StyleSheet` |

Status codes, and the one that matters:

- **`401`** — a missing or wrong bearer token on any `/api/*` route.
- **`400`** — a body that is not an action: malformed JSON, or a `kind` this
  build has no vocabulary for. Also an unknown `theme`.
- **`200` for an action the model refused.** A refusal is a *domain answer*, not
  a transport failure. A client describes a run it last saw a moment ago, and
  that run moves on its own — gates auto-approve, turns end, phases change — so
  "no gate is pending for that toolUseId" is the system working. The reason
  string is what the client needs in both cases, and an error status would push
  callers into treating a normal outcome as a broken connection.
- **`403`** — a CORS preflight from an origin that is not allowed (see below).
- **`500`** — listing sessions failed, or the stylesheet could not be built.
- **`503`** — `/api/frame` before the model has produced its first frame. Retry;
  nothing is wrong with the request.

Every failure body is the same shape: `{"error":"one sentence"}`.

`/api/sessions` is built with `ui.SessionRows`, the same call the terminal's
`/resume` picker makes — same snapshots, same hidden children, same labels — so
the two front ends cannot offer different lists. With no session lister wired it
answers `[]`, which is an empty picker and not an error.

### `/api/events` — the SSE framing

```
id: 7
event: frame
data: {"phase":"AUTO-RUN", …}

: ping

```

- **`id` is the hub's `rev`**, which counts *distinct frames* from 1. It does not
  advance while a run sits idle. A client that sees it jump missed frames, and
  there is no replay: the newest frame is the whole state, so the cure for a gap
  is the frame after it.
- **`event: frame`**, `data` the frame JSON on one line. `encoding/json` emits no
  literal newline, so `data:` is never continued.
- The stream is **flushed after every write**. Without that a frame would sit in
  a buffer until the next one arrived, which on an idle run is forever.
- **A comment heartbeat (`: ping`) every ~20s.** An idle acy run emits no frames
  at all — that silence is the point — so without a heartbeat a working
  connection is indistinguishable from a dead one to anything in between. It is a
  comment, not an event; no client handler sees it.
- The first event is the **current frame**, sent on connect, so a webview opened
  halfway through a run renders immediately.
- **`event: done`** is written when the *run* ends (the model quit) as opposed to
  when this connection ends. They mean different things: a closed socket means
  reconnect, `done` means there is nothing left to reconnect to.
- The stream terminates cleanly when the client disconnects (`r.Context()`), when
  the hub is done, and when the server shuts down.

### Auth

Every `/api/*` request must carry `Authorization: Bearer <token>`. The token is
compared with `crypto/subtle.ConstantTimeCompare` — a comparison that returns on
the first differing byte leaks the token one character at a time to anything that
can time a local request, which is every process on this machine.

The token is 256 bits of `crypto/rand`, minted per run unless `--token` supplies
one, and printed once on the stdout line above.

`/healthz` is deliberately outside the check: a parent process needs to know the
listener is up before it has anything else, that is not a secret, and a health
check that needs a credential is one more thing to get wrong at startup. It says
nothing about the run.

The listener binds **127.0.0.1 only**. A non-loopback address is an error, not a
warning: this server hands out the ability to approve tool calls on this machine.

### CORS, deliberately narrow

The webview is a genuinely cross-origin client — served from
`vscode-webview://<uuid>`, fetching `http://127.0.0.1:<port>` — and because every
request carries an `Authorization` header, the browser **preflights** each one
with an `OPTIONS`. That preflight must be answered or nothing works at all: the
real request is never sent, and the failure appears in the webview's console
rather than in acy's log.

So, exactly:

- `Access-Control-Allow-Origin` is **reflected**, and only for an origin matching
  `vscode-webview://` (prefix — the uuid is per webview) or listed in the
  server's `AllowOrigins`. **Never `*`.** A wildcard would tell every page in the
  browser it may talk to this port.
- `OPTIONS` preflights from those origins are answered `204` with
  `Access-Control-Allow-Methods: GET, POST, OPTIONS` and
  `Access-Control-Allow-Headers: authorization, content-type`.
- Anything else gets **no CORS headers whatsoever** and a `403`. The absence is
  the answer; there is nothing there for a page that is probing to learn.
- `Vary: Origin` is always set when an `Origin` is present, so a cache cannot
  serve one origin's answer to another.
- Credentials are **not** allowed. acy authenticates with a header a client has
  to be given, never with an ambient cookie a page would carry automatically.

**The token remains the real defence.** CORS is a browser-side rule and stops no
program holding a socket; what it stops is a *page* — `evil.example` gets no
grant, so it can neither preflight successfully nor read a response, and it never
had the token, which was printed to the process that launched acy.

## `Frame`

| field | type | meaning |
|---|---|---|
| `phase` | string | `"PLAN"`, `"AUTO-RUN"` or `"COMPLETE"` |
| `status` | string | the one-line header state (`"working…"`, `"idle"`, …); prose, not an enum |
| `hint` | `Hint` | the composer hint line: `{text, kind}` |
| `composer` | `Composer` | `{active}` — whether the composer owns the keyboard right now |
| `sessionId` | string | claude's session id; empty until its `init` event |
| `model` | string | the model claude reported at init |
| `billing` | string | `"subscription"`, `"API"`, or `""` when not yet known |
| `ended` | bool | the stream closed; there is nothing left to send to |
| `busy` | bool | a turn, a pending gate or a dispatched task is in flight |
| `processing` | bool | a turn specifically is in flight |
| `planReady` | bool | a plan is on screen waiting to be armed |
| `paused` | bool | every gate countdown is frozen |
| `showHelp` | bool | the help overlay is open |
| `picking` | bool | the resume picker is open |
| `turnStartUnixMs` | int64 | when the in-flight turn began; `0` when idle |
| `cost` | `Cost` | `{parent, child, total}` in USD |
| `tokens` | `Ledger` | the token ledger, split by spender |
| `dispatches` | int | tasks delegated this run — can exceed `tasks.length`, which is trimmed |
| `entries` | `Entry[]` | the transcript, in display order |
| `queue` | `QueueItem[]` | messages held until the session next falls idle |
| `gates` | `Gate[]` | permission requests counting down; `[0]` is the one on screen |
| `ask` | `Ask` or null | the question claude is blocked on |
| `tasks` | `Task[]` | the delegated-task ledger, oldest first |
| `picker` | `SessionRow[]` | the `/resume` rows; empty unless `picking` |
| `engineers` | `Engineer[]` | the architect's fleet ledger, oldest first; empty for a session with no fleet wired |
| `fleet` | `FleetSummary` | `{active, capacityUsed, capacityTotal}` across the fleet's hosts; all zero with no fleet wired |
| `tickets` | `Ticket[]` | the architect's ticket board, sorted by id; empty for a session with no ticket store wired |
| `flow` | `Flow` | the ticket board redrawn as mermaid and ascii; `{mermaid: "", ascii: ""}` for a session with no ticket store wired |
| `interruptedTasks` | string[] | tasks a restart caught mid-flight |
| `logPath` | string | the debug log, if one is open |
| `configPath` | string | the `.acy.json` this run's settings came from |
| `cwd` | string | the project this run belongs to |
| `branch` | string | the current git branch/SHA badge; `""` when disabled or unresolved |
| `finishOutcome` | string | `"completed"` or `"abandoned"`, once the session calls Finish; omitted before then |
| `finishSummary` | string | the summary that came with `finishOutcome`; omitted before then |

Every list field is always an array, never `null`. `finishOutcome` and
`finishSummary` are `omitempty` rather than empty strings, so a client can tell
"not finished yet" from "finished with nothing to say" — check for the field's
presence, not just its value.

### `Hint`

| field | type | meaning |
|---|---|---|
| `text` | string | exactly what the TUI prints under the composer |
| `kind` | string | `gate`, `working`, `busy`, `planReady`, `plan`, `complete`, `default` |

`kind` exists so a client can style the line without re-deriving the condition
that chose it. The selection lives in one place — `composerHint` in
`internal/ui/present.go` — and both front ends call it.

### `Composer`

| field | type | meaning |
|---|---|---|
| `active` | bool | the composer is the surface the keyboard is pointed at |

`active` is `false` while `/help`, the `/resume` picker, an open `Ask`, or the
`/queue` edit overlay owns the keyboard, and `true` otherwise — including while
a gate is pending, since the gate panel stacks above the composer rather than
replacing it. A client should blink its own cursor only while `active` is
`true`; the field itself never changes on its own between two frames of an
idle run.

### `Cost` and `Ledger`

`cost` is `{parent, child, total}`. `parent` is every claude process the
supervisor drove itself; `child` is the dispatched tasks, which report their
spend to the orchestrator rather than through the driver.

`tokens` is `{parent, child, total, context, contextWindow}`, where each of the
first three is `{input, output, cacheCreate, cacheRead}`. `context` is the most
recent turn's context size — a reading, not a running total — and
`contextWindow` is what claude said it fits in, `0` until a `result` event
reports one. The parent/child split is the point: a run that delegates should
show `parent` flat while `child` climbs.

### `Entry`

| field | type | meaning |
|---|---|---|
| `seq` | int | identity of this entry across frames |
| `kind` | string | one of `meta`, `you`, `claude`, `thinking`, `tool`, `toolOK`, `toolErr`, `plan`, `turn`, `complete`, `good`, `warn`, `queued`, `flow` |
| `title` | string | a tool name, where there is one |
| `body` | string | plain text, ANSI stripped |
| `raw` | string | the unhighlighted source behind `body` |
| `lang` | string | language hint for `raw` |
| `html` | string | the entry rendered as a sanitized HTML fragment; `""` unless the run asked for it |
| `task` | string | the delegated task this came from; `""` is the supervisor itself |

`seq` is **monotonic in creation order and never reused**, including across
`/clear` — which empties the transcript but deliberately does not rewind the
counter. It is an *identity*, not a sort key: `entries` already arrives in
display order, and a resumed run's "N earlier entries elided" notice carries a
higher `seq` than the entries below it because it was minted later.

`body` and `raw` differ only for tool calls. The TUI bakes chroma syntax
highlighting into a tool body at ingest, on purpose — it re-renders every entry
on each 120 ms tick, and re-lexing at that rate would burn CPU for nothing — but
escape codes are a terminal's answer. So `body` is that text with the ANSI
stripped, and `raw` is the same text before it was ever highlighted, with `lang`
naming the language:

| tool | `lang` |
|---|---|
| `Bash` | `bash` |
| `Edit` | `diff` |
| `Write` | inferred from the file extension (`go`, `typescript`, `python`, …) |
| everything else | `""` |

The inferred names are [chroma](https://github.com/alecthomas/chroma) lexer names
lowercased, so `.txt` yields the literal `plaintext` rather than `""`. Treat both
`""` and an unrecognised name as plain text; do not treat either as an error.

### `entry.html` — the server-rendered fragment

`html` is the entry as markup, produced by `internal/htmlrender` from the same
`kind`/`title`/`body`/`raw`/`lang` above.

**The client renders no markdown and highlights no code.** Not a suggestion: the
webview's CSP forbids `unsafe-inline`, and shipping a JavaScript markdown and
syntax-highlighting stack into an extension to re-derive what `render.go` already
knows is two implementations of one transcript, which is the thing `Frame` exists
to prevent. If an entry needs to look different, it changes here.

What each kind becomes:

| `kind` | rendering | source field |
|---|---|---|
| `claude`, `plan`, `complete` | markdown (CommonMark + GFM); fenced code goes through chroma | `body` |
| `tool` with a non-empty `lang` | a chroma-highlighted `<pre class="chroma">` block | `raw` |
| `tool` with an empty `lang` | preformatted escaped text — an argument preview is not code | `raw` |
| `toolOK`, `toolErr`, `thinking` | preformatted escaped text | `body` |
| `meta`, `you`, `turn`, `good`, `warn`, `queued` | escaped text, line breaks kept as `<br>` | `body` |
| `flow` | preformatted escaped text — the ascii lanes followed by the fenced mermaid block, the same two halves the terminal shows | `body` |

The shape is one wrapper `<div class="acy-entry acy-entry--<kind>">`, an optional
`<div class="acy-entry__title">` when `title` is non-empty, and the content. The
kind is on the wrapper so a client styles a fragment it was handed rather than
switching on the kind to decide how to build one.

**`html` is empty unless the run asked for it.** It is rendered once, when the
entry is appended — the same bargain the terminal's syntax highlighting makes,
because the transcript re-renders on every 120 ms tick and re-running goldmark
over the history at that rate would burn CPU for nothing — and it is behind
`ui.Config.RenderHTML`, which defaults to false. **A terminal run (`acy run`)
leaves it false and every `html` is therefore `""`**, so the front end that
cannot display markup does not pay to produce it. A server feeding a webview sets
it. An empty `html` is not an error and not a missing value; it is a run that was
never asked.

**Three long blocks are not line-capped here.** The TUI clamps tool output,
results and thinking to `maxLines` because a terminal has a fixed viewport and no
way to scroll inside one entry. `html` carries the whole thing: the client is
expected to collapse and expand it itself.

**Nothing in a fragment carries color.** Styling is class names only, and the
chroma classes are defined by `htmlrender.StyleSheet(ThemeDark|ThemeLight)` — the
dark theme is `dracula`, the same palette `chromaTheme` pins for the terminal, so
the same file highlights the same way in both front ends. A client switches theme
by swapping that one stylesheet and re-renders no entries, which is only possible
because the entries were never colored. It is served at
`GET /api/highlight.css?theme=dark|light`, and it is not optional: a server that
serves frames without it has shipped an unreadable transcript.

#### The fragment is already safe, and it is inert on purpose

Entry bodies are untrusted — model output and raw tool results — so a `git log`
that prints `<script>`, a grep hit carrying `onerror=`, or a markdown link with a
`javascript:` href all reach the renderer verbatim. Two independent things make
the fragment safe, and `internal/htmlrender` tests both adversarially:

- goldmark is never given `WithUnsafe`, so raw HTML in prose is dropped rather
  than passed through, and every non-markdown kind is escaped rather than parsed.
- the result is then run through bluemonday, which knows nothing about how it was
  produced. Its policy is `UGCPolicy` extended in exactly one direction: `class`
  is permitted on `span`, `code` and `pre`, because chroma's class-based
  formatter has nothing but classes and the CSP forbids the inline styles that
  would be the alternative.

So a payload survives as *text* — you can still read what the command printed —
and never as an element or an attribute. A client can insert `html` directly.
That is the point of it being rendered here.

### `QueueItem`

| field | type | meaning |
|---|---|---|
| `id` | int | the identity; `queueEdit`/`queueRemove` name this, never a position |
| `text` | string | the held message's text |

`id` matters for the same reason a gate's `toolUseId` does: the queue flushes
out from under a client the moment the session falls idle, so a client cannot
target "the message at position 2" and expect it to still be that message by
the time its action arrives.

### `Gate`

| field | type | meaning |
|---|---|---|
| `toolUseId` | string | the identity; an action names this, never a position |
| `tool` | string | the tool name as claude reported it |
| `task` | string | the delegated task that raised it; `""` is the supervisor |
| `args` | string | the one-line argument preview the countdown panel shows |
| `deadlineUnixMs` | int64 | when this auto-approves |
| `remainingMs` | int64 | the frozen time left, once `paused` is set |

Exactly one of `deadlineUnixMs` and `remainingMs` is non-zero. While the
countdown runs, only the deadline is set; while `frame.paused` is set, only the
remainder is — the model keeps a stale deadline internally for the resume to
re-derive from, and a client shown that would animate towards a moment that will
never arrive.

### `Ask`

Only the question currently being asked travels: the earlier ones are answered
and the later ones are not being asked yet.

| field | type | meaning |
|---|---|---|
| `header` | string | the short label above the question |
| `question` | string | the question itself |
| `index` | int | 0-based position within the ask |
| `total` | int | how many questions the ask carries |
| `multiSelect` | bool | whether more than one option may be chosen |
| `cursor` | int | the option the TUI has highlighted |
| `options` | `{label, description, selected}[]` | |
| `deadlineUnixMs` | int64 | when the question auto-skips; `0` in PLAN, where a human is present and it may wait forever |

### `Task`

| field | type | meaning |
|---|---|---|
| `id` | string | the task id |
| `title` | string | what the task was asked to do |
| `outcome` | string | how it ended; empty while it is still running |
| `cost` | float | USD |
| `tokens` | `Tokens` | |
| `running` | bool | the task has no end time |

`running` matters: a running task's blank `outcome` and zero `cost` are "not in
yet", not "finished badly", and only this field tells the two apart.

### `Engineer` and `FleetSummary`

An architect session (`--role architect`) delegates whole tickets to remote
engineers — each a fresh acy run on its own machine, in its own worktree —
rather than editing code itself. `engineers` is that ledger, and `fleet` is
capacity across the hosts it runs on.

| field (`Engineer`) | type | meaning |
|---|---|---|
| `id` | string | the engineer's id (`e1`, `e2`, …) |
| `ticket` | string | the ticket key it was launched with |
| `title` | string | a few words naming the task |
| `host` | string | which fleet host it is running on |
| `state` | string | `launching`, `running`, `done`, `failed` or `cancelled` |
| `outcome` | string | how it ended; empty while `state` is not yet terminal |
| `prUrl` | string | the PR it opened, once it has one |
| `costUsd` | float | USD |
| `branch` | string | the branch it is working on |

| field (`FleetSummary`) | type | meaning |
|---|---|---|
| `active` | int | engineers currently `launching` or `running` |
| `capacityUsed` | int | host slots in use across every host |
| `capacityTotal` | int | host slots that exist in total |

Both are empty/zero for a session with no fleet configured — `.acy.json` has
no `fleet` section, or acy was not started in architect mode — which is not an
error, the same way an empty `tasks` ledger is not one.

### `Ticket`

The architect's ticket board, read from the markdown files under
`.acy/tickets` in the project itself rather than from acy's own state
directory — the run's memory, kept current by the model's own
`ReadTickets`/`UpdateTicket` calls (see `internal/mcp/protocol.go`) rather than
inferred by acy. `tickets` is the summary a client lists; the full brief each
ticket carries is what the model itself reads via `ReadTickets`, not part of
this projection.

| field | type | meaning |
|---|---|---|
| `id` | string | the ticket's id |
| `title` | string | its title |
| `status` | string | `todo`, `in-progress`, `in-review`, `merged` or `blocked` |
| `prUrl` | string | the PR it is associated with, once it has one |

Empty for a session with no ticket store wired — `acy arch` is the only
caller that wires one — which is not an error, the same way an empty
`engineers` ledger is not one.

### `Flow`

The same board as `tickets`, redrawn as a diagram rather than listed. It is
the *current* board, not a transcript entry — it is kept up to date on every
frame the same way `tickets` is, independent of the `flow`-kind entries
`/flow` and a ticket milestone append to the transcript.

| field | type | meaning |
|---|---|---|
| `mermaid` | string | the board as a mermaid flowchart source |
| `ascii` | string | the board as a plain-text status-lane summary |

Both are `""` for a session with no ticket store wired — not the rendering of
an empty board, which is what a wired store with zero tickets on it produces.

### `SessionRow`

| field | type | meaning |
|---|---|---|
| `id` | string | claude's session id |
| `modTimeUnixMs` | int64 | last write to the transcript |
| `summary` | string | claude's one-line summary; may be empty |
| `label` | string | acy's own state — phase, task count, cost — empty for a session acy never supervised |
| `selected` | bool | the picker's current row |

An empty `label` is how a plain `claude` session is told apart from a run acy
supervised. It is not a missing value.

## `Action`

An action is one thing a front end asks the model to do. It is a single JSON
object with a `kind` and whatever fields that kind uses; fields belonging to
other kinds are ignored, never inspected.

```json
{ "kind": "gateAllow", "toolUseId": "toolu_01ABC…" }
```

One flat shape rather than a variant per action, because this has to survive
`encoding/json` in both directions and a client assembling one by hand should
not have to model Go's type system to do it.

| `kind` | fields | what it does |
|---|---|---|
| `submit` | `text` | exactly what pressing Enter with `text` in the composer does |
| `arm` | — | Ctrl+G: the plan is approved, start delegating |
| `interject` | — | Esc: interrupt the in-flight turn so you can redirect |
| `gateAllow` | `toolUseId` | approve that one pending gate now |
| `gateDeny` | `toolUseId` | veto that one pending gate |
| `gatePause` | `paused` | freeze (`true`) or resume (`false`) every countdown |
| `askAnswer` | `questionIndex`, `optionIndices` | answer the open question |
| `askSkip` | — | abandon the open question; claude proceeds on its own judgment |
| `resume` | `sessionId` | restore a prior run — transcript, phase and cost |
| `pickerClose` | — | Esc: dismiss the `/resume` picker, resuming nothing |
| `setModel` | `name` | `/model`: the model for the next launched or resumed session |
| `clear` | — | `/clear`: empty the transcript view (not the conversation) |
| `done` | `summary` | `/done`: end the run by hand |
| `queueClear` | — | `/queue clear`: drop every held message, unsent |
| `queueEdit` | `queueId`, `text` | replace the text of the queued message carrying `queueId` |
| `queueRemove` | `queueId` | drop the queued message carrying `queueId`, unsent |
| `quit` | — | stop the driver and exit |

**`queueEdit` with blank text removes instead of refusing.** Editing a message
down to blank or whitespace-only text drops it the same as `queueRemove`
would, rather than being rejected as an empty edit — the two `text` fields
already mean the same "nothing to send", and the accepted `reason` says which
one happened (`"empty edit removed the queued message"` vs. `"queued message
updated"`).

### `ActionResult`

Every action is answered exactly once.

| field | type | meaning |
|---|---|---|
| `accepted` | bool | whether the model did it |
| `reason` | string | one sentence, in both the accepted and the rejected case |

`reason` is populated on success too, because "sent" and "queued until the
session falls idle" are different outcomes of the same accepted `submit` and a
client that could only see the boolean would have to re-derive which one
happened from the next frame.

**The acknowledgement can be dropped, and never blocks.** The model runs on one
goroutine — the terminal, every countdown and the driver reader are all behind
it — so a send into an acknowledgement channel nobody is reading is skipped
rather than waited on. A hung client costs itself its answer; it must not be
able to cost the run. A client that misses an acknowledgement can still see what
happened in the next `Frame`.

### Refusals

A refusal is a normal outcome, not an error: a client is describing a run it
last saw a moment ago, and that run moves on its own. Where the TUI already
prints something for a refusal, the action leaves that same transcript entry —
there is one implementation, so there is one wording.

| `kind` | refused when |
|---|---|
| `submit` | `text` is blank; the session has ended; there is no driver. A `/command` is routed regardless of `ended` — `/quit` and `/tokens` are exactly what you still want then |
| `arm` | the phase is not `PLAN`; there is no `sessionId` yet (claude emits none until the first user message); there is no driver — a resume knows its id before its process exists, and arming into that gap would launch a second claude for the same session |
| `interject` | a gate is pending; there is no driver; nothing is in flight |
| `gateAllow` / `gateDeny` | no pending gate carries that `toolUseId` — unknown, or already resolved |
| `gatePause` | never; it is idempotent, and `reason` says whether anything changed |
| `askAnswer` | no question is open; `questionIndex` is not the question currently being asked; `optionIndices` is empty, out of range, or holds more than one option for a single-select question |
| `askSkip` | no question is open |
| `resume` | `sessionId` is empty |
| `pickerClose` | the picker is not open |
| `setModel` | `name` is empty |
| `done` | the run is already `COMPLETE` |
| `queueEdit` / `queueRemove` | no queued message carries that `queueId` — already flushed to claude, or never existed (`"that message has already gone out"`) |
| `clear`, `queueClear`, `quit` | never |
| anything else | the `kind` is not one of the above |

The `interject` refusal deserves its reason spelled out: the PreToolUse hook
that raised the pending gate is **blocked on the gate socket** waiting for a
decision, and interrupting the turn out from under it is an unanswered-hook
deadlock. Answer the gate first (`gateAllow`/`gateDeny`), interject after. The
terminal suppresses Esc for the same reason; the action refuses it because an
HTTP caller never passes through the terminal's key routing.

### Answering a question

`askAnswer` names its question by index for the same reason a gate names its
`toolUseId`: in `AUTO-RUN` a question auto-skips on a countdown, and the panel
advances a question at a time as answers arrive. `frame.ask.index` is the only
value that will be accepted. `optionIndices` are positions in
`frame.ask.options`, one for a single-select question and one or more for a
`multiSelect` one.

Answering the last question submits the whole ask back to claude and unblocks
its turn; answering any earlier one advances the panel and the `reason` says so.
