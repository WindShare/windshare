# Desktop live sharing: refine t8 and merge into main

Optimize `t8n3q6w9z2` (`d9353799`) and merge it into `main`. The comparison used `main` at `99e0f146` and `k9v4m7x2q8` at `8ac31a71`.

## Rationale

- **Use t8 as the foundation.** It completes more of the everyday flow: mixed drops, multiple folder selection, task registration before readiness, explicit stop, and separation of real and preview behavior. Its selection/session/projection boundaries are easier to maintain.
- **Borrow mechanisms selectively from k9.** Immediate state notifications, selected-root summaries and bounded log segments are useful. Its duplicated registries can suppress reconnect updates; retry can return an already-failed task; production navigation still exposes simulated receiving. Importing the whole branch would carry those problems forward.
- **Remove unnecessary costs from the common path.** In t8, readiness reached after a command reply waits for a periodic snapshot and GUI refresh; unchanged state also causes background wakeups. This establishes an avoidable delay, not a measured average or strict upper bound. k9 reads filesystem metadata on the GUI thread. Reliable sharing requires neither behavior.
- **Keep the architectural investment focused.** Application-owned tasks, idempotent commands and snapshot recovery protect ordinary sharing without extra user choices. The C ABI and control socket add build/debugging cost, but file content remains on existing transfer paths. First-use setup affects every new user and belongs in the product flow.

## Execution

Work in an isolated checkout of t8, preserving unrelated changes in the current main workspace.

1. **Make task state authoritative and explicit.** Engine owns lifecycle, retained startup readiness and current relay availability as separate facts; Rust keeps one derived model for UI projections. Define host-owned creation states, including waiting for configuration, admission and terminal outcomes, before building setup UI. Use typed outcomes. Replaying a request keeps its operation identity; retrying a terminal failure creates a new operation. Preserve cancellation across configuration/admission/readiness races, and keep navigation independent of task ownership.

2. **Replace polling as the primary update mechanism.** State owners notify after committing queryable state, including readiness and joined terminal results. Send complete share-list snapshots with one session-scoped revision, advancing only on state changes; coalesce pending updates to the latest full snapshot. Wake GPUI on model changes; keep detailed observations separate and lossy. Reconcile snapshots on connection/reconnection, with a bounded periodic fallback while work is active. Notification delivery must not block transfers, and missing notifications must not permanently stale the UI.

3. **Complete the existing sharing presentation.** Preserve native selection, mixed drops, grid/list, link controls and animations. Make copying/opening available as soon as a usable link arrives. Carry the engine's selected-root name, type and known file size through the host; leave unknown directory totals absent. Perform filesystem work off the UI thread, without descendant scans or staging copies. Stopping/finished tasks must not present historical links as usable. Preserve the Finished filter and creation-based share age; keep preview-only receive/payment flows inaccessible in production.

4. **Add a concise first-use setup flow.** Reuse saved CLI settings and `engine/relaysetup`; configured installations proceed directly to sharing. The host owns asynchronous relay/frontend setup and pending admission without holding the command lock. On shutdown, cancel and join both off the UI thread; late completions must not start shares. Retain the selection; resume the same operation only while awaiting configuration and not cancelled. Cancelling a share preserves saved configuration. Preserve explicit relay choices and disabled defaults. Official account provisioning remains a separate dependency; manual configuration supports development and self-hosting while default onboarding remains unfinished. Keep the account backend outside this change.

5. **Bound diagnostics without making sharing depend on them.** Keep asynchronous, bounded logging and correlate session, operation and task identities. Daily rotation alone does not bound disk usage: add size-based retention, using existing logging/time primitives rather than custom calendar arithmetic. Keep detailed trace opt-in and allow sharing to continue when log files cannot be opened. Record meaningful transitions and dropped evidence without logging unchanged snapshots repeatedly.

6. **Restore production host import boundaries.** `scripts/ci/_engineboundary/main.go` currently scans only `cmd` and `engine`. Include `host/**` and extend the production-host classification to cover both `host/**` and `cmd/windshare-host/**`; adding a scan root alone leaves the test-support predicate incomplete. Reject direct connectivity/transport/relay implementation imports and `internal/test*` dependencies, while allowing host-side filesystem adapter assembly.

7. **Keep packaging explicit and runnable.** Define the runtime package contents as the executable, required native libraries, license and concise instructions; keep the generated C header as a build artifact. Preserve t8's relocated `--host-check` startup/control/shutdown check and explicit Windows C compiler setup. Avoid k9's exact-two-file constraint, which rejects legitimate distribution files without establishing that the package can run.

8. **Integrate into main.** Commit focused changes on t8 and incorporate newer main changes while preserving ownership boundaries. Before merging into main, reuse existing fixtures to verify real desktop-to-browser download integrity, control reconnection and stopping shares. Align the desktop run/build documentation with the resulting behavior and remove superseded integration notes. Keep unrelated workspace edits outside the merge.

## Scope

Desktop sending through the existing engine only. Desktop receiving, Explorer integration, offline hosting, persistent share history, mobile providers and a general RPC framework remain separate work.
