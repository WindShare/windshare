# Browser Downloads

WindShare lets you preview, browse, and download shared files and folders directly in the browser without waiting for an initial full scan.

## Download Scopes

- **Download all**: Downloads everything in the share.
- **Download this folder**: Downloads the current folder and its subfolders.
- **Download selected**: Downloads explicitly checked items across folders and pages.

You can continue browsing and previewing files while downloads run in the background.

## Saving Options

WindShare recommends the best save method based on browser capabilities and the selected content:

| Save Option | Behavior | Best For |
|---|---|---|
| **Save to folder** | Writes files directly into a chosen local folder via File System Access API. Small files save directly; large or slow files stage in browser storage (OPFS) and copy upon completion for crash resilience. | Modern Chromium browsers; preserving directory structure. |
| **Direct ZIP** | Streams an archive directly into a chosen folder with incremental checkpoints. Resumable if interrupted. | Multi-file or folder downloads when a single archive file is preferred. |
| **Browser workspace** | Buffers data in browser storage (OPFS) first, then triggers a browser save/export when complete. | Browsers lacking folder access permissions or single-file downloads. |

> Under **Saving and recovery options**, choosing **Write directly to folder** bypasses browser staging completely to conserve local browser storage, but unfinished large files may lose uncheckpointed progress if the browser crashes.

## Progress & Details

- **Live indicators**: Displays received/reused bytes, completed files, and transfer speed. Exact totals, percentages, and remaining time appear once item discovery finishes.
- **Details panel**: Displays per-file transfer status, recovery checkpoints, actual elapsed time, and restart-safe progress.
- **Downloads hub**: Access active and retained download tasks anytime from the share page or home screen.

## Pause, Resume & Recovery

- **Pause vs. Stop**:
  - **Pause**: Commits current progress so transfers can safely resume later.
  - **Stop**: Cancels transfer and removes incomplete staging data while preserving already-saved files.
- **Crash recovery**: If the tab or browser unexpectedly closes, verified checkpoints allow transfers to resume from where they left off.
- **Save partial ZIP**: For paused ZIP tasks with completed files, you can export already-completed files into a standalone ZIP immediately without waiting for the rest.
- **Verify saved ZIP**: Allows verifying a completed archive locally after an interrupted write session.

## Storage & Cleanup

- **Staging vs. Destination**: Staged files temporarily occupy browser storage (OPFS) alongside the target folder until copied. Browser storage quotas do not reflect destination disk free space.
- **Automatic cleanup**: Staged files are automatically removed after destination writes are confirmed.
- **Manual cleanup**: For stopped tasks or aborted exports, use **Discard incomplete browser data** or **Retry staging cleanup** from the task card to free browser storage.
