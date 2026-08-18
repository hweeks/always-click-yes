# codex CLI findings — reconnaissance for a second acy backend

Recon only. No Go/TypeScript changed. Every claim below is labeled `VERIFIED LIVE`,
`VERIFIED FROM --help/docs`, or `UNKNOWN`, with the exact command that produced it.

## Setup

- `command -v codex` → `/Users/hammie/.local/bin/codex`; `codex --version` → `codex-cli 0.146.0`.
  Already installed on this machine — nothing installed by this recon.
- **Authenticated.** `codex login status` → `Logged in using ChatGPT` (exit 0). This means
  everything below marked `VERIFIED LIVE` is a real model round trip, not a guess dressed up
  as one — including two live sessions that spent real usage against this account's ChatGPT
  plan (`account/rateLimits/updated` showed `"planType":"team"`, `usedPercent` stayed at 3%
  across the whole recon).
- No interactive login was attempted. No `~/.codex/config.toml` write was made — every probe
  used either the real (untouched) `CODEX_HOME` for read-only inspection, or passed
  configuration live over the `app-server` JSON-RPC channel (`thread/start`'s `config` /
  `sandbox` / `approvalPolicy` params) or via `codex exec -c key=value`, never by editing the
  file. `~/.codex/config.toml` was read once to confirm it exists and holds per-project trust
  levels and an MCP server entry — its contents are not reproduced here because one entry
  contains a live API token; that token was not otherwise touched, transmitted, or used.
- Fixtures captured, all under `docs/codex-fixtures/`:
  - `app-server-session.ndjson` — one live `codex app-server` process, one thread, two turns
    over stdio: turn 1 reads a file cleanly; turn 2 asks the shell to write a file under
    `sandbox: read-only`, which raises a real blocking `item/commandExecution/requestApproval`
    request mid-turn, which this recon answered with `{"decision":"decline"}`, and the model
    gracefully continues and reports the denial. This is the single most important fixture —
    it is the approval gate, live, byte for byte, both directions.
  - `exec-trivial-read.jsonl` — `codex exec --json` (the older one-shot, non-bidirectional
    surface) reading the same file. Flat `type: "thread.started"/"item.completed"/...` shape,
    snake_case fields — a different, simpler wire format than `app-server`'s.
  - `exec-approval-required.jsonl` — the same `exec` surface, but under
    `sandbox=read-only` + `-c approval_policy="on-request"`, asked to write a file. **No
    approval-request event of any kind appears in the stream.** The write just fails at the
    sandbox boundary and the model reports failure in the same turn. This is the proof that
    `exec` cannot host acy's gate at all (see Q3).
  - `rollout-transcript-sample.jsonl` — the on-disk session transcript claude-equivalent
    (`~/.codex/sessions/2026/08/18/rollout-...jsonl`) for the `app-server` session above, for
    Q7.

---

## 1. Non-interactive modes

`VERIFIED FROM --help/docs` (`codex --help`, then `--help` on each), cross-checked live.

Top-level subcommands (`codex --help`, `codex-cli 0.146.0`):

```
Commands:
  exec            Run Codex non-interactively [aliases: e]
  review          Run a code review non-interactively
  login           Manage login
  logout          Remove stored authentication credentials
  mcp             Manage external MCP servers for Codex
  plugin          Manage Codex plugins
  mcp-server      Start Codex as an MCP server (stdio)
  app-server      [experimental] Run the app server or related tooling
  remote-control  [experimental] Manage the app-server daemon with remote control enabled
  app             Launch the Desktop app (opens the app installer if missing)
  completion      Generate shell completion scripts
  update          Update Codex to the latest version
  doctor          Diagnose local Codex installation, config, auth, and runtime health
  sandbox         Run commands within a Codex-provided sandbox
  debug           Debugging tools
  apply           Apply the latest diff produced by Codex agent as a `git apply` to your local
                  working tree [aliases: a]
  resume          Resume a previous interactive session (picker by default; use --last to continue
                  the most recent)
  archive / delete / unarchive / fork
  cloud           [EXPERIMENTAL] Browse tasks from Codex Cloud and apply changes locally
  exec-server     [EXPERIMENTAL] Run the standalone exec-server service
  features        Inspect feature flags
```

**There is no `codex proto` in this build.** AGENTS.md-style docs for older codex versions
describe a `codex proto` (raw JSON-lines protocol over stdio); it is simply absent from
`codex --help` on 0.146.0 and from every subcommand list probed. `app-server` has superseded
it — see Q2.

Headless/scriptable candidates, full `--help` text:

<details><summary><code>codex exec --help</code></summary>

```
Run Codex non-interactively

Usage: codex exec [OPTIONS] [PROMPT]
       codex exec [OPTIONS] <COMMAND> [ARGS]

Commands:
  resume  Resume a previous session by id or pick the most recent with --last
  review  Run a code review against the current repository
  help    Print this message or the help of the given subcommand(s)

Arguments:
  [PROMPT]  Initial instructions for the agent. If not provided as an argument (or if `-` is
            used), instructions are read from stdin. If stdin is piped and a prompt is also
            provided, stdin is appended as a `<stdin>` block

Options:
  -c, --config <key=value>      Override a config value (dotted path, TOML-parsed)
      --enable/--disable <FEATURE>
  -i, --image <FILE>...
  -m, --model <MODEL>
      --oss / --local-provider <OSS_PROVIDER>
  -p, --profile <CONFIG_PROFILE_V2>
  -s, --sandbox <SANDBOX_MODE>   [possible values: read-only, workspace-write, danger-full-access]
      --dangerously-bypass-approvals-and-sandbox
      --dangerously-bypass-hook-trust
  -C, --cd <DIR>
      --add-dir <DIR>
      --skip-git-repo-check
      --ephemeral                Run without persisting session files to disk
      --ignore-user-config
      --ignore-rules
      --output-schema <FILE>     Path to a JSON Schema file describing the model's final response shape
      --color <COLOR>            [default: auto]
      --json                     Print events to stdout as JSONL
  -o, --output-last-message <FILE>
```

Notably **absent from `exec --help`: `-a/--ask-for-approval`.** It exists on the top-level
`codex`, on `resume`, and on `fork`, but not on `exec` or `exec resume` or `exec review` — see
Q3/Q1 interaction below.
</details>

<details><summary><code>codex app-server --help</code></summary>

```
[experimental] Run the app server or related tooling

Usage: codex app-server [OPTIONS] [COMMAND]

Commands:
  daemon                Manage the local app-server daemon
  proxy                 Proxy stdio bytes to the running app-server control socket
  generate-ts           [experimental] Generate TypeScript bindings for the app server protocol
  generate-json-schema  [experimental] Generate JSON Schema for the app server protocol
  help

Options:
      --listen <URL>    Transport endpoint URL. Supported: `stdio://` (default), `unix://`,
                        `unix://PATH`, `ws://IP:PORT`, `off`   [default: stdio://]
      --stdio           Use stdio as the transport (equivalent to --listen stdio://)
      --code-mode-host <WS_URL>
      --analytics-default-enabled
      --ws-auth <MODE>  [possible values: capability-token, signed-bearer-token]
      --ws-token-file / --ws-token-sha256 / --ws-shared-secret-file / --ws-issuer / --ws-audience
      --ws-max-clock-skew-seconds <SECONDS>
  -c, --config <key=value> / --enable / --disable / --strict-config
```

`codex app-server` with no subcommand and default `--listen stdio://` is a bare, long-lived
JSON-RPC-over-stdio process — this is the bidirectional surface. `daemon` manages a
*persistent* background instance reachable over `unix://`/`ws://` instead of one-per-invocation
stdio (relevant if acy ever wants one app-server serving multiple concurrent sessions rather
than one process per acy run).
</details>

<details><summary><code>codex mcp-server --help</code></summary>

```
Start Codex as an MCP server (stdio)
```
This is codex acting as an **MCP server** (offering itself as a tool to some other MCP
*client*, e.g. an IDE) — the inverse of Q5's question (codex acting as an MCP *client* of
acy's own server). Not the same surface; noted for completeness so a future reader doesn't
conflate the two.
</details>

<details><summary><code>codex debug --help</code> and children</summary>

```
Commands:
  models        Render the raw model catalog as JSON
  app-server    Tooling: helps debug the app server (send-message-v2)
  prompt-input  Render the model-visible prompt input list as JSON
```
Not a runtime backend, but load-bearing for this recon: `debug prompt-input` rendered the
exact model-visible message list (including AGENTS.md injection, see Q6) with **no model call
and no cost**, and `debug models` dumped the full model/reasoning-effort catalog the same way.
</details>

**Conclusion:** two real headless surfaces exist — `codex exec` (one-shot, fire-and-forget,
JSONL or plain text output, `--output-schema` for structured output) and `codex app-server`
(long-lived, bidirectional, JSON-RPC over stdio/unix/ws). `codex mcp-server` is codex-as-a-tool,
not a driving surface. `codex proto` does not exist in 0.146.0.

---

## 2. Bidirectional streaming

`VERIFIED LIVE` (`docs/codex-fixtures/app-server-session.ndjson`, captured via a hand-rolled
Python stdio client — no shell timeout binary was needed for this one since it drove the pipes
directly with `select`).

**Yes: `codex app-server` (default `--listen stdio://`).** It is **newline-delimited JSON,
not Content-Length-framed** — confirmed by sending one bare JSON line and reading one bare
JSON line back with no framing headers at all. It carries no `"jsonrpc":"2.0"` field in
either direction (checked against the schema and the live bytes) — despite calling itself
JSON-RPC-shaped (`id`/`method`/`params`, and responses via `id`/`result` or `id`/`error`), it
skips the version tag real JSON-RPC 2.0 requires.

**A second user turn was injected into the same live process without relaunching** — this is
exactly the capability acy's "arm in place" needs. Sequence, all on one `codex app-server`
process, one `threadId`:

1. `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"acy-recon","version":"0.0.1"}}}`
   → `{"id":1,"result":{"userAgent":"...","codexHome":"/Users/hammie/.codex",...}}`
2. `{"id":2,"method":"thread/start","params":{"cwd":"/tmp/codex-scratch","approvalPolicy":"on-request","sandbox":"read-only"}}`
   → `{"id":2,"result":{"thread":{"id":"01a01547-...",...},"model":"gpt-5.6-terra",...}}`
3. `{"id":3,"method":"turn/start","params":{"threadId":"01a01547-...","input":[{"type":"text","text":"Read the file hello.txt..."}]}}`
   → streamed `item/*` events, ending in `turn/completed`.
4. **Same process, same threadId**, a second `{"id":4,"method":"turn/start","params":{"threadId":"01a01547-...","input":[{"type":"text","text":"Now use the shell to create a new file..."}]}}` — no relaunch, no re-`initialize`, no second `thread/start`. This is byte-for-byte in the fixture (lines 29+).

**User-message submission, byte for byte** (from the fixture, request id 3):
```json
{"id": 3, "method": "turn/start", "params": {"threadId": "01a01545-9c6c-7dd3-9178-415ac026722c", "input": [{"type": "text", "text": "Read the file hello.txt in your working directory and tell me exactly what it says. Do not do anything else."}]}}
```
`input` is an array of a tagged union (`UserInput`); the only variant used here is
`{"type":"text","text":"..."}` — others in the schema are `image`, `localImage`, `localAudio`, etc.

There is also `turn/steer` — `{"expectedTurnId","threadId","input"}` — which redirects an
**already-active** turn (with a precondition on the turn id, so a stale steer fails loudly
instead of landing on the wrong turn) rather than queuing behind it. See "Capability gaps" for
what this means against acy's own queue-and-flush model.

**Event types observed on the wire** (method field of server→client messages, this session):
`remoteControl/status/changed`, `thread/started`, `mcpServer/startupStatus/updated`,
`thread/status/changed`, `turn/started`, `item/started`, `item/completed`,
`item/agentMessage/delta` (streaming text deltas), `thread/tokenUsage/updated`,
`account/rateLimits/updated`, `turn/completed`, and — mid-turn — the blocking
`item/commandExecution/requestApproval` (see Q3). Item `type`s seen: `userMessage`,
`reasoning`, `commandExecution`, `agentMessage`.

The full request/notification method catalog (`codex app-server generate-json-schema --out
<dir> --experimental`, itself a local, no-model-call, no-cost operation) lists **127
`ClientRequest` methods** and **11 `ServerRequest` methods** — `thread/*`, `turn/*`, `fs/*`,
`mcpServer/*`, `account/*`, `plugin/*`, `skills/*`, `remoteControl/*`, etc. `--experimental`
was needed to include the newer `thread/turn/item`-shaped methods used above alongside an
older, flatter `execCommandApproval`/`applyPatchApproval` pair that still exists in the schema
(`ServerRequest.json`) — both were generated together, so which one an *unflagged* production
`app-server` actually speaks by default was not independently isolated; what's certain is that
the newer `thread/*`/`turn/*`/`item/*` methods used above worked live, unflagged, on a plain
`codex app-server` invocation with no `--enable`.

---

## 3. The approval gate

`VERIFIED LIVE` for `app-server`; `VERIFIED LIVE` (absence) for `exec`.

**Yes, `app-server` blocks — for real, mid-turn, on a request the client must answer over the
same stdio channel.** Fixture `app-server-session.ndjson`, lines 47–49 then the reply:

The turn's status flips to `"active"` with `"activeFlags":["waitingOnApproval"]`, then:

```json
{"method":"item/commandExecution/requestApproval","id":0,"params":{
  "threadId":"01a01545-9c6c-7dd3-9178-415ac026722c",
  "turnId":"01a01546-13a1-7390-af66-b3178126df17",
  "itemId":"exec-0186bb29-8cf1-4eda-9c22-42fe25d9dc68",
  "startedAtMs":1787063313830,
  "environmentId":"local",
  "reason":"Do you want to allow me to create new.txt in the working directory and verify its contents?",
  "command":"/bin/zsh -lc \"printf 'hi\\n' > new.txt && test -f new.txt && sed -n '1p' new.txt\"",
  "cwd":"/tmp/codex-scratch",
  "commandActions":[{"type":"unknown","command":"printf 'hi\\n' > new.txt && test -f new.txt && sed -n '1p' new.txt"}],
  "proposedExecpolicyAmendment":["/bin/zsh","-lc","printf 'hi\\n' > new.txt && test -f new.txt && sed -n '1p' new.txt"],
  "availableDecisions":["accept",{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["/bin/zsh","-lc","..."]}},"cancel"]
}}
```

This is itself a **JSON-RPC request FROM the server**, with its own `id` (here `0`, in a
namespace separate from the client's own request ids) — the client must reply with a
`JSONRPCResponse` on the *same* stdio stream, keyed by that `id`:

```json
{"id": 0, "result": {"decision": "decline"}}
```

`decision` (`CommandExecutionApprovalDecision`, from `generate-json-schema`'s
`CommandExecutionRequestApprovalResponse.json`) is one of: `"accept"`, `"acceptForSession"`
(approve for the rest of the session), `{"acceptWithExecpolicyAmendment":{...}}` (approve +
persist a rule), `{"applyNetworkPolicyAmendment":{...}}`, `"decline"` ("continue the turn, try
something else"), `"cancel"` ("deny, and interrupt the turn immediately"). Sent `"decline"`
live; the model received it, said *"I couldn't create the file because permission to write was
declined,"* and the turn completed normally — `new.txt` was never created (`ls
/tmp/codex-scratch` confirmed). The generated schema also documents an older, parallel
flat-named pair, `execCommandApproval`/`ExecCommandApprovalResponse`, with an equivalent
`ReviewDecision` enum plus a `"timed_out"` and bare `"abort"` value not present in the newer
one — one of these two request shapes is almost certainly what a non-`--experimental` build
actually sends; this recon did not isolate which (see the caveat at the end of Q2).

There is a second, distinct approval surface: `item/permissions/requestApproval`
(`PermissionsRequestApprovalParams`) for filesystem/network *permission-profile* escalation
requests, separate from `item/commandExecution/requestApproval`/`item/fileChange/requestApproval`
for shell/patch approvals. Both are real `ServerRequest` variants in the schema; only the
command-execution one was actually triggered live in this recon.

**`codex exec` (the `-p`-shaped, one-shot surface) has no equivalent at all.** Live proof,
`docs/codex-fixtures/exec-approval-required.jsonl`: run with `-s read-only -c
'approval_policy="on-request"'`, asked to write a file — no approval-request event of any kind
appears on the JSONL stream. The command fails at the sandbox boundary
(`operation not permitted`), the model reports the failure in the same turn, and the process
exits 0. **This is a fire-and-relaunch surface, not a fire-and-forget-with-a-hook one — there
is no hook at all.** It cannot host acy's countdown gate under any flag combination found,
because there is nothing for acy to answer. (`-a/--ask-for-approval` isn't even accepted by
`exec` — see Q1 — which is consistent with this: the flag would have nothing to attach to.)

Flags controlling this, top-level and on `exec`/`resume`/`fork`:
- `--ask-for-approval`/`-a` ∈ `{untrusted, on-request, never}`, or a `granular` object
  (`{granular: {sandbox_approval, rules, mcp_elicitations, request_permissions,
  skill_approval}}` — all booleans) for fine-grained gating of specific approval categories.
  Present on the interactive/`resume`/`fork` surfaces; **absent from `exec`**.
- `--sandbox`/`-s` ∈ `{read-only, workspace-write, danger-full-access}` — see Q4, this is
  orthogonal to approval, not a substitute for it.
- `--dangerously-bypass-approvals-and-sandbox` — skips both entirely; explicitly labeled
  dangerous in the help text, for externally-sandboxed environments only.

---

## 4. Restricting the tool registry

`VERIFIED LIVE` + `VERIFIED FROM --help/docs`. This is the one where acy's headline guarantee
does **not** survive unchanged.

**Codex has no concept of an absent tool.** There is no flag or app-server param anywhere in
the 127-method `ClientRequest`/`TurnStartParams`/`ThreadStartParams` schema that removes the
shell-execution capability from the model's registry the way claude's `--tools Read,Grep,Glob`
removes `Write`/`Edit`/`Bash` outright. What exists instead, confirmed both from the
`SandboxPolicy` schema and live behavior:

- **`--sandbox`/`sandboxPolicy`** is a filesystem+network jail wrapped *around* a shell tool
  that always exists and is always callable: `readOnly` (`networkAccess: bool`,
  default false), `workspaceWrite` (`writableRoots`, `networkAccess`, `excludeSlashTmp`,
  `excludeTmpdirEnvVar`), `dangerFullAccess`, `externalSandbox`. Every variant still lets the
  model *attempt* a shell command — `readOnly` just makes a write attempt fail at the OS
  sandbox boundary (`operation not permitted`, as seen live in Q3's `exec` fixture) rather than
  the tool not existing.
- **`--ask-for-approval`/`approvalPolicy`** is the other, orthogonal knob: whether an attempt
  that the sandbox *would* allow (or that needs escalation past the sandbox) requires a human
  decision first. It gates *when a call happens*, not *whether the tool exists*.
- There is a **named "permission profile"** concept (`-P/--permission-profile` on `codex
  sandbox`; `permissions` field — a profile id string — on `ThreadStartParams`/
  `TurnStartParams`, mutually exclusive with `sandbox`/`sandboxPolicy`) that composes
  fine-grained filesystem/network rules (`RequestPermissionProfile` →
  `AdditionalFileSystemPermissions.entries[]` of `{path, access: read|write|deny}`,
  `AdditionalNetworkPermissions.enabled`). This is a richer sandbox description language, not
  a tool-registry filter — it still describes what a still-present shell tool may touch, never
  what tools exist.
- Grepping every schema file this recon captured for a "tools" or "toolRegistry"-shaped field
  on `ThreadStartParams`/`TurnStartParams` (the two places a per-session override would have
  to live) found none.

**Determined by:** reading `ThreadStartParams`/`TurnStartParams`/`SandboxPolicy` from
`codex app-server generate-json-schema`'s output directly (no live call needed for this part),
then confirming live in Q3's fixtures that a *sandboxed* write attempt still produces a real
`commandExecution` item (the tool ran, and was then denied/blocked at the sandbox or approval
layer) rather than the model having no shell tool to reach for at all.

**Consequence stated plainly:** acy's "Write/Edit/Bash are not in the registry at all" is a
structural guarantee with no codex analog. The nearest codex equivalent — sandbox +
approval-policy layered together — is enforcement *around* an ever-present tool, which is
weaker in kind, not just in degree: a bug in the sandboxing or a `dangerouslyBypass*` flag
removes a *constraint*, where on claude's side the same mistake would have nothing to remove
(there is no flag that makes `Write` reappear in a `--tools Read,Grep,Glob` registry).

---

## 5. MCP client support

`VERIFIED LIVE` for the per-invocation config mechanism; `VERIFIED FROM --help/docs` +
schema-only (not live-exercised) for the approval-passthrough question.

**Yes, per-invocation, no `~/.codex/config.toml` edit required — two independent mechanisms:**

1. **CLI:** `-c 'mcp_servers.<name>.command="..."'` (a dotted-path `-c` override, same
   mechanism the built-in `--help` examples show for `sandbox_permissions` and
   `shell_environment_policy.inherit`). Not independently live-tested against `exec` in this
   recon (see mechanism 2, which was), but it is the same override syntax already proven to
   change nested config via `-c approval_policy="on-request"` in Q3, so this is high-confidence
   from the documented dotted-path/TOML-value semantics rather than fully live-verified for
   this specific key.
2. **`app-server`, live-verified:** `thread/start`'s `params.config` field
   (`type: object, additionalProperties: true` — an arbitrary raw-config overlay scoped to
   that one thread) accepts an `mcp_servers` table directly in the JSON-RPC call:
   ```json
   {"id":2,"method":"thread/start","params":{"cwd":"/tmp/codex-scratch","approvalPolicy":"on-request","sandbox":"read-only","config":{"mcp_servers":{"acyrecon":{"command":"/bin/cat","args":[]}}}}}
   ```
   Sent live; the immediate response and following notifications included
   `{"method":"mcpServer/startupStatus/updated","params":{"threadId":"...","name":"acyrecon","status":"starting",...}}` — codex spun up a process for the ad hoc server. (It never
   reached `"ready"` because `/bin/cat` isn't a real MCP server and never completes the
   handshake — the point of the probe was only to prove the name/command were accepted and
   acted on, not to exercise a working tool call.) `~/.codex/config.toml` was not written —
   confirmed by re-reading it after the probe.
3. **`codex mcp add <name> -- <command> [args...]`** (or `--url` for streamable HTTP) is the
   *persistent*, global alternative — it does write to `~/.codex/config.toml`. Deliberately
   not run in this recon, to honor "don't touch the user's real config."

**A structurally interesting third option, found in the schema, not the CLI:** app-server's
`thread/start` accepts a `dynamicTools` array (`FunctionDynamicToolSpec`:
`{type:"function", name, description, inputSchema}`, or a `NamespaceDynamicToolSpec` grouping
several). When the model calls one, the **server sends the client a request**,
`item/tool/call` (`DynamicToolCallParams: {callId, threadId, turnId, tool, arguments,
namespace?}`), and the client answers with `DynamicToolCallResponse: {success, contentItems[]}`
— all over the same `app-server` stdio channel, no separate MCP server process, no
`--mcp-config` file. This is schema-only in this recon (never actually invoked live — doing so
would need a real turn where the model chooses to call it, which wasn't provoked); it is worth
flagging because it is a more direct structural analog to what acy's own MCP server
(`Dispatch`/`AskUserQuestion`/`PresentPlan`) does than routing through real MCP would be. It
was present in the schema without `--experimental`-only markers beyond what the whole
generation run required, so its stability across codex releases is unknown — treat it as
promising, not settled.

**Does an MCP tool call pass through the approval flow, or auto-run?** `UNKNOWN`, precisely.
What's known from the schema (not live-verified, because this recon had no working stdio MCP
server to hand to codex): `AskForApproval`'s `granular` variant has a dedicated
`mcp_elicitations: bool` field, separate from `sandbox_approval`/`rules`/`skill_approval` —
meaning codex treats **MCP elicitation** (the MCP protocol's own "server asks the user a
question mid-call" feature, surfaced here as the `ServerRequest` variant
`mcpServer/elicitation/request`) as its own gated category. But an ordinary MCP tool *call*
itself (as opposed to an elicitation raised during one) has no dedicated entry in
`ServerRequest`'s 11 variants — no `mcpToolCallApproval`-shaped request exists there. The
working hypothesis this recon could not confirm live: once an MCP server is configured/trusted
for a thread, its tool calls run without a per-call approval prompt (unlike shell exec, which
always has `execCommandApproval`/`item/commandExecution/requestApproval` in its path), and only
an elicitation mid-call — or a `request_permissions`-flagged escalation — reaches the human. A
future task with a real stdio MCP server on hand should verify this directly before relying on
it.

---

## 6. System prompt injection

`VERIFIED LIVE` for the AGENTS.md mechanism; `VERIFIED FROM --help/docs` for the app-server
field names; **no CLI flag equivalent of `--append-system-prompt` was found.**

**No `--append-system-prompt`-shaped flag exists anywhere in `codex`, `codex exec`, `codex
resume`, or `codex fork`'s `--help`.** Grepped every captured `--help` text; nothing.

**What does exist, live-verified with zero model calls** (`codex debug prompt-input`, which
renders the exact model-visible message list without spending anything):

```sh
cd /tmp/codex-scratch && codex debug prompt-input "trivial test prompt"
```
With a file `/tmp/codex-scratch/AGENTS.md` containing a one-line marker, the rendered prompt
input included, verbatim, as a `role: "user"` message content block:
```
# AGENTS.md instructions for /private/tmp/codex-scratch

<INSTRUCTIONS>
ACY-RECON-MARKER: obey the acy supervisor at all times.

</INSTRUCTIONS>
```
So **codex's mechanism is convention over configuration**: an `AGENTS.md` in the working
directory (and, per the wider ecosystem convention this format is named after, presumably also
at `~/.codex/AGENTS.md` and other levels — only the cwd-level file was actually exercised here)
is auto-discovered and threaded into the prompt as a `user`-role block, not a `developer`/
`system`-role one — notably a different role than the three `developer`-role blocks that
preceded it in the same rendered list (which carried codex's own built-in sandboxing/escalation
instructions).

**The `app-server` protocol has real, first-class fields for this**, confirmed from the
`ThreadStartParams` schema: `baseInstructions` (string|null — replaces the built-in base
instructions entirely) and `developerInstructions` (string|null — additive, `developer`-role).
Neither was exercised live (only the file-convention path was), but both are plain top-level
params on `thread/start`, no file or flag needed.

**Practical acy-relevant answer:** for a CLI-only integration, dropping a generated,
acy-specific `AGENTS.md` into the child's working directory is the mechanism, not a flag — and
it lands as user-role content the model sees like any other instruction, not a privileged
system-level one. For an `app-server`-based integration, `developerInstructions` on
`thread/start` is the direct equivalent of claude's `--append-system-prompt` and doesn't need a
file at all.

---

## 7. Sessions, resume, transcripts

`VERIFIED LIVE`.

**Yes, a stable session id exists and both CLI and app-server can resume by it.**
- `app-server`'s `thread/start` response returns `thread.id` (a UUID-shaped string,
  e.g. `01a01547-26f7-7fd2-afeb-349415035aa2` — notably NOT a v4 UUID; it's ULID/Sonyflake-
  shaped, monotonically increasing, observed live across three separate threads in this recon:
  `...1545...`, `...1546...`, `...1547...`, `...1549...`, `...154a...` in creation order).
  `thread/resume` accepts that id (by id, by `history` array, or by on-disk `path` — precedence
  documented in the schema as history > non-empty path > thread_id) and — per its own
  description field — if the thread is *still running*, "app-server rejoins that thread,"
  which is the app-server-native version of acy's "arm in place."
- CLI: `codex resume [SESSION_ID] [--last]` (interactive picker or direct), `codex exec resume
  [SESSION_ID] [--last] [PROMPT]` (the non-interactive equivalent — confirmed via `--help`,
  not exercised live in this recon), `codex fork [SESSION_ID] [--last]` (branches a new thread
  from an existing one — no claude equivalent; `--fork-session` on claude is closer to a
  request flag than a top-level command).

**On-disk transcript**, confirmed live at
`~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<timestamp>-<thread-id>.jsonl` (copied as
`docs/codex-fixtures/rollout-transcript-sample.jsonl`, 34 lines for the two-turn
approval-round-trip session). Per-line shape: `{"timestamp": "...", "type": "<kind>",
"payload": {...}}`. `type` values actually seen in this one file: `session_meta`, `event_msg`
(with nested `payload.type` of `task_started`, `token_count`, `task_complete`, and others),
`response_item` (with nested `payload.type` of `message` (`role`: `developer`/`user`/
`assistant`), `reasoning`, `custom_tool_call`, `custom_tool_call_output`), `turn_context`.
This is structurally similar in spirit to claude's "superset of the stream-json event, replay
by parsing the same shapes" design, but **the field names, casing, and nesting are all
different from claude's** (`session_meta`/`event_msg`/`response_item`/`turn_context` wrapper
vs. claude's flat `{"type":"user"|"assistant","message":{...}}`), so
`internal/session/replay.go`'s parser cannot be reused as-is — a codex backend would need its
own decoder reading this shape, not a shared one. Unlike claude's transcript (which has **no**
`result` records and thus no in-band cost data), codex's **does** carry both per-turn and
cumulative token counts and rate-limit/plan info directly in `event_msg.payload.type ==
"token_count"` lines — replay could recover cost/usage history that claude's format cannot.
`turn_context` lines additionally carry the full sandbox/approval/model/effort config in effect
for that turn, which claude's transcript also doesn't expose per-turn.

---

## 8. Interrupt

`VERIFIED FROM --help/docs` (schema); not exercised live in this recon (no in-flight turn was
interrupted — the two live turns captured were left to finish or blocked on approval, never
aborted mid-stream).

**`turn/interrupt`** — `TurnInterruptParams: {"threadId": string, "turnId": string}` — a plain
`ClientRequest` on the `app-server` channel. No claude-style `control_request` envelope
wrapping it; it's just another top-level RPC method with its own `id`.

There is also **`turn/steer`** — `TurnSteerParams: {"expectedTurnId", "threadId", "input"}` —
which is not an interrupt but a *redirect*: it feeds new input into a turn that is still
running, gated by `expectedTurnId` (the call fails if the active turn has already moved on).
This has no claude/acy equivalent today; acy's Esc-interject aborts the turn and queues text
for the *next* one, where `turn/steer` appears designed to alter the *current* one without
ending it. See "Capability gaps."

`exec` (the one-shot surface): no interrupt concept applies — there is one turn, and the
process either finishes or is killed by the OS. Killing the process is the only "interrupt"
`exec` has, same as claude's raw process being SIGKILLed rather than sent a `control_request`.

---

## 9. Usage, cost and budget

`VERIFIED LIVE` for token accounting; `VERIFIED LIVE` (absence) for a USD figure; `VERIFIED
FROM --help/docs` for the absence of a spend-ceiling flag.

**Token usage, both per-turn and cumulative, including cached tokens — yes, and split more
finely than claude's.** Live, from `app-server-session.ndjson`:
```json
{"method":"thread/tokenUsage/updated","params":{"threadId":"...","turnId":"...","tokenUsage":{
  "total":{"totalTokens":33266,"inputTokens":33153,"cachedInputTokens":22016,"cacheWriteInputTokens":0,"outputTokens":113,"reasoningOutputTokens":40},
  "last":{"totalTokens":16652,"inputTokens":16643,"cachedInputTokens":11008,"cacheWriteInputTokens":0,"outputTokens":9,"reasoningOutputTokens":0},
  "modelContextWindow":258400
}}}
```
Both `total` (cumulative for the thread) and `last` (this turn alone) are present **in the same
event**, unlike claude where per-turn (`usage`) and cumulative (`modelUsage`) are two separate
top-level fields on two different things (a `result` event's two fields) that accumulate
oppositely and are easy to mix up (acy's own driver code has a comment scar about exactly this).
codex additionally reports `cacheWriteInputTokens` and `reasoningOutputTokens` as their own
fields, both absent from claude's `Usage` struct. The on-disk transcript's `token_count` event
carries the identical `total_token_usage`/`last_token_usage` pair (snake_case), so replay can
recover this history too (Q7).

**No USD figure anywhere.** Grepped every captured fixture and every generated schema file for
a cost/USD-shaped field; the only hits were coincidental substring matches (e.g. `...usD` inside
an unrelated camelCase identifier), not a real field. Confirmed instead: `account/rateLimits/
updated` reports **percentage-of-window usage** (`primary: {usedPercent, windowDurationMins,
resetsAt}`), a `credits: {hasCredits, unlimited, balance}` object, and `planType` (observed
live as `"team"`) — this is a ChatGPT-plan-relative signal, not a dollar amount, and it is
account-wide, not scoped to the thread/turn that triggered it.

**No `--max-budget-usd`-shaped flag.** Not on `codex`, `exec`, `resume`, `fork`, or `app-server`
`--help`. The schema does have a *token*-denominated budget, but it belongs to a different,
unrelated feature: `ThreadGoal` (an autonomous "goal" object; `tokenBudget: integer|null`,
`ThreadGoalStatus` enum including `"budgetLimited"`), which is about a long-running background
goal running out of its own allotted token budget, not a per-invocation spend ceiling a caller
sets up front. There is also a `sessionBudgetExceeded` value inside the enumerated
`codexErrorInfo` kinds (account/org-level cap being hit), which acy could detect and react to,
but not one it can configure.

**Bottom line for acy's purpose:** the cache-read number acy cares most about
(`cachedInputTokens`) is present, per-turn and cumulative, in both the live event stream and
the on-disk transcript — better exposed than claude's, in fact. What's missing is any
dollar-denominated cost and any client-settable spend ceiling; a codex-backed acy would have to
approximate cost itself from token counts and a model price table, and would have no
`--max-budget-usd` to hand to the child process at all — enforcement would have to be acy's own
(watch `tokenUsage`/`token_count` and interrupt/deny once a threshold is crossed).

---

## 10. Structured output

`VERIFIED FROM --help/docs` for `exec`; `VERIFIED FROM --help/docs` (schema field, not live-
exercised) for `app-server`.

**Yes, both surfaces have a direct equivalent of claude's `--json-schema`.**
- `codex exec --output-schema <FILE>`: "Path to a JSON Schema file describing the model's
  final response shape." Same shape of feature as claude's `--json-schema`, but via a file path
  rather than an inline string, and specific to `exec`'s one-shot final answer.
- `app-server`'s `TurnStartParams.outputSchema`: "Optional JSON Schema used to constrain the
  final assistant message for this turn" — a plain JSON value (schema type `true`, i.e.
  accepts any well-formed JSON Schema), sent inline in the `turn/start` call, per-turn rather
  than per-process. Not exercised live in this recon (would require constructing a schema and
  a turn that returns structured data — reasonable follow-up, not done here to conserve
  budget), but it is a first-class, documented param, not an inferred behavior.

Neither was proven live to actually populate a corresponding `structured_output`-shaped field
on completion the way claude's `result` event does — the schema names the *input* mechanism
clearly; this recon did not confirm the exact shape of the *output* field that carries the
validated result. Worth a live check before an implementation depends on the output shape
specifically (as opposed to the fact that the input mechanism exists, which is solid).

---

## 11. Model selection and reasoning effort

`VERIFIED LIVE`.

- **Model:** `-m/--model <MODEL>` on `codex`, `exec`, `resume`, `fork`. On `app-server`:
  `ThreadStartParams.model` (thread-level) and `TurnStartParams.model` ("Override the model for
  this turn and subsequent turns" — so it can change mid-thread without a new thread).
- **Reasoning effort:** no CLI flag by that name. It's a config key,
  `-c model_reasoning_effort=<value>` (seen live in the untouched `~/.codex/config.toml`:
  `model_reasoning_effort = "medium"`), and on `app-server`, `TurnStartParams.effort`
  ("Override the reasoning effort for this turn and subsequent turns" — again live-adjustable
  per turn, unlike claude's `--effort` which is a process-launch flag).
- **Effort values are per-model, not a fixed global enum** — confirmed via
  `codex debug models --bundled` (local catalog dump, no model call): each of the 8 bundled
  models advertises its own `default_reasoning_level` and `supported_reasoning_levels[]`. For
  `gpt-5.6-sol`: `low`, `medium`, `high`, `xhigh`, `max`, `ultra` (`ultra` described as "Maximum
  reasoning with automatic task delegation" — i.e. it can itself fan out to sub-agents, a
  different axis than claude's `--effort`). The schema's own `ReasoningEffort` type is just
  `{"type":"string","minLength":1}` — deliberately unconstrained, because the valid set is
  whatever the currently-selected model publishes, not a fixed list codex hardcodes.

---

## Fit against acy's seams

- **`internal/driver`** — needs a second implementation entirely, not a flag change: codex's
  live surface is JSON-RPC-over-NDJSON with a request/response `id` plus async
  notifications, not claude's flat `-p --output-format stream-json` NDJSON-of-events; a
  codex driver has to track pending request ids (including server-initiated ones like
  `item/commandExecution/requestApproval`) in both directions, not just decode a one-way event
  stream and write bare user-turn lines.
- **`internal/gate` + `internal/cli/hook.go`** — the whole "hook subprocess connects to a unix
  socket per tool call" design goes away: the approval request already arrives in-band on the
  same `app-server` stdio/socket connection the driver owns, so gating becomes "the driver
  itself holds a pending approval and answers it," with no second process and no PreToolUse
  hook plumbing at all — a structurally simpler shape, but not a drop-in port.
- **`internal/config`** — `WriteHookSettings` has nothing to generate (no hook to register);
  `WriteMCPConfig` could still generate a codex-side config, but the natural per-invocation
  path is either `-c mcp_servers.<name>...` args or (for `app-server`) an inline `config` object
  on `thread/start` — no file necessarily required.
- **`internal/mcp`** — acy's own server can likely still be registered as an MCP server for
  codex the same way it is for claude, but the more native fit revealed here is `app-server`'s
  `dynamicTools`/`item/tool/call`, which doesn't need a separate MCP server process at all;
  which path to take is a design question outside this recon's scope, but both are viable and
  should be weighed.
- **`internal/session`** — `Replay`/`ProjectDir`'s claude-specific slug rules and record shape
  (`{"type":"user"|"assistant","message":{...}}`) don't apply; a codex replay reads
  `~/.codex/sessions/<Y>/<M>/<D>/rollout-*-<thread-id>.jsonl` and a different wrapper shape
  (`{"timestamp","type","payload"}` with `event_msg`/`response_item`/`turn_context` kinds) —
  same *idea*, different parser, and (bonus) it can recover cost/usage history claude's
  transcript never had.
- **`internal/orchestrator`** — `--json-schema`/`structured_output` has a real analog
  (`--output-schema`/`TurnStartParams.outputSchema`), so the disposable-child-returns-a-
  validated-report pattern itself survives; the budget-ceiling half of a child's contract
  (`--max-budget-usd`) does not — there is no such flag, so acy would have to police a child's
  spend itself by watching `tokenUsage`/`token_count` rather than handing codex a ceiling.
- **`internal/supervisor`** — the launcher/spawn closures would fork into a codex-specific pair
  around a persistent `app-server` process (or one per role, mirroring today's parent/child
  split) rather than one-`claude`-process-per-phase; arming becomes a `turn/start` (or
  `turn/steer`) on the same thread rather than a relaunch with `--resume`, which is a closer
  match to acy's "flip a phase in place" intent than claude's own resume-based arming is.

## Capability gaps

Features acy currently advertises that this recon found **no working codex equivalent** for,
or found only a structurally different, weaker one:

- **The per-tool countdown gate with veto, as acy implements it today (a blocking PreToolUse
  hook subprocess), has no codex analog** — but the *underlying capability* (a blocking,
  in-band approval request the client must answer before the tool runs) does exist on
  `app-server`, live-verified in Q3. The gap is architectural (no separate hook process,
  request arrives on the same channel as everything else), not a missing capability.
- **`codex exec` cannot host any approval gate at all** — live-verified in Q3: a write blocked
  by sandbox/approval policy just fails closed in the same turn, with no request event to
  answer. Any codex backend that wants acy's gate has to use `app-server`, never `exec`.
  Corroborated by `-a/--ask-for-approval` being entirely absent from `exec --help`.
- **Acy's structural "the tool does not exist" guarantee has no codex equivalent at all** —
  the single biggest gap, detailed in Q4. Codex only ever offers sandboxing and approval
  *around* an ever-present shell tool.
- **Arming a run in place** (flipping a phase on a *live* session with no relaunch) has a
  plausible-but-not-yet-proven better fit on codex: `turn/start`/`turn/steer` on an existing
  `threadId`, live-verified for `turn/start` in Q2 (two turns injected into one process with no
  relaunch). Whether `turn/steer`'s mid-turn redirect specifically maps onto acy's "arm" moment
  (as opposed to just "send the next message") is a design question, not something this recon
  resolved.
- **`Esc` interject mid-turn**: claude's `control_request`/`interrupt` maps to codex's
  `turn/interrupt` in principle (schema-verified, Q8) — but codex additionally offers
  `turn/steer`, which redirects a live turn *without* ending it, a capability claude/acy's
  current design has no use for today because it doesn't exist on claude's side. Not a gap so
  much as an extra capability whose fit isn't decided.
- **The message queue flushing as one turn once the session goes idle**: no codex equivalent
  was found or looked for beyond what's implied by `turn/steer` existing — this recon did not
  determine whether `app-server` has any concept of turn-batching/coalescing multiple pending
  client messages into one model turn the way acy's own queue does. Likely still an acy-side
  responsibility either way. `UNKNOWN`.
- **`/resume` restoring a transcript**: the underlying resume capability is real and arguably
  richer on codex (resume by id, by history, or by path; `thread/resume`'s own doc comment
  says a *running* thread is rejoined rather than relaunched) — Q7. The gap is only that acy's
  existing replay parser is claude-shaped and cannot read codex's transcript format unmodified.
- **The `/tokens` cache-read ledger**: fully supported, and arguably better —
  `cachedInputTokens`/`cacheWriteInputTokens` are both present per-turn and cumulative, live and
  on disk (Q9). Not a gap.
- **A dispatched child's schema-validated report**: the mechanism exists
  (`--output-schema`/`outputSchema`, Q10) but this recon did not live-verify the shape of the
  field carrying the validated result on completion — flagged as a follow-up check, not a
  confirmed gap.
- **A per-child dollar spend ceiling** (`--max-budget-usd`): no equivalent flag exists on any
  codex surface (Q9). This is a real, confirmed gap — acy would have to enforce any spend
  ceiling itself by watching token counts, since codex won't self-terminate on a dollar figure
  it was never given.
- **MCP tool calls passing through the same approval flow as shell/patch calls**: `UNKNOWN`
  (Q5) — this recon found no dedicated `ServerRequest` variant for it and could not test with a
  real MCP server. Anyone building on "MCP tool calls are gated the same way Bash is" should
  verify that assumption first; it may not hold.
