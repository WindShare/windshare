# Web Runtime Modernization Execution Plan

Date: 2026-11-13

## Purpose and evidence

Prioritize opening a share, seeing files, and starting a download. Keep the homepage lightweight and browsing responsive during transfers, while retaining the existing native UI primitives.

The source review and local production bundle establish the following:

| Finding | Evidence | Architectural implication |
| --- | --- | --- |
| The homepage loads the receiver's application graph. | The entry JS is approximately 2.66 MB minified / 705 KB gzip. `web/src/main.tsx` assembles receiving, output, recovery, and diagnostics before rendering. `ui/portal/PortalApp.tsx` consumes the complete `ReceiverController`. | Separate entry ownership and runtime loading. Component-only lazy loading cannot remove dependencies still reachable through startup assembly. |
| Progress publication mixes accounting frequency with presentation frequency. | `transfer/progress/content.ts` limits receipt notifications to 250 ms, while `transfer/job.ts` publishes on atomic writes and recoverability updates. `ui/controller.ts` publishes a complete snapshot, and `ui/ReceiverApp.tsx` recomposes task presentation. | Put numerical presentation cadence at one application boundary and give UI regions independent subscriptions. |
| Popover fallback placement is overwritten. | `ui/portal/styles/popover.css` sets `inset: auto` and `margin: 0` unconditionally while using `anchor()`. | Preserve a usable default layout and apply anchored placement only when supported. |
| Modal scroll containment is incomplete. | `ui/receiver-presentation/sheets.css` contains scrolling inside the dialog, but its backdrop only defines a background color. | Extend the existing modal primitive's CSS instead of introducing a second overlay mechanism. |

Startup already begins joining without awaiting retained inventory; the loading cost is the eager module graph and concurrent initialization. Bundle size is measured; transfer-time interaction cost has not been measured. The progress finding identifies a synchronous dependency chain, not a quantified responsiveness regression.

The deterministic guidance scan reported one Web finding, concerning password autofill. It is a share-key input rather than an account sign-in form, so that finding is excluded. The scan covers only a subset of the guide catalog. Existing native dialog/popover use, container queries, appearance handling, and dynamic imports for `mp4box` and curve algorithms remain useful foundations.

## 1. Separate document startup from receiver ownership

Primary files: `web/src/main.tsx`, `web/src/App.tsx`, `ui/capability/location.ts`, `ui/controller.ts`, and `ui/portal/PortalApp.tsx`.

Introduce a small document-level owner for initial location capture, page lifecycle, runtime loading, and incoming user intents. Keep receiver assembly in a dynamically imported module with an explicit factory and disposal contract. Use consumer-defined interfaces for homepage commands and downloads access; avoid pulling runtime implementations through shared barrel exports.

The startup sequence is:

1. Synchronously capture the capability and remove its sensitive URL fragment. Keep this operation and its dependencies in the small entry module, before any asynchronous import or fallible storage work.
2. Establish lightweight startup error reporting and apply the existing appearance/language preferences before the first paint.
3. If a share capability is present, immediately start loading the receiver and show its opening state. Begin connection work as soon as the runtime is ready, without waiting for retained inventory, unrelated output providers, or another user action.
4. Otherwise, render the homepage. Submitting a link or requesting downloads starts the corresponding runtime work.

Represent runtime loading with a discriminated state that owns its pending work, result, or failure. Share a single in-flight load per document. Incoming intents retain their operation identity and follow existing replacement rules; late completions must not recreate a disposed owner or apply an obsolete intent.

Keep URL observation outside React remounts. A persisted page retains its owner; a real document exit releases it. Preserve the current intent in memory for loading retries and record startup attempt identifiers. If in-page retry cannot recover, direct the user to reopen the original link; automatic refresh would lose its erased capability. This needs no capability persistence or cross-version recovery system.

### Homepage and retained downloads

Retain the downloads entry on the homepage. Opening Downloads ensures inventory is loaded: reuse loaded state and share one in-flight request, keeping the panel open while it loads. Model unloaded, loading, empty, available, and failed inventory states distinctly. A first visit should not enumerate recovery storage merely to display the landing page.

Loading or refreshing inventory must preserve pending actions and their inventory authority. Opening Downloads only discovers and displays tasks; finishing a retained task locally requires an explicit action such as **Finish and save**. After a share's first directory is visible, discover matching retained tasks asynchronously without blocking new downloads.

Discovery still performs provider-owned crash-consistency repairs needed to classify records, including workspace activation and Direct ZIP bootstrap recovery. Keep these repairs separate from receive continuation, finalization, and export; no separate background recovery service is needed.

Move receive-only CSS with the receiving surface. Keep homepage and document-level styles with their actual owners. Preserve the current theme behavior across either entry path.

## 2. Load output implementations at their ownership boundary

Primary files: `application/browser-receive/composition.ts`, its route factories, `output/capability/acquisition.ts`, and `application/browser-receive/retained/`.

Separate capability facts and output offers from destination implementations. Use a few route-level splits, prepare likely save routes while browsing, and load recovery or repair implementations for the records that need them. Keep route support, module readiness, and acquired authority distinct: an unloaded implementation does not make a supported route unavailable.

Keep the chosen route, acquired handle, and operation identity together under the existing authority contracts. Module loading must not produce a second destination selection, restart a transfer, or introduce another copy of its state machine.

Apply these activation rules to fresh downloads, retained reauthorization, and publication pickers:

- Keep the small acquisition adapter available before presenting an enabled picker action. Invoke the picker synchronously in the click handler.
- Resolve any prerequisites that must precede acquisition during route preparation. Heavy work that can follow acquisition may load after the handle is obtained.
- Do not insert an awaited dynamic import between a click and its picker invocation, or require a second click to recover lost user activation.
- Reuse existing preparation and failure presentation when a required module is not ready.

Retain lightweight diagnostic capture throughout startup and receiving. Separate export formatting and optional diagnostic UI where their dependencies permit; do not defer critical incident recording until someone opens diagnostics.

Use module boundaries to produce the split. Leave bundler configuration responsible for packaging those boundaries rather than imposing arbitrary vendor chunks. Existing lazy crypto fallbacks and MP4 support retain their current behavior.

## 3. Separate progress facts from UI snapshot publication

Primary files: `transfer/job.ts`, `transfer/job/observers.ts`, `transfer/progress/`, `application/receive/active/coordinator.ts`, `ui/controller.ts`, and `ui/experience/task-composition.ts`.

The transfer ledger remains authoritative for every accepted write, recoverable range, failure, and settlement. Introduce a small progress observation contract through which the application can read the latest facts and receive semantic transitions. High-frequency counter changes should not require constructing the full product snapshot immediately.

The application presentation owner controls UI snapshot publication, separately from output publication:

- Coalesce numerical changes into one pending update. Reuse the existing 250 ms cadence as the initial named presentation policy, with an injected clock/scheduler.
- Publish phase changes, discovery completion, action availability, failures, pause/cancel outcomes, and completion immediately, including the latest counters in the same revision.
- Bind pending updates to their operation and generation. Replacing or disposing an operation cancels its scheduled publication.
- Keep `ReceiveRuntimeState` facts, including action-relevant `browserFolderProgress.summary`, current. Throttle only presentation notifications; commands, output authority, checkpointing, settlement, and performance milestones use authoritative facts.
- Do not make authoritative state advancement depend on animation frames or foreground visibility. A returning page reads current facts.

Move receipt, write, recovery, and Direct ZIP progress onto this common UI snapshot boundary before retiring duplicated presentation throttles. Keep source accounting immediate and preserve the distinction between receipt, writes, recoverability, and output publication without building full UI snapshots at fragment frequency.

Replace the whole-page subscription with stable projections for browsing, connection state, active-task progress, and retained downloads. Derive related projections from the same committed presentation revision. Keep snapshot identity unchanged when a projection's inputs have not changed.

Make task composition depend on the task inputs it actually consumes. A byte-count update should update the active task's presentation without sorting historical tasks or rebuilding the file explorer. Keep required cross-state relationships explicit so that an immediate lifecycle transition cannot leave stale actions visible.

Use browser performance traces first. Add bounded, feature-detected Event Timing or Long Animation Frame attribution to existing diagnostic capture only when needed to explain measured stalls. Task slicing or workers require evidence of substantial main-thread work; avoid a general scheduler framework.

## 4. Complete native overlay behavior

Primary files: `ui/portal/styles/popover.css`, `ui/portal/PortalPopover.tsx`, `ui/receiver-presentation/sheets.css`, and `ui/controls/DetailSheet.tsx`.

### Popover placement

Give the panel a usable default placement. Put the `anchor()` positioning and the inset/margin resets that enable it inside an `@supports` condition for the positioning syntax actually used. Preserve the current width, viewport bounds, and overflow behavior.

Use the browser's default centered placement as the lightweight fallback. Keep native invocation, light dismissal, focus behavior, and current keyboard semantics. This change does not need a positioning library or a broad browser-version detection layer.

### Modal scrolling

Keep `showModal()`, the existing invoker focus restoration, and nested-dialog cancellation handling. Extend the shared backdrop rule with `overflow: hidden` and `overscroll-behavior: contain`, alongside the dialog's existing containment.

Treat containment on non-scrollable backdrops as progressive enhancement. Syntax support alone does not establish that behavior in a browser. Preserve native inertness and focus handling where complete scroll containment is unavailable; avoid root overflow mutation and its effects on scrollbar layout and scroll position.

## Delivery sequence and scope

Record a small baseline for share opening (first directory, save readiness, first received data) and browsing during transfer. Implement document/runtime separation and coarse output splits as one loading improvement, comparing the full share-opening path. Establish the progress observation contract before migrating UI subscriptions. Overlay fixes can ship independently.

Remove superseded eager imports, whole-page subscriptions, and duplicate presentation throttles as their replacements take ownership. Reuse existing operation, generation, and authority ownership without a parallel business state machine. This work requires no new state-management dependency, service worker, or protocol/Go/Rust transport changes.

## Implementation and local evidence — 2026-11-14

Implemented document-owned lazy receiver startup, explicit retained discovery/actions, prepared output and reauthorization routes, one application-owned progress cadence, independent UI projections, and native overlay fallbacks. Retained action implementations are prepared before discovery exposes their actions, preserving offline local recovery. Diagnostic capture and export formatting remain together because automatic archives and the synchronous developer export API use the same formatter; the optional diagnostic panel loads separately.

`make web`, `make browser`, and `make sloc` pass: 4,008 tests, 3 real sender/relay smoke scenarios, and 196 Chromium browser contracts. Browser coverage includes offline recovery, same-document capability replacement, saved language/appearance, and nested-dialog focus. Production screenshots also verified the cold Downloads loading surface and focus restoration after runtime adoption.

Local comparison: Chromium, production preview served directly, HTTP cache disabled, real Go sender and Rust relay, portable ZIP, 16 files of 1 MiB, tracing enabled. Times below are medians of three alternating runs per build. Directory/save readiness is observed inside the page when rows and an enabled action appear, avoiding automation polling delay.

| Measurement | Before | After |
| --- | ---: | ---: |
| Homepage JS, all loaded chunks, minified bytes | 2,661,513 | 502,427 |
| Same JS compressed with gzip, bytes | 697,942 | 148,758 |
| First directory rendered, ms from navigation | 144.4 | 160.4 |
| Download action ready, ms from navigation | 144.5 | 173.9 |
| Download click request to first bulk relay frame, ms | 83.5 | 89.2 |
| Longest main-thread task during transfer/interactions, ms | 21.9 | 15.6 |
| Maximum reported interaction duration per run, ms | 32 | 16 |

The bulk-frame measurement observes the first incoming frame above 32 KiB after the automated click request; it is a transport proxy, not authenticated-content completion. Interaction traces cover eight Downloads open/close cycles; Event Timing has a 16 ms reporting threshold. All six runs completed without page errors or main-thread tasks above 50 ms. These small local samples establish the payload reduction and expose a modest cold-route loading cost; they do not establish field INP improvement or a general share-opening speedup. No additional production performance telemetry, worker, or task scheduler was justified.

Local artifacts are under the ignored `web/test-results/runtime-modernization/`: `summary.json`, `*-observed-*.json`, matching traces/diagnostic exports, production screenshots, gate logs, and the measurement harness.

## Guidance sources

- [Performance: JavaScript code splitting and main-thread work](https://github.com/GoogleChrome/modern-web-guidance/blob/main/skills/modern-web-guidance/guides/performance/performance.md)
- [Identify causes of poor INP](https://github.com/GoogleChrome/modern-web-guidance/blob/main/skills/modern-web-guidance/guides/performance/identify-inp-causes.md)
- [Viewport-aware dropdowns](https://github.com/GoogleChrome/modern-web-guidance/blob/main/skills/modern-web-guidance/guides/ui-atoms/viewport-aware-dropdowns.md)
- [Prevent page scrolling underneath modal dialogs](https://github.com/GoogleChrome/modern-web-guidance/blob/main/skills/modern-web-guidance/guides/ui-behaviors/prevent-page-scroll-under-modal-dialog.md)
