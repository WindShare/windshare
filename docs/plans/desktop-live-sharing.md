# Desktop live sharing

## Goal and rationale

Connect the everyday flow: select files or folders → obtain a usable link → let a browser receive them → stop sharing or quit the app.

The desktop currently uses sample selections, links and timed completion. The [engine](../../engine/share.go) already owns real sharing: `StartShare` returns a task; `Ready` waits for the first relay registration and local session admission, without waiting for P2P, a receiver or descendant discovery. The missing boundary is desktop integration, not another transfer implementation.

This improves the primary user journey. A thin C ABI and local WebSocket follow the stated product direction and keep transfer behavior shared across clients. They add native build and debugging cost, so scope the work to desktop sharing rather than a general RPC framework or future mobile infrastructure.

## Ownership and semantics

- **Engine:** owns tasks, connectivity, shared capacity and resource shutdown. Keep filesystem assembly outside `engine/**`.
- **Host:** owns one engine for the application lifetime. C ABI `Start/Stop` starts and closes that host; `Start` returns a loopback port and per-start random bearer token after listening begins. Local WebSocket carries commands, results and observations; file content stays on existing transfer paths.
- **Platform adapter:** resolves native selections into `FileSourceFactory`. Implement the desktop filesystem source now; keep Windows path and handle semantics out of shared task contracts.
- **Desktop:** presents engine results through an application-level task model; pages only select what to show. Navigation and control-connection loss do not stop shares. Explicit cancellation, stopping a share and quitting the application have distinct meanings.

Host readiness only means the local listener is available. Share `Ready` is a retained startup milestone, not a live availability guarantee. Keep task lifecycle and current connectivity separate; relay unavailability does not imply existing P2P transfers have stopped. `StopShare` ends a share; do not present it as reversible pause. Report shutdown completion after engine work has joined.

Observation queues are bounded, lossy and not broadcast. Add engine queries for current share lifecycle, readiness and relay availability at their existing owners. The host alone drains each task stream and distributes bounded notifications. Reconcile snapshots and retained results on connect/reconnect, with bounded periodic refresh for silent drops; prevent stale updates from overwriting newer state. Keep lifecycle decisions in the engine.

## Execution

### 1. Connect the host and a minimal desktop caller

- Extract `shareFileSource` from [CLI assembly](../../cmd/wind/internal/cli/share.go) into `adapters/filesystem`; use it from both CLI and host. Reuse `engine/relayconfig`, `core/link` and [observation encoding](../../engine/observation/wire/contract.go) without importing CLI presentation code.
- Add the host outside the engine subtree, with narrow commands to create, list and query shares, observe updates, cancel preparation and stop sharing. Return task identity before waiting for readiness.
- Bind tasks to the application, independently of request waits and subscriptions. Retain task handles/results and map creation operations to tasks within the application session, so lost replies can be recovered and retries cannot create duplicate shares. Cancellation targets that operation and stops its share even if readiness races with the request. Delegate shutdown to `Engine.Close`.
- Integrate the Go library and C ABI into desktop build, packaging and CI, with explicit ownership of returned ABI values. Connect a minimal GPUI caller to real creation, result retrieval and joined shutdown before expanding the interface. Keep blocking startup/shutdown work off the GPUI thread.
- Enable asynchronous file logs with bounded retention by default; correlate desktop operations, host requests and engine tasks. Keep detailed trace opt-in and observation delivery bounded so a slow UI cannot stall transfers.

Use an already configured relay/frontend for this first increment. Missing configuration must produce an actionable state rather than endless preparation or a sample link.

### 2. Complete the user-facing sharing flow

- Replace `SampleBundle`, generated preview links and completion timers with native selection and real tasks. Accept mixed file/folder drops as one share alongside native pickers, without descendant counting or staging copies.
- Present preparation, active sharing, stopping and terminal outcomes from real state. Represent temporary connectivity recovery separately. Replace the sample pause/resume action with stop sharing.
- Make the returned link immediately available for copying; decorative animation must not delay interaction. Show selected-root information already available from the engine, leaving unknown directory totals absent.
- Register tasks in the desktop model before readiness; preserve preparing and active shares across navigation, new-share actions and control reconnection. Keep all sample data, receive/transfer simulations and unsupported controls in development preview only. Complete desktop-to-browser downloads through existing receiver and transfer capabilities.
- Reuse saved CLI/GUI configuration and `engine/relaysetup`. Bootstrap official defaults only for an uninitialized installation, preserving explicit choices, including disabled defaults. Make official account bootstrap an explicit dependency: existing restricted-relay device approval does not provide automatic account creation. Retain the selection through setup and continue the same creation operation after success; surface required authorization or retry when blocked. Self-hosted installations get a concise setup entry.

The first increment is an implementation step, not the product endpoint. Finish the second increment before treating desktop sharing as delivered.

## Scope

Keep desktop receive, offline hosting, persistent share history and mobile source providers outside this change. Explorer integration remains a separate follow-up for the everyday Windows flow. Keep the current transfer protocols and engine ownership model; introduce only the host contracts needed by live sharing.
