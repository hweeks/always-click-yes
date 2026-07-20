# Always Click Yes (acy) — VS Code extension

Supervise a long-running [Claude Code](https://claude.com/claude-code) task from
VS Code: plan it, arm it, and let [acy](https://github.com/hweeks/always-click-yes)
approve each permission prompt after a short, interruptible countdown. The
extension launches acy's TUI in an integrated terminal — same UI, same keys,
one supervisor per window.

## Commands

| Command | What it does |
|---------|--------------|
| **ACY: Plan & Run** | start a supervisor in the workspace folder (`acy run`) |
| **ACY: Continue Last Run** | resume the most recent run in this folder (`acy run --continue`) |
| **ACY: Create .acy.json** | seed a project config from the `acy.defaults.*` settings and open it |

The `▶ acy` status-bar item is **Plan & Run**. If a supervisor terminal is
already alive, either command reveals it instead of launching a second one.

## Configuration

Run settings live in the project's **`.acy.json`**, not in flags and not in
VS Code settings — the same file a terminal `acy run` reads, so the CLI and the
extension can never disagree. Editing it gets IntelliSense from the bundled
schema. Example:

```json
{
  "model": "opus",
  "countdown": "20s",
  "maxLines": 15
}
```

Extension settings cover only what can't live in the project file:

- `acy.binaryPath` — where the acy binary is. Leave empty to use the binary
  bundled with a platform-specific build of this extension, or `acy` from
  your `PATH`.
- `acy.defaults` — seed values for **ACY: Create .acy.json**. Once the file
  exists, the file is the source of truth; these are never consulted at launch.

## Install

Grab a `.vsix` from the
[latest release](https://github.com/hweeks/always-click-yes/releases/latest)
and install it via **Extensions: Install from VSIX…**. The platform-specific
packages (`darwin-arm64`, `darwin-x64`, `linux-x64`, `linux-arm64`,
`win32-x64` — the last experimental) bundle the acy binary; the `universal`
package expects `acy` on your `PATH` or `acy.binaryPath` set.

## What you're opting into

acy is an experiment in total trust: you make contact at the plan and at the
end, and in between the machine runs unattended, approving its own way to the
finish. Read the [project README](https://github.com/hweeks/always-click-yes)
before leaving it alone with anything you love. The veto key is `s`.
