# Browser Downloads

WindShare lets you browse shared files and folders without waiting for an initial full scan, then preview or download them directly in the browser.

## Download Scopes

- **Download all**: Downloads everything in the share.
- **Download this folder**: Downloads the current folder and its subfolders.
- **Download selected**: Downloads explicitly checked items across folders and pages.

You can continue browsing and previewing files while downloads run in the background.

## Saving Options

WindShare recommends a save method based on browser capabilities, available storage, and the selected content. Open **Other ways to save** for alternatives.

| Save Method | Behavior |
|---|---|
| **Save to folder** | Preserves the folder hierarchy in a chosen local folder. Small files save directly; larger or slower files may first use browser storage (OPFS), depending on recovery costs and available staging capacity. Each staged file is copied to the folder when complete. |
| **Save ZIP to a folder** (Direct ZIP) | Streams an uncompressed archive into a chosen folder with verified resume checkpoints. The ZIP becomes usable after closing and verification finish. Checkpointing and resuming may require temporary destination space for a copy of the saved prefix. |
| **Browser workspace** | Retains content in OPFS, then starts a browser download when allowed; otherwise choose **Save**. Actions include **Download original** and **Receive ZIP, then save**. The retained result remains available for another export. |
| **Browser fallback** | A browser download with a size limit may be offered as a fallback. It checks that the complete result fits before receiving. Progress cannot survive a page reload. |

> Under **Other ways to save**, expand **Saving and recovery options**. Choosing **Write directly to folder** bypasses file staging in browser storage. A crash may lose most progress in a large unfinished file; completed files remain saved.

## Progress & Details

- **Live indicators**: Displays received/reused bytes, completed or saved files, and transfer speed. Exact totals and percentages depend on completed item discovery; remaining time also requires a sufficiently stable transfer rate.
- **Details panel**: Displays task summaries, failure information, elapsed time, and recovery information where available.
- **Downloads hub**: Access active and retained download tasks in this browser from the share page or home screen.

## Pause, Resume & Recovery

- **Available controls**: Folder downloads offer **Pause** and **Stop**. Direct ZIP and browser workspace downloads offer **Pause**; the browser fallback offers **Stop** only.
- **Pause**: Saves progress supported by the chosen method. Wait for pausing to finish before leaving the page.
- **Stop**: Ends receiving. For folder downloads, incomplete browser staging is removed while saved files remain; complete staged files can still be saved locally. Failed cleanup can be retried.
- **Crash recovery**: Resumable methods use the last verified checkpoint, which may lag behind received bytes. Keep destination files unchanged and site data intact; continuing may require the original share link and renewed destination permission. The browser fallback must start again after a reload.
- **Save partial ZIP**: Available for retained browser-workspace ZIP tasks with completed files when the browser supports a file save picker. Exports complete files as a separate ZIP while keeping the task available for continuation.
- **Verify saved ZIP**: For Direct ZIP tasks requiring completion verification after an interrupted write session, verifies the saved archive locally.

## Storage & Cleanup

- **Staging vs. Destination**: Staging and export can temporarily require both browser storage and a destination copy. Browser storage quotas do not reflect destination disk free space.
- **Folder staging cleanup**: Each staged file is removed after its destination write is confirmed. Use **Retry staging cleanup** for pending cleanup, or **Discard incomplete browser data** for incomplete staging left by an ended task.
- **Retained browser results**: A browser download starting does not confirm that it was saved. Workspace results remain available for another export; use **Delete retained result** when no longer needed.
