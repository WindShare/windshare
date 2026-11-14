# Desktop live sharing

## Goal and rationale

Connect the everyday flow: select files or folders → obtain a usable link → let a browser receive them → stop sharing or quit the app.

The default desktop reuses the original UI and animation with native selections and real engine tasks; sample flows require `--preview`. The [engine](../../engine/share.go) already owns real sharing: `StartShare` returns a task; `Ready` waits for the first relay registration and local session admission, without waiting for P2P, a receiver or descendant discovery. The missing boundary is desktop integration, not another transfer implementation.

This improves the primary user journey. A thin C ABI and local WebSocket follow the stated product direction and keep transfer behavior shared across clients. They add native build and debugging cost, so scope the work to desktop sharing rather than a general RPC framework or future mobile infrastructure.

## Ownership and semantics

- **Engine:** owns tasks, connectivity, shared capacity and resource shutdown. Keep filesystem assembly outside `engine/**`.
- **Host:** owns one engine for the application lifetime. C ABI `Start/Stop` starts and closes that host; `Start` returns a loopback port and per-start random bearer token after listening begins. Local WebSocket carries commands, results and observations; file content stays on existing transfer paths.
- **Platform adapter:** resolves native selections into `FileSourceFactory`. Implement the desktop filesystem source now; keep Windows path and handle semantics out of shared task contracts.
- **Desktop:** presents engine results through an application-level task model; pages only select what to show. Navigation and control-connection loss do not stop shares. Explicit cancellation, stopping a share and quitting the application have distinct meanings.

Host readiness only means the local listener is available. Share `Ready` is a retained startup milestone, not a live availability guarantee. Keep task lifecycle and current connectivity separate; relay unavailability does not imply existing P2P transfers have stopped. `StopShare` ends a share; do not present it as reversible pause. Automatic recovery retains the link; sharing again after a terminal task creates a new task and link from the retained selection. Report shutdown completion after engine work has joined.

Observation queues are bounded, lossy and not broadcast. Add engine queries for current share lifecycle, readiness and relay availability at their existing owners. The host alone drains each task stream and distributes bounded notifications. Reconcile snapshots and retained results on connect/reconnect, with bounded periodic refresh for silent drops; prevent stale updates from overwriting newer state. Keep lifecycle decisions in the engine.

## Execution

### 1. Connect the host and a minimal desktop caller

Implemented and verified on Windows (2026-11-11), initially through
`windshare --live-share <path...>`. The relocated desktop package
downloaded a 1 MiB file through the saved relay/frontend configuration in Edge;
source and download SHA-256 matched. Explicit stop and quitting with an active
share both reached joined engine shutdown. See [desktop build and run](../../desktop/README.md).

- Extract `shareFileSource` from [CLI assembly](../../cmd/wind/internal/cli/share.go) into `adapters/filesystem`; use it from both CLI and host. Reuse `engine/relayconfig`, `core/link` and [observation encoding](../../engine/observation/wire/contract.go) without importing CLI presentation code.
- Add the host outside the engine subtree, with narrow commands to create, list and query shares, observe updates, cancel preparation and stop sharing. Return task identity before waiting for readiness.
- Bind tasks to the application, independently of request waits and subscriptions. Retain task handles/results and map creation operations to tasks within the application session, so lost replies can be recovered and command replays cannot create duplicate shares. Cancellation targets that operation and stops its share even if readiness races with the request. Delegate shutdown to `Engine.Close`.
- Integrate the Go library and C ABI into desktop build, packaging and CI, with explicit ownership of returned ABI values. Connect a minimal GPUI caller to real creation, result retrieval and joined shutdown before expanding the interface. Keep blocking startup/shutdown work off the GPUI thread.
- Enable asynchronous file logs with bounded retention by default; correlate desktop operations, host requests and engine tasks. Keep detailed trace opt-in and observation delivery bounded so a slow UI cannot stall transfers.

Use an already configured relay/frontend for this first increment. Missing configuration must produce an actionable state rather than endless preparation or a sample link.

### 2. Complete the user-facing sharing flow

The configured-installation flow is implemented: `make desktop` opens native file
and folder selection, accepts mixed drops, exposes real links immediately, and
retains tasks in My shares. Windows verification covered picker cancellation,
Chinese paths with spaces, repeated shares across navigation, browser download
with matching SHA-256, explicit stop, and joined quit with another active share.
In-app relay setup, browser device approval and continuation of retained selections
are implemented. Recovery now retains configuration commits, cancels unfinished
approval with retryable cleanup, distinguishes relay recovery from terminal admission,
and creates new tasks from retained selections after authorization failures. Retries
reload saved settings before the engine applies its network refresh/backoff policy.
Quitting prompts for every unfinished selection or setup, with dismissal keeping work alive.

Windows release-package verification (2026-11-14) covered missing-file initialization,
partial setup failure, recovery of the same selection using saved settings, two browser
downloads with matching SHA-256, stop/repeat with different links, navigation, and joined
shutdown. Both Esc and closing the quit prompt preserved preparing or active shares.
`desktop-check`, `desktop-package`, `hygiene`, `lint`, `vet`, `sloc` and targeted Go race tests passed.

The [development network](../../deploy/network/development/README.md) contains the
existing two trust roots and verified directory (generation 1 / serial 2).
`make development-build` builds all three clients from this checkout with that
identity. Recorded desktop-to-browser evidence used `?n=us1` without explicit
relays and verified a 1 MiB download. The production `deploy/network/release.json`
remains unprovisioned; production publication and the separate P2 account workflow
remain external dependencies.

- Preserve the original shell, animated selection/flight/landing, result card, and share grid/list. Use one presentation for native and preview sessions; replace `SampleBundle`, generated preview links and completion timers with native selection and real tasks. Accept mixed file/folder drops as one share alongside native pickers, without descendant counting or staging copies.
- Present preparation, sharing, stopping and terminal outcomes from real state. Use owner-provided typed reasons to offer setup for missing configuration, retry for temporary network unavailability, or required authorization only when blocked. The host retains the creation operation and selection. Distinguish active relay recovery from terminal admission failure while preserving existing P2P transfers.
- Complete relay/frontend setup through the existing configuration and setup owners, including missing-file initialization and explicit settings commit points. Report partial failures and preserve committed settings. Cancelling a share stops further admission and cancels its unfinished device approval through `engine/relaysetup`; expose cancellation failures for retry.
- Make the returned link immediately available for copying; decorative animation must not delay interaction. Show selected-root information already available from the engine, leaving unknown directory totals absent.
- Register tasks in the desktop model before readiness; preserve preparing and active shares across navigation, new-share actions and control reconnection. Keep all sample data, receive/transfer simulations and unsupported controls in development preview only. Complete desktop-to-browser downloads through existing receiver and transfer capabilities.
- Before a user-requested window close or quit, offer “Keep running” and “Quit and stop all shares” if any share is unfinished, including preparation or setup. Dismissing keeps tasks running; confirming starts shutdown. With no unfinished shares, quit directly.
- Reuse saved CLI/GUI configuration, `engine/relaysetup` and the injected built-in network, preserving explicit choices, including configured-only mode and disabled defaults. First-run official sharing depends on [Global Network P1](global-service/global-network.md): provisioned release trust roots, a signed directory, usable nodes and the frontend. Use valid snapshots and configured relays immediately. Default network selection gets one bounded refresh attempt when neither is available, then offers retry on failure. User retries reuse the same refresher, coalescing and rate limits; expose any cooldown instead of silently returning a cached failure.
- Official account provisioning belongs to Global Network P2. Use valid credentials when available; permitted anonymous admission can proceed without waiting for account setup, subject to the node's lower-priority service policy. Prompt for authorization only when credentials are needed to obtain a usable path. Restricted-relay device approval stays a separate self-hosted setup flow.

Prioritize zero-configuration first-run sharing and blocking recovery fixes. Verify the packaged desktop-to-browser flow and recovery with retained selections before treating desktop sharing as delivered. Keep the normal path at selection → first admission → immediately usable link; recovery handling adds no choices, confirmations or synchronous checks to that path.

## Scope

Keep desktop receive, offline hosting, persistent share history and mobile source providers outside this change. Explorer integration remains a separate follow-up for the everyday Windows flow. Keep the current transfer protocols and engine ownership model; introduce only the host contracts needed by live sharing. Keep recovery and configuration commits within existing owners, without a general recovery framework or cross-system rollback. An in-app trace switch can follow in diagnostics settings.
