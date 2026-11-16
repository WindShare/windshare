# Desktop Explorer sharing

Make the primary Windows flow: select files and folders in Explorer → choose **Share with WindShare** → open the share view → copy a usable link. This is the next desktop priority because it connects the user's existing selection directly to the working sharing engine.

## Implementation status

Implemented 2026-11-15. [Desktop usage and installation](../../desktop/README.md) describes the supported sources and package channels.

- `make desktop-check`: formatting, strict Clippy, 46 Rust tests, 6 build tests and native build passed. `make sloc` passed with no failures.
- Windows 11 build 26300.9974: direct Explorer mixed selections, successive shares, minimized-window restoration, picker cancellation and quit-prompt invalidation passed. Signed development installation, repeated installation, deferred update, idle update and removal passed.
- Windows Share Target: an actual Explorer file activation reached `ReportCompleted`; Explorer then exited, and an independent CLI receiver downloaded the original content with matching SHA-256. Native tests additionally verify file/folder reopening, temporary-item rejection and path containment. An unsupported location produced save-locally guidance before application handoff.
- On this host, the full Windows share sheet remains unresponsive even with WindShare unregistered. Explorer's **Share with / 共享对象 → WindShare** submenu successfully exercises the Share Target path. System menu availability for folders remains Windows-dependent.

## Ownership

| Location | Responsibility |
| --- | --- |
| [Activation adapter](../../desktop/crates/windshare-activation/src/) | Native paths, instance election, bounded handoff, system-share source lifetime and cancellable native picker |
| [Application admission](../../desktop/crates/windshare-desktop/src/live/application.rs) | Serialized intent ownership, presentation queue and shutdown lifecycle |
| [Controller admission](../../desktop/crates/windshare-host/src/admission.rs) | Stable operation identity, retained selections and explicit submission failures |
| [Desktop packaging](../../desktop/package.mjs) | Matching signed binaries, external-location identity package and current-user installation |

## Product behavior

Offer both **Share with WindShare** directly in Explorer and **Share with / 共享对象 → WindShare** through Windows Share Target registration. The system entry also receives supported files from other applications. Both enter the same sharing experience. Keep the direct command as the primary path for files, folders and mixed selections; system menu availability depends on the Windows version and content supplied by the source. [Microsoft's description of the system menu](https://blogs.windows.com/windows-insider/2025/02/28/announcing-windows-11-insider-preview-build-26120-3360-dev-and-beta-channels/) explains its connection to Windows sharing.

One invocation shares the complete selection as one bundle, including mixed files and folders. A cold launch opens the share view immediately; a running application restores its window and presents the new share while previous shares continue. Choosing the menu action already expresses the user's intent, so creation proceeds without another picker or confirmation.

**User-confirmed animation behavior (2026-11-14):** Preserve or refine the existing waiting animation. Present the link once the engine reaches `ShareReadyReached` and the presentation reaches its result phase; continue waiting feedback if readiness takes longer. Animation controls presentation only, while engine preparation proceeds independently without waiting for descendant traversal or P2P negotiation. Preserve selections through existing network, configuration and authorization recovery, avoiding unnecessary animation restarts. Keep the current relationship between application lifetime and active shares, including the quit prompt.

## Architecture

```text
Explorer command ────┐
Windows Share Target ┴→ Windows activation ─┐
Picker / drop ──────────────────────────────┴→ ShareIntent → Controller → Go host / engine
```

Introduce two focused Rust crates in the desktop workspace:

- `windshare-shell`: the native COM DLL implementing the Explorer command. It captures the selected paths and hands them off; it has no GPUI, Go host or transfer ownership.
- `windshare-activation`: typed activation messages, local transport, Windows instance ownership and a Windows Share Target adapter. The adapter owns system share objects through content acquisition and handoff; the desktop application projects accepted intents into its existing share model.

Keep Windows handles, shell objects and instance routing in these adapters. The existing host remains the owner of engine tasks, configuration and transfer lifetime.

Model activation as distinct `ShowWindow` and `ShareSelection(ShareIntent)` messages. A share intent identifies one deliberate user action and carries its selected roots. Transport retries preserve that identity; a second deliberate click gets a new identity even when the paths match. Carry the intent identity into the existing host operation ID rather than assigning a fresh operation on each delivery.

Distinguish dispatcher receipt, application acceptance and engine readiness. Application acceptance means the existing controller owns the selection; reuse its operation model. Delivery retries are scoped to the running application session, without a persistent queue or cross-crash recovery.

## Implementation sequence

Verify a minimal signed identity package and Explorer registration early, then complete distribution packaging in step 6. Deliver the direct Explorer flow first; Windows Share Target is an independent increment using the same foundation.

### 1. Make share admission independent of the current view

Refactor native selection into application-owned intent submission and view presentation. Route picker results, drops, startup arguments, Explorer commands and acquired Windows Share selections through that submission path. Separate picker availability from application lifetime so an open dialog cannot silently discard an external selection.

Refactor controller creation to consume the stable intent identity. Repeated delivery finds the existing operation and presentation; conflicting content under the same identity returns an explicit error. Retain a visible selection and failure when submission cannot proceed.

Parse startup arguments through `args_os` and typed launch intents. Resolve relative command-line paths against the invoking process's directory before forwarding them. Preserve native paths until the existing host encoding boundary; unsupported paths produce an explicit error without lossy replacement.

### 2. Establish one application owner and reliable handoff

Perform instance election before loading the Go host or constructing GPUI. Use Windows instance ownership and a named pipe restricted to the current user and logon session, with separate identities for installed release and development channels.

Let a short-lived dispatch mode of the desktop executable own bounded launch, connection and retry work. Transfer selections through bounded local IPC instead of concatenating selected paths into a shell command or command line. Explorer returns after the dispatcher receives the selection; the dispatcher remains responsible until application acceptance or a visible failure.

Model the receiver lifecycle as starting, running and closing, serializing application acceptance with committed shutdown. Retain accepted selections in the controller while the window is being constructed, then acknowledge ownership. Preserve intent identities across lost replies and reconnects, with bounded queues and explicit capacity errors. Respect the [host request byte limit](../../desktop/crates/windshare-host/src/request.rs) and existing selected-root count limit; never truncate a selection.

A secondary process forwards its intent without starting another engine, then exits after application acceptance and settlement of any system share operation. During committed shutdown, wait for the old owner within the dispatch deadline before launching a replacement. Report startup or handoff failure visibly and allow the user to retry from Explorer.

### 3. Add the Explorer command

Implement `IExplorerCommand` in `windshare-shell`. Register the same command for arbitrary files and selected directories. Use the full `IShellItemArray` supplied to `Invoke` so mixed selections remain one request.

Keep menu construction limited to inexpensive title, icon and availability work. Resolve filesystem selections when invoked; report unsupported shell items instead of silently sharing only part of the selection. Reuse host validation for filesystem restrictions and conflicting filenames; defer automatic name disambiguation. Pass selected roots through without scanning descendants or copying sources.

Expose one localized action, **Share with WindShare / 用 WindShare 分享**, using the existing product icon. Use the Windows 11 modern menu integration through package identity and manifest registration. [Microsoft's Explorer integration guidance](https://learn.microsoft.com/en-us/windows/apps/desktop/modernize/integrate-packaged-app-with-file-explorer) describes the COM and selection contracts.

### 4. Receive Windows Share Target activations

Register `windows.shareTarget` under the same installed application identity. Declare `StorageItems` and `SupportsAnyFileType` for WindShare's general file-sharing role; add other data formats when corresponding source handling exists. Receive desktop share activation through Windows interop alongside GPUI, acquire the complete item list asynchronously, and route it into the same `ShareIntent` and instance handoff. [Microsoft's Share Target integration guidance](https://learn.microsoft.com/en-us/windows/apps/develop/windows-integration/integrate-sharesheet-receive) covers registration and desktop activation.

Initially support local files and folders whose storage provenance establishes independence from the activation process and source application; a path or successful read alone is insufficient. Define supported source categories and verify continued access after system handoff and source exit in a minimal prototype before enabling Share Target. Reject sources requiring additional lifetime management, or whose independence cannot be established, with an actionable save-locally error, without partial sharing or staging copies. Keep these checks in the Share Target adapter; ordinary Explorer selections reuse existing filesystem validation.

Keep `ShareOperation` and related Windows objects in the activation adapter until the application owns the usable sources and intent. Report data retrieval when dependence on the source application ends, and report completion when that handoff succeeds; terminal acquisition or handoff failures are reported to Windows. System handoff completion is separate from link readiness and remote downloads, which remain owned by the existing task. [Microsoft's receive lifecycle guidance](https://learn.microsoft.com/en-us/windows/apps/develop/windows-integration/receive-shared-data) describes the status transitions.

### 5. Connect activation to the existing share experience

Restore and foreground the application on a user invocation, then present the corresponding share through the existing native flow. Maintain separate task entries for successive invocations; navigation and another share request leave earlier tasks running.

Handle transient UI state at the presentation layer while the application retains the request. A new explicit share dismisses a pending picker or quit prompt and invalidates its callback so late replies cannot replace or close newly accepted work. Once shutdown is committed, admission follows the closing behavior above. Existing setup work remains attached to its original operation; when it blocks a new share, show its progress and a direct return action. Recovery controls appear only when blocked.

Emit structured events for selection handoff, instance routing, application ownership, host submission and link readiness. Correlate instance, intent, operation and task identities, with entry source, selection counts, routing decisions, system handoff outcomes and elapsed time. This makes cold-start delay, lost requests and duplicate creation distinguishable without adding work to Explorer's menu construction.

### 6. Ship registration with the desktop installation

Extend desktop build and packaging to include the matching shell DLL, localized resources and manifests for both the Explorer command and Windows Share Target. Keep the executable and Go library layout, and use a signed identity package with an external installation location. This provides modern Explorer integration while retaining the native application layout. [Microsoft's identity-package guidance](https://learn.microsoft.com/en-us/windows/apps/desktop/modernize/grant-identity-to-nonpackaged-apps) covers signing, executable identity metadata and installer registration.

Add current-user installation and removal entry points that register and unregister both system entries through the package alongside its installed binaries. Defer updates until the application exits and affected shell binaries can be replaced, keeping the current registered target usable. Active shares need no automatic shutdown, restart or hot-update mechanism. Keep development package identity separate from the release publisher and application identity. Ordinary development builds remain runnable without signing or installation.

Carry the built-in network identity and saved configuration through the installed launch path. Explorer commands and Windows Share activations should reach the same network and recovery behavior as an ordinary desktop launch, without requiring the user to repeat setup. Official zero-configuration sharing also requires provisioned [release trust roots and a signed directory](../../deploy/network/README.md), a separate release dependency.
