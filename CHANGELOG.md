# Changelog

## [1.4.0](https://github.com/hweeks/always-click-yes/compare/v1.3.0...v1.4.0) (2026-07-31)


### Features

* bound delegated task spending ([da8dc22](https://github.com/hweeks/always-click-yes/commit/da8dc227fcdc4093cb2f08110b14f3a3fa1033e1))
* default delegated children to sonnet ([1e39397](https://github.com/hweeks/always-click-yes/commit/1e393972f11c97cea85c9cacfc7a08fc1510c566))
* expand child reports and webview controls ([92a60dd](https://github.com/hweeks/always-click-yes/commit/92a60dd15dddb2c0818423a974dcd486cf1e1784))
* make delegated work resilient and token-thrifty ([ebd8238](https://github.com/hweeks/always-click-yes/commit/ebd8238cc3c5e8c2c2ac0971357d593f3874af10))
* retry work after Claude cooldown ([9fc29bf](https://github.com/hweeks/always-click-yes/commit/9fc29bfe03b93259ade93b2e0b44bdf9213ad485))


### Bug Fixes

* raise default delegated budgets ([b95afe4](https://github.com/hweeks/always-click-yes/commit/b95afe4e7545d4ba575f93550ccc3c95089e803f))

## [1.3.0](https://github.com/hweeks/always-click-yes/compare/v1.2.0...v1.3.0) (2026-07-29)


### Features

* serve the supervisor over HTTP and render it in a VS Code webview ([706be04](https://github.com/hweeks/always-click-yes/commit/706be041980f9a4b1ef0054d6ab7486591ba00f2))

## [1.2.0](https://github.com/hweeks/always-click-yes/compare/v1.1.0...v1.2.0) (2026-07-28)


### Features

* queue messages, multi-line input and file paste in the composer ([1de92cf](https://github.com/hweeks/always-click-yes/commit/1de92cffcb595c6a9f390043e591efb2267d1b7e))
* queue messages, multi-line input and file paste in the composer ([23c074c](https://github.com/hweeks/always-click-yes/commit/23c074cb15b8b7d614a3297ae84172d0d3224977))
* small updoot ([6e9a40f](https://github.com/hweeks/always-click-yes/commit/6e9a40fc6f0e8c6302960754076e38b37821dde6))
* small updoot ([f5244bf](https://github.com/hweeks/always-click-yes/commit/f5244bfb2198e27e9afe5f9afcefc04a8d0a3d79))

## [1.1.0](https://github.com/hweeks/always-click-yes/compare/v1.0.1...v1.1.0) (2026-07-27)


### Features

* bundle the acy binary in the .vsix, detect claude, and publish to the Marketplace ([f71f106](https://github.com/hweeks/always-click-yes/commit/f71f106652a5c31f18b5878694e215e7470e6160))
* detect a missing claude CLI before a run dies on it ([fc3be21](https://github.com/hweeks/always-click-yes/commit/fc3be219919c0e4c5ebdf1449381e2eae8ba8375))
* publish the release .vsix packages to the VS Code Marketplace ([e5bc369](https://github.com/hweeks/always-click-yes/commit/e5bc369d503112edf5f236575b6f72d7ff0e9b76))


### Bug Fixes

* chmod the bundled acy binary before launching it ([c0bdaad](https://github.com/hweeks/always-click-yes/commit/c0bdaad79b94e9b821376504792375070f9107e9))

## [1.0.1](https://github.com/hweeks/always-click-yes/compare/v1.0.0...v1.0.1) (2026-07-26)


### Bug Fixes

* document /tasks, which shipped without reaching the README ([d47e6a9](https://github.com/hweeks/always-click-yes/commit/d47e6a988f54243749cecb94aaa95b381405756d))
* document /tasks, which shipped without reaching the README ([6312afb](https://github.com/hweeks/always-click-yes/commit/6312afb49d59e4c5e06e8cd8d8df513d676a73a6))

## 1.0.0 (2026-07-26)


### ⚠ BREAKING CHANGES

* the supervising session can no longer write, run commands or search the web; --plan-tools now defaults to Read,Grep,Glob and governs both phases. Runs no longer end via a STATUS: DONE line, and no longer auto-nudge.

### Features

* add a VS Code extension that launches acy in a terminal ([22b4c79](https://github.com/hweeks/always-click-yes/commit/22b4c7918df5501f8dc8ac7df679e25baefcbb68))
* automate releases and report a version ([beb67eb](https://github.com/hweeks/always-click-yes/commit/beb67eb1f166124e75114b1e44884a81febb395b))
* automate releases and report a version ([86e9fdd](https://github.com/hweeks/always-click-yes/commit/86e9fdd402d5ec6e2d2e5ed7072a43a7fb47eff2))
* delegate work to disposable child sessions ([d6a2757](https://github.com/hweeks/always-click-yes/commit/d6a275786ed10ea744ddfbefc9cfa170666c890e))
* judge completion in-session, and ask questions over an acy MCP bridge ([2e7be3c](https://github.com/hweeks/always-click-yes/commit/2e7be3c69677af0396b89b2138b080fba99954d6))
* phase-colored chrome, boxed entries, and syntax-highlighted tool code ([4dfea10](https://github.com/hweeks/always-click-yes/commit/4dfea100ecdb35b981a3d69f91169243bb4d6fc6))
* phase-colored chrome, boxed entries, and syntax-highlighted tool code ([8ee41d3](https://github.com/hweeks/always-click-yes/commit/8ee41d3768d2023d180b5516e84d0fa0604ab08b))
* publish windows binaries and per-platform vsix packages on release ([5d852e7](https://github.com/hweeks/always-click-yes/commit/5d852e7489dba92486c6e25b501a93a59b716316))
* read run settings from a project .acy.json ([314b03f](https://github.com/hweeks/always-click-yes/commit/314b03f6c88c86006346e8ca3480d7dd354ac3f5))
* resume an acy session at its current place ([caf9e82](https://github.com/hweeks/always-click-yes/commit/caf9e82a34ba7231dabe46fcee25310810dc32c5))
* resume mid-run, an MCP ask bridge, and completion judged in-session ([75e5b80](https://github.com/hweeks/always-click-yes/commit/75e5b8004a89919d36e694dc68100c33f6df67c3))
* seed .acy.json from settings, validate it in-editor, and add a status-bar launcher ([34f11b9](https://github.com/hweeks/always-click-yes/commit/34f11b91bae332974eeef5cda0eeddab9e03e9f9))


### Bug Fixes

* match claude's project-dir slug, and never pass --session-id with --resume ([fa17703](https://github.com/hweeks/always-click-yes/commit/fa17703e25e96731681bbdbc223b2a1afc07ac5a))
