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
| **Save to folder** | Writes directly to a chosen local folder, preserving the folder hierarchy and supporting pause and resume. A crash may lose progress on an unfinished large file; completed files remain saved. |
| **Browser workspace** | Retains content in OPFS, then starts a browser download when allowed; otherwise choose **Save**. Used for large single files and uncompressed ZIPs. The retained result remains available for another export. |
| **Direct browser download** | Used automatically for small single files (up to 16 MiB) and as a fallback when OPFS is unavailable. Progress cannot survive a page reload. |

Folder downloads preserve accepted names up to 255 UTF-8 bytes. When the browser or filesystem rejects a name (for example, a reserved device name, a browser-restricted extension, or an overlong name), WindShare deterministically saves it with a safe `.windshare-<hash>` suffix and reports the adjusted entries in task details. Download as a ZIP archive to preserve original catalog names, subject to the extractor's filesystem limits.

ZIP entry modified times are written in the receiving browser's local time zone, because ZIP date fields have no time zone and extractors read them as local time. They have 2-second precision and are clamped to 1980–2107.

## Progress & Details

- **Live indicators**: Displays received/reused bytes, completed or saved files, and transfer speed. Exact totals and percentages depend on completed item discovery; remaining time also requires a sufficiently stable transfer rate.
- **Details panel**: Displays task summaries, failure information, elapsed time, and recovery information where available.
- **Incomplete workspace ZIPs** remain available to continue. Details retains the missing count and example filenames after reload.
- **Downloads hub**: Access active and retained download tasks in this browser from the share page or home screen.

## Pause, Resume & Recovery

If the sender is temporarily unreachable when opening a link, WindShare retries for 30 seconds, then offers **Retry now**, **Continue waiting**, or **Cancel**. Continue waiting retries at a lower rate; temporary unavailability does not mean the link has expired. Invalid links are reported directly. A relay that returns malformed or unverifiable data is excluded while other relays can still connect; if every relay is excluded, the connection fails.

Keep the original page open during an active download outage. Reconnection continues automatically, preserving the download, save destination, and reusable progress. When all connections are lost, retries start promptly and gradually slow down; server cooldowns and recovery limits still apply. **Retry now** skips the ordinary retry wait without restarting the download. During an active connection attempt or a required cooldown, the button is disabled; the page shows the current activity, waiting reason, and automatic retry countdown. Healthy direct or relay transfers continue when another relay disconnects or is excluded. A verified conflict with the current share identity still ends the session. File changes, share termination, and save errors are handled separately from network waiting.

- **Available controls**: Folder and browser workspace downloads offer **Pause** and **Stop**; direct browser downloads offer **Stop** only.
- **Pause**: Saves progress supported by the chosen method and cancels that download's pending network wait. Reconnection does not resume a paused download; choose **Resume** when ready. Wait for pausing to finish before leaving the page. Remote lease cleanup runs independently after local settlement and shared-read safety; a delayed cleanup reply does not keep the download pausing.
- **Stop**: Ends receiving and cancels that download's recovery. Completed files already saved remain in the chosen folder for folder downloads, or remain in browser storage for browser-workspace ZIPs until exported or deleted.
- **Page closure or crash recovery**: Reopen the original link to recover supported downloads from the last verified checkpoint, which may lag behind received bytes. Keep destination files unchanged and site data intact; continuing may require renewed destination permission. The browser fallback must start again after a reload.
- If a source **reopens with a new revision**, folder and browser workspace downloads can retry the file once when metadata agrees. They reuse saved ranges and fetch the remainder under a fresh lease. Metadata agreement can miss edits that preserve size and modification evidence.
- **Save partial ZIP**: Available for paused or partially completed browser-workspace ZIP tasks with completed files. Exports complete files directly as a separate browser download without modifying stored data; paused tasks remain available to continue, while stopped or partially failed tasks can only be exported or deleted.

## Storage & Cleanup

- **Browser workspace vs. Destination**: Exporting a browser-workspace file or ZIP temporarily requires both browser storage and destination disk space. Browser storage quotas do not reflect destination disk free space.
- **Retained browser results**: A browser download starting does not confirm that it was saved. **Save again** on the result or in Downloads exports the retained copy without the sender. **Download from sender again** starts a separate receive. Use **Delete retained result** when no longer needed.
