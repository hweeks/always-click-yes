# Changelog

## [1.6.0](https://github.com/hweeks/always-click-yes/compare/v1.5.0...v1.6.0) (2026-08-06)


### Features

* add arch mode and the acy arch command ([9119ef1](https://github.com/hweeks/always-click-yes/commit/9119ef187cc6997bffa57f9da148a4bd9b131c03))
* add ask interception and finish outcome seams ([fcd480c](https://github.com/hweeks/always-click-yes/commit/fcd480c13a5ba1d116122cad9b739af82e21f77a))
* add engineer wire protocol and journal ([05e1a3c](https://github.com/hweeks/always-click-yes/commit/05e1a3c56cc47b82381ef4aa227410ed7d9ba806))
* add fleet doctor and the per-host transport factory ([62527c8](https://github.com/hweeks/always-click-yes/commit/62527c87fa3c35b15e3d7fd85d1921241a241320))
* add gitops worktree and PR harness ([b842ff4](https://github.com/hweeks/always-click-yes/commit/b842ff428444b317d2362b7d8e7a2c6ec8c8e09e))
* add make arch for the arch-mode dogfood loop ([06b330a](https://github.com/hweeks/always-click-yes/commit/06b330acb6e3e973e2427ee6913f3b27a750367d))
* add the architect MCP role and fleet tool contracts ([382904c](https://github.com/hweeks/always-click-yes/commit/382904c8e2287c9282eabfb89a44b190ebd2106d))
* add the engineer core runtime ([61b0201](https://github.com/hweeks/always-click-yes/commit/61b02019f7cc724351ed67705b9de6ea38f52b14))
* add the engineer daemon and attach protocol ([8e26ec9](https://github.com/hweeks/always-click-yes/commit/8e26ec915f31a675ff9d7a3e66687b49363e6ca4))
* add the fleet config and engineer transports ([687e694](https://github.com/hweeks/always-click-yes/commit/687e69442af0019a9cc1288aada1e2e9838c45c8))
* add the fleet manager ([202c37c](https://github.com/hweeks/always-click-yes/commit/202c37cd722a94222e2c5d9bbc120e6422b84b10))
* add the markdown ticket store ([76c254f](https://github.com/hweeks/always-click-yes/commit/76c254fcf1866557e2d14ecbabff38088da8e56b))
* arch mode — an architect supervising a fleet of engineer instances ([2e06c26](https://github.com/hweeks/always-click-yes/commit/2e06c26b54c6a7ea841a185064c749fb4ba19fd1))
* bound fleet spend and enforce the wire handshake ([75632fe](https://github.com/hweeks/always-click-yes/commit/75632fe0a77a357c927a7a3440404324dafc03dd))
* extend the remote PATH per fleet host ([96fee6b](https://github.com/hweeks/always-click-yes/commit/96fee6b5750834866c5f9cb9ca930be245ee26f3))
* let the architect create tickets ([2b56890](https://github.com/hweeks/always-click-yes/commit/2b56890b90bd4ab8fad55c7e6721d08d36d31afc))
* put the ticket board in the architect's hands ([b39297d](https://github.com/hweeks/always-click-yes/commit/b39297db4b64fcdcc12f0f65e7e5373be12d7f23))
* record branch and PR on tickets ([132d299](https://github.com/hweeks/always-click-yes/commit/132d2990dde785874bea3fdb30ba3ac944772f95))
* resume an arch run and re-attach its engineers ([86fdcce](https://github.com/hweeks/always-click-yes/commit/86fdcce57f30baeca13fe9e7a8b96f9c446fc47d))
* route the fleet tools through the supervisor UI ([4e98bc5](https://github.com/hweeks/always-click-yes/commit/4e98bc58c6d4d169079678aedfd935f720a20dfc))
* source a per-host rc file for fleet commands ([7363832](https://github.com/hweeks/always-click-yes/commit/7363832bffaa9a1931ce4e2740381370a91454e8))
* watch acy PRs and hold launches at the cap ([ea53519](https://github.com/hweeks/always-click-yes/commit/ea535190e5dcdaaca243bcab2a28593fa2a46ad1))


### Bug Fixes

* carry token totals through the engineer result ([5a5feba](https://github.com/hweeks/always-click-yes/commit/5a5febad7e67d31088fa35a590dedeae76f59b61))
* deflake the engineerd suite under parallel load ([2b53279](https://github.com/hweeks/always-click-yes/commit/2b53279d5da3f286cc3a163f9928e5cb4efce4a9))
* forward the fleet tools through the mcp stdio server ([3fa376a](https://github.com/hweeks/always-click-yes/commit/3fa376aa977d615483f3e452e16b442c04976284))
* give the fleet doctor test its own git identity ([ef6305c](https://github.com/hweeks/always-click-yes/commit/ef6305ce54b8cac8035f15a368a37bb6fa859215))
* honor claude auth status --json even on nonzero exit ([3f02ba6](https://github.com/hweeks/always-click-yes/commit/3f02ba6ae9d27b0b76275cbe0d4413e7bc12dc4e))
* never let attach exit before the journal replay flushes ([ed03386](https://github.com/hweeks/always-click-yes/commit/ed033862e69bf80b9ae9b2c35e689db8c7514d18))
* persist the fleet ledger on launch and on engineer completion ([66a76f3](https://github.com/hweeks/always-click-yes/commit/66a76f309b656ede2cc0ab49c7f90ae867bc6354))
* restrict the engineer session to the read-only registry ([39f05ef](https://github.com/hweeks/always-click-yes/commit/39f05efd7c6faac846aa35255f123c92973fc7db))
* stop calling dispatched children engineers ([1e85e09](https://github.com/hweeks/always-click-yes/commit/1e85e09fcff88e289ddf38126c2b54770e4d8c21))
* stop engineer sessions opening their own PRs ([4c11c51](https://github.com/hweeks/always-click-yes/commit/4c11c5169039cf532a0836e4e940a9224d867bfb))


### Refactors

* extract supervisor wiring from cli into internal/supervisor ([46f9e8f](https://github.com/hweeks/always-click-yes/commit/46f9e8f6111116dd9c4d541c100baf2529b5f60b))

## [1.5.0](https://github.com/hweeks/always-click-yes/compare/v1.4.0...v1.5.0) (2026-07-31)


### Features

* add LiteLLM provider gateway ([e31dad7](https://github.com/hweeks/always-click-yes/commit/e31dad73dadf07911f71e7afbe75a8ad4f534409))
* add LiteLLM provider gateway ([1cb96e2](https://github.com/hweeks/always-click-yes/commit/1cb96e2ff70b130eace4c402ebddf37fea732a70))


### Bug Fixes

* check gateway close errors ([83de947](https://github.com/hweeks/always-click-yes/commit/83de947166c8e08cf37b71c4afb1f37162c16a79))

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
