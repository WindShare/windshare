# Receiver UI refactor

## Product direction

Opening a WindShare link should immediately explain what is shared, offer an obvious way to download it, and keep the result easy to find. A single photo opens as a photo, a video as a media preview, another single file as a file view, and folders or mixed shares as a browser.

The shared content remains the main workspace while downloads run. Each task has one primary stage and next action; browsing, preview, connection state, and saved operations have independent lifetimes. A focused transfer view is available by expanding a task, without requiring the user to leave the share whenever receiving starts.

Preserve the product's defining behavior: open and browse without a full directory scan, receive while discovering descendants, retain useful progress through interruption, and avoid unnecessary copies or a second full write. Good defaults should make these capabilities effortless. Material trade-offs remain visible at the decision that causes them.

This refactor covers presentation, frontend state ownership, task display metadata, and selection/projection changes needed for progressive saving. Existing transport, protocol, output authority, checkpoint, and recovery semantics remain authoritative. Task queues, continuous video playback, and downloads that survive switching shares are separate follow-ups.

## Problems to address

The current presentation follows subsystem ownership, so several components independently explain the same operation. The refactor replaces that composition with a shared experience model.

| Current behavior | Architectural cause | Design response |
| --- | --- | --- |
| Old tasks appear before the current share | Retained inventory is rendered as primary page content | Put identifiable operations in a Downloads drawer; suggest continuation only when relevant to this share |
| Output options, lifecycle actions, results, and progress compete | Each subsystem owns a separate headline | Use one task presenter for the active task and Downloads |
| Selecting a folder can show zero selected files and bytes | `projectBrowsePage` summarizes only visible files | Summarize selection intent independently of the current page and distinguish known totals from open discovery |
| Receiving disables directory and breadcrumb navigation | `receiveLocked` couples navigation to output authority | Separate browsing, selection drafts, and immutable task intent; derive availability per action |
| Many route buttons explain storage machinery | Candidate offers are exposed directly | Recommend one user result and group meaningful alternatives by outcome and consequences |
| A generic heading and narrow save sidebar dominate | Layout follows the application and its components | Give the shared object visual identity and move task progress into a compact expandable region |
| Saved tasks lead with repair status | Display identity is missing from the operation presentation | Persist an object/selection label, destination label, and creation time alongside operation identity |

Implementation anchors: [receiver composition](../../../web/src/ui/V2ReceiverApp.tsx), [page projection](../../../web/src/ui/v2-controller-state.ts), [controller](../../../web/src/ui/v2-controller.ts), [navigation coordinator](../../../web/src/ui/controller/navigation.ts), [artifact offers](../../../web/src/output/planning/offers.ts), and [ZIP ranking](../../../web/src/output/planning/zip-route-recommendation.ts).

## Page structure

The receiver uses a compact application header, a content workspace, an expandable current task, and user-opened details.

```text
WindShare                         Encrypted   Downloads

Shared object name                         Sender online
Filename and size, or folder breadcrumbs and entries

Photo / video / file view / folder browser

Download action and any material saving consequence

Current download: object name · stage · useful progress
                                              Details
```

The header keeps the brand small. Encryption explanation, connection details, and diagnostic export are available without occupying the main flow. Joining shows “Connecting to the sender…”; after connection, ordinary status becomes quiet. A delay or interruption becomes prominent when it affects an action or the current task.

Downloads is accessible from the receiver and landing portal. It lists current and retained operations, with an attention count for decisions. Browsing within the current share and opening Downloads or task details preserve the current task without expanding unrelated history. The current task and Downloads use the same identity, stage, and actions.

Information has three levels:

- **Result and next action:** the object, task stage, useful progress, and what the user can do now.
- **Decision consequences:** extra storage, changed output format, recovery behavior, or a later Save step beside the relevant action.
- **Details:** per-file results, filename restoration, retained data, connection paths, and diagnostic information opened by the user.

## Content and browsing

### Single-file shares

Use authenticated share structure to establish that the share contains one file. One visible row or one currently selected file does not establish the shape of the whole share. Resolve the presentation from available root metadata without scanning descendants for decorative totals.

| Content | Main surface | Primary action |
| --- | --- | --- |
| Photo | Image at its natural aspect ratio, with portrait images centered and uncropped | Download original |
| Video | Available poster or explicit frame preview | Download original, separate from Preview |
| Other file | Filename, type, known size, and supported preview | Download file |

Use existing authenticated thumbnails or posters when available. A single photo may load automatically within named byte and decoded-image budgets; otherwise require Preview. Unsupported formats and preview failures keep the original download action usable.

Reuse the existing image and MP4 frame previews and shared range cache. Preview owns its lifetime independently of downloads and releases resources when closed or replaced. Speculative loading must not delay downloads or trigger directory scans or thumbnail generation.

### Folders and mixed shares

The explorer fills the workspace. An authenticated single-folder share opens inside that folder, using its name as the page identity while preserving its output hierarchy. Desktop uses aligned filename and size columns; mobile uses filename-first rows with metadata below. Folder names navigate, file names open supported previews, and separate checkboxes select content. Essential actions remain visible without hover.

Breadcrumbs describe location. Pagination controls describe authenticated directory pages and appear separately. Loading, empty directories, omitted entries, and directory failures are explained locally; an unsuccessful directory request does not turn a healthy download into a failed task.

Outside selection mode, the primary action explicitly names its scope: “Download all” at the share root or “Download this folder” inside a directory. Entering selection mode changes the action to “Download selected”. Removing the last check leaves that action disabled with “Select items to download”; exiting selection mode restores the whole-share or current-folder action. An empty selection never silently becomes a request for everything.

Refactor the shared selection model to own subtree rules and exclusions across pages, navigation, and transfer intent. Clicking a mixed checkbox selects the whole subtree; selecting or clearing a subtree removes descendant overrides. Undiscovered descendants inherit the rule. The summary covers the entire draft and its exclusions without double-counting parent and child selections. “Select this page” remains distinct from selecting a whole folder.

Show intent immediately: “1 folder selected, excluding 2 items”. Show exact totals when already proven; otherwise reuse known catalog facts for lower bounds such as “At least 6.4 MiB found”. Browsing loads the current location, draft discovery obtains authenticated evidence needed to start saving, and task discovery follows transfer demand. Reuse cached evidence, bounded prefetch, and independent cancellation; do not traverse the whole share merely for summaries or route-cost estimates.

Establish output layout from selection intent and authenticated root/path facts independently of total discovery. Equivalent selections retain the expected names and hierarchy regardless of cache state or click timing. Incomplete statistics never justify fallback names, extra nesting, or conversion to ZIP.

Starting a task captures the selected intent and its display label. Subsequent draft changes replace only draft projections; they never reset the task's output authority or progress. Apply runtime admission limits to starting another task, with the reason beside that action; keep browsing, preview, and draft editing available while receiving. A draft is not presented as queued or scheduled work.

## Default saving behavior

Users choose a result: the original file, a folder tree, or a ZIP. The application chooses the execution route from eligible offers. Components do not infer support from a browser name or recreate storage capability rules.

Preserve existing FSA/OPFS eligibility, recommendation budgets, and route-specific recovery and space trade-offs. Ordinary small files keep their current short flow; large or unknown size alone does not select FSA. Describe user outcomes without backend names.

| Requested result and available capabilities | Default behavior |
| --- | --- |
| A single file | Keep the existing original-file default; offer direct saving to a folder as an alternative when eligible |
| A folder or multiple items with an eligible tree destination | Save to folder, preserving hierarchy |
| A folder or multiple items without a usable tree destination | Offer an eligible ZIP route and explain the format change |
| ZIP requested with multiple eligible routes | Use the existing ZIP recommendation policy and its discovery/cost evidence |
| No eligible route for the requested result | Explain the specific limitation beside the action and provide actionable desktop-receiver guidance |

“Other ways to save” opens a compact sheet comparing available outcomes. Multiple internal routes to the same outcome normally collapse into one choice. Expose a route difference only when the user gains a meaningful choice about space, recovery, or the saving process. Filename adjustments that affect the result remain visible even when the chosen route succeeds.

Use one saving-action presentation over existing offers and route-specific recommendation policies. Adjust candidate enumeration only where it hides a meaningful existing choice. Cost comparisons may remain undecided; keep an eligible progressive action available without waiting for exact totals.

Refresh recommendations as facts change before the user commits. After commitment, keep the task's result and authorized destination fixed; a changed route or destination requires the corresponding user action. Destination pickers start directly from a trusted click, with preparatory asynchronous work completed beforehand. Cancelling a picker returns to the same draft without creating a phantom task. After receiving completes, automatically hand off the authorized result where supported; show Save when a user action is needed or automatic handoff did not start.

Describe actual route costs, including OPFS export space and FSA checkpoint or resume copies. Received bytes do not imply recoverable bytes after restart. Disclose material extra space, a required later Save action, recovery limits, or conversion to ZIP beside the relevant action.

## Task experience

A task keeps its object/selection label and destination throughout its lifetime. Execution phase, blocking reason, result completeness, and publication are independent facts; the presenter derives one headline and next action. Discovery, connection health, filename changes, and cleanup remain supplementary facets.

| Primary presentation | Main information and action |
| --- | --- |
| Preparing | Preparing the requested output or acquiring a destination; cancel returns to the draft |
| Downloading | One progress headline with precise byte semantics, completed files, and measured speed when useful; Pause when supported |
| Waiting | The dependency preventing progress, what remains usable, and automatic reconnect or an appropriate action |
| Paused | Retained progress and Continue; describe route-specific recovery limits |
| Finishing | Local packaging, integrity checks, or final writing; keep the task visibly unfinished |
| Ready to save | User action is required to save the retained result; Save is primary and retention timing is visible when relevant |
| Handed to browser | “Download started — check browser downloads”; WindShare cannot claim the browser saved the file |
| Saved | A settled result and destination, with any remaining fidelity notice |
| Needs action | A resolvable obstacle and its concrete next action |
| Cancelled / Failed | Why receiving ended, any usable retained output, and whether a new attempt is possible |

The current task stays compact under the content and expands for progress details. Mobile opens task details as a full-page view with an obvious return to the share. Preparing and terminal results replace the task's controls without replacing the shared content. When receiving is paused or finished, starting another task follows the runtime's resource and output-authority state.

Open discovery uses an indeterminate indicator with bytes and completed-file counts. A determinate bar requires an exact closed denominator; changing estimates do not become an apparent percentage of the whole task. Distinguish network receipt, durable recovery progress, and final publication. Show one headline and keep supplementary counters in details. Do not derive a whole-task ETA from incomplete discovery.

Reconnect automatically on recoverable interruption and preserve the visible share and task. Show “Share ended” only from confirmed terminal facts, never timeout alone; explain when a new link is needed. Results already ready for local saving remain available without the sender.

Pause means keeping supported progress. Cancellation explains the actual disposition of destination files, partial output, and temporary data before a destructive confirmation. Deleting a history record and deleting owned unfinished output are separate operations; already exported files are not implied to disappear with history.

## Downloads and recovery

Each row begins with the shared object or selection label, followed by stage, destination when known, creation time, and the relevant action. Stable labels make similar operations distinguishable. A matching operation from the current share may produce one compact continuation suggestion; matching uses share and operation identity rather than a filename alone.

After refresh or reopening, rows distinguish local Save/finalization, share reconnection, reopening the original link when credentials are missing, and destination reauthorization. A record alone does not imply a running transfer. Local finalization proceeds automatically when authorized and safe; otherwise show the required action and consequence. Local work blocks only competing operations, leaving browsing and preview available.

Filename adjustments remain attached to the result: “1 filename was adjusted for this device.” Explain effects on projects or scripts when relevant. Restoration actions appear when the lifecycle permits them, with exact paths, sidecars, and commands inside details. Routine cleanup does not replace a successful save headline; unresolved fidelity or an action required to obtain usable output remains prominent.

Persist display metadata with the operation so a restored row is still identifiable. Keep labels separate from authority: display text never authorizes destination writes, continuation, or deletion. Active and retained views share the same task presenter and operation identity to avoid contradictory stages or duplicate entries.

## Presentation architecture

Build a receiver experience boundary that consumes domain facts and produces coherent view models and display-ready actions. React components render those models and send semantic user intents to coordinators.

The model separates these concerns:

| Concern | Ownership |
| --- | --- |
| Share identity and content kind | Authenticated catalog facts projected into photo, video, file, or browser views |
| Navigation and selection draft | Current location, pages, selection rules, and an honest selection summary |
| Task | Immutable receive intent, stable display identity, primary stage, progress, result, and available actions |
| Connection | Share availability and the effect of connectivity on current actions |
| Downloads | Operation inventory and references to the shared task presentation |
| Open details | Preview, saving alternatives, task details, and Downloads navigation |

Use discriminated unions for execution phases and content kinds, with independent completeness and publication facts. Browsing and overlays do not own task lifetimes. Preserve existing receive admission limits; sharing Downloads across views does not require a queue or cross-tab execution framework.

One task presenter reconciles lifecycle, progress, settlement, and issues. Completeness remains visible through saving and publication; available actions come from the output lifecycle, including its complete-only and partial-export rules. Cleanup does not replace the result. Both the current task and Downloads use this projection.

Keep recommendation policy pure and separate from route execution. Existing output planners establish eligible routes, coordinators correlate generations and operations, and domain services own protocol, storage authority, recovery, and settlement. Add missing product facts at their natural owner rather than guessing them in components. Retire competing presentation responsibilities as their replacement is connected.

Organize UI modules around deep behaviors: experience composition, share content and explorer, saving decisions, task presentation, Downloads and recovery, and semantic controls. Preview owns media resources; domain modules retain protocol and storage authority. Avoid splitting files into shallow wrappers that still depend on the entire controller snapshot.

Emit structured events for presentation transitions, recommendation decisions, promoted issues, and user actions. Include operation and generation identifiers, the chosen stage or result, and the decision reason. Use the existing diagnostic correlation system; keep high-frequency byte samples out of transition logs.

## Visual and responsive design

Use a neutral background, high-contrast text, fine separators, and the existing green as a restrained action accent. Let filenames and media carry the hierarchy. Replace the marketing-scale heading, nested cards, heavy shadows, and narrow permanent save rail with one content workspace.

Desktop gives the explorer useful width and opens list previews in a side sheet. Single-file media stays inline. Tablet uses overlay details; mobile uses touch-sized rows and full-screen preview or task details. Keep download actions within easy reach without covering the media, player controls, or the end of the list. Selection and task controls remain clearly associated with their own object or draft.

Essential interactions work by keyboard and touch. Preserve visible focus, full-name access when truncating, focus return after closing previews or dialogs, and meaningful status announcements. Throttle progress announcements so assistive technology hears stage changes and useful updates rather than every byte sample.

## Implementation sequence

1. **Establish contracts and ownership.** Separate navigation, selection drafts, immutable task intent, and task presentation facts. Then replace global interaction locks with action-specific availability. Implement subtree semantics in the shared selection model. Decouple authenticated output-layout proof from total discovery before bounding draft discovery; retain required root and path validation. Use a small fixture gallery for open discovery, interruption, partial results, and saving.
2. **Unify saving presentation.** Project existing offers and recommendation policies into one saving action model. Preserve progressive defaults, small-file convenience, automatic handoff, and trusted-click destination acquisition.
3. **Connect a complete single-file flow early.** Wire the new boundary into production for opening a file, choosing a destination, receiving, interruption, recovery, and final saving. Verify real authority and lifecycle behavior before expanding layouts.
4. **Extend content experiences.** Add the explorer, cross-page selection, direct entry into single folders, and bounded photo loading. Reuse existing video previews. Tune responsive layouts with portrait media, long filenames, absent previews, and browsing during downloads.
5. **Integrate Downloads and recovery.** Persist display metadata, share the entry with the portal, and reuse existing task and session lifetimes. Connect continuation, local finalization, authorization, filename restoration, and deletion to existing authority owners.
6. **Retire replaced presentation.** Remove old responsibilities as each flow lands, then finish removing the monolithic composition and obsolete copy. Exercise shared decisions with pure fixtures and focused component scenarios.
