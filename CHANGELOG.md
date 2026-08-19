# Changelog

## [1.9.0](https://github.com/hweeks/always-click-yes/compare/v1.8.0...v1.9.0) (2026-08-19)


### Features

* add a codex agent backend behind an Agent interface seam ([88c63b3](https://github.com/hweeks/always-click-yes/commit/88c63b332350070504f726b7051651d483124d85))
* integrate Codex backend ([c2923c8](https://github.com/hweeks/always-click-yes/commit/c2923c8844945392e534eb4215f7a323285a571b))

## [1.8.0](https://github.com/hweeks/always-click-yes/compare/v1.7.0...v1.8.0) (2026-08-13)


### Features

* add a jira issue-key field to the ticket board ([64b22de](https://github.com/hweeks/always-click-yes/commit/64b22de5519b073f0a3002c8875df128edaf3104))
* add an optional jira section to .acy.json and merge it into --mcp-config ([765633b](https://github.com/hweeks/always-click-yes/commit/765633bbe782189a2bae29da422f582fa5172349))
* carry the ticket flow diagram in the frame and webview protocol ([b469843](https://github.com/hweeks/always-click-yes/commit/b469843ebcb3935542f0aca01573f41e9ecd5369))
* deny merges and protected-branch pushes at the permission gate ([0a0d3f1](https://github.com/hweeks/always-click-yes/commit/0a0d3f19ef906b03bdddfff03772cb441550008f))
* draw the ticket flow at each milestone and on /flow ([5bb90fe](https://github.com/hweeks/always-click-yes/commit/5bb90fec0ed384960a9dd71d64d3c1736f258f9d))
* make queued messages editable ([c41540e](https://github.com/hweeks/always-click-yes/commit/c41540eb38f477dc9e54031a4382884ab15d38c2))
* render the ticket board as mermaid and ascii ([678cdf5](https://github.com/hweeks/always-click-yes/commit/678cdf53223da967b47d0edee84f21da20955e58))
* **ui:** blur the composer only while it owns the keyboard ([7c69a6b](https://github.com/hweeks/always-click-yes/commit/7c69a6bd538d20e8973ac6f376745bdffa619918))
* **ui:** show the current git branch beside the phase chip ([b6c4b07](https://github.com/hweeks/always-click-yes/commit/b6c4b07f553e3fd33f640cb3b98b005b3f673df0))
* wire jira mcp tooling into the architect's config and prompt ([2f58de1](https://github.com/hweeks/always-click-yes/commit/2f58de162a9568c49d2abb6f7bf337d59dfb6ee4))
* write flow.mmd to the ticket store on every board change ([f54775c](https://github.com/hweeks/always-click-yes/commit/f54775cf7e857acf8357077651c656b46d153233))


### Bug Fixes

* check every git push refspec, not just the first, in merge guard ([b016427](https://github.com/hweeks/always-click-yes/commit/b016427252a56761e84917dd0a4cbac13d7c5d95))
* give queue editor and Ask a single source of truth for the keyboard ([7c771ea](https://github.com/hweeks/always-click-yes/commit/7c771eac4b9c55aca6761b63dcd745c78695b597))
* reject embedded newlines in single-line ticket frontmatter fields ([5a4ea51](https://github.com/hweeks/always-click-yes/commit/5a4ea51592f1582e0a76c541da066f48d4269b9a))
* render the flow entry's ASCII half in its HTML, not just the mermaid ([2c817be](https://github.com/hweeks/always-click-yes/commit/2c817bea94d83fb8ad1f44577b26b4a1e9f07602))
* skip ticket-board pushes to the default branch ([c45ec11](https://github.com/hweeks/always-click-yes/commit/c45ec116e5c232563bf98eb453ccab21206d6994))
* thread the configured Jira project key and site to the architect ([c3cd6a8](https://github.com/hweeks/always-click-yes/commit/c3cd6a835b5bacbc2279c52d8496037e34f91e03))
* **ui:** bound the branch badge to the header width ([c4d2795](https://github.com/hweeks/always-click-yes/commit/c4d2795a928889c48fce52797b6151a40ae71936))
* **ui:** route composerActive's method through the free function ([ef54a1d](https://github.com/hweeks/always-click-yes/commit/ef54a1d77a6de33c44e77b67963e9524ad10d59f))


### Performance

* cache the ticket board projection instead of re-reading it every frame ([94f28c0](https://github.com/hweeks/always-click-yes/commit/94f28c07cd54ff0f9be6337954fd64bf61f26211))

## [1.7.0](https://github.com/hweeks/always-click-yes/compare/v1.6.0...v1.7.0) (2026-08-07)


### Features

* add fleet.verifyCommands and fleet.verifyTimeoutSeconds config ([287d35e](https://github.com/hweeks/always-click-yes/commit/287d35eb77ceb6f9f5c243b3e6ae49c6addba307))
* add internal/verify to run post-session verification commands ([728302a](https://github.com/hweeks/always-click-yes/commit/728302ac3296c26c7a05dbfd5ccd7789e4c09546))
* add VerifyCheck/VerifyStatus to the engineer result wire type ([6b848d2](https://github.com/hweeks/always-click-yes/commit/6b848d2ec9b61d619312dd1975442e6aac3cc22e))
* check for a Go toolchain on fleet hosts in doctor ([bc4d50a](https://github.com/hweeks/always-click-yes/commit/bc4d50a80e8792a55e9169578fd4cf2085585887))
* run verification in finalize and append a summary digest ([4c5e79c](https://github.com/hweeks/always-click-yes/commit/4c5e79cf5d121a18b2fb124ce16fd4e73afcf2c2))
* surface verification digest in fleet transcript and Await text ([a75b03c](https://github.com/hweeks/always-click-yes/commit/a75b03c0f6a451032f7325c04142bf2eb0041445))
* thread verify commands and timeout through the engineer wire ([da21f44](https://github.com/hweeks/always-click-yes/commit/da21f44912d4c08dc1cf9cb9e3da837fb77a7638))


### Bug Fixes

* derive fleet rc shell from its rc file, diagnose a broken wrapper ([c981b31](https://github.com/hweeks/always-click-yes/commit/c981b3129aa1d8a6fe36a1d5e8e50b1e5f98028f))
* derive fleet rc shell from its rc file, diagnose a broken wrapper ([ba1c7dd](https://github.com/hweeks/always-click-yes/commit/ba1c7dd748d118ae9186fef986aa45bd82694895))
* document the ssh arg-joining trap and check for a Go toolchain in fleet doctor ([0adc528](https://github.com/hweeks/always-click-yes/commit/0adc528a1128b46e073d96d9d935cb15a7ca43f2))
* wrap and box every transcript entry ([f79e8c6](https://github.com/hweeks/always-click-yes/commit/f79e8c61799366f9e2fa5af713c9015b27d29079))


### Performance

* memoize rebuild() so an idle tick does zero rendering work ([9dcb0c8](https://github.com/hweeks/always-click-yes/commit/9dcb0c8beaf188ca84acd00914dc9e6ed13e4abb))

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
