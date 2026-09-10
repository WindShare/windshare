# Browser downloads

Open a share to see its file, photo, video frame preview, or folder browser. Folder names navigate; checkboxes select items across folders and pages. **Download all**, **Download this folder**, and **Download selected** name the current scope. Clearing an explicit selection never downloads everything.

Keep browsing and previewing while the current download runs. Expand **Details** for recovery progress, per-file issues, and filename restoration. **Downloads**, available on the share and home pages, keeps identifiable current and retained tasks together. Reopening a task may require the original share link or authorization for the same destination.

Downloads count the selected contents while receiving files. The task shows received or reused bytes, completed files, and current receive speed; it switches to an exact total and percentage as soon as counting finishes. Remaining time appears only with a known total and a stable receive rate. Large selections may keep counting while bounded discovery waits for transfer to catch up.

The primary download action recommends an available result. **Other ways to save** explains alternatives, including ZIP packaging, extra storage, and a later Save step. Supported browsers start the authorized download when ready. **Download started** means the browser took over; it does not prove the file was saved.

Direct ZIP saves into the chosen folder as files arrive. Normal completion appends the ZIP directory and tail to the current write session before saving, without reopening the downloaded contents just to finish the archive. Reopening requires destination permission and checks the ZIP's ownership and checkpoint data. If the unfinished file's source revision changes, WindShare keeps the verified completed files and receives that file again from its beginning. **Verify saved ZIP** can confirm a completed save locally after an interrupted completion update, without the sender. Different ZIP files can download concurrently, including into the same folder or into folders with the same name. WindShare prevents its tabs from changing the same ZIP concurrently; avoid editing or replacing an unfinished ZIP from another application.

Browser workspace downloads retain received bytes on this device. A single file becomes the saved artifact without another workspace copy. Folder ZIPs grow as files arrive and can finish locally after receiving completes. A failed continuation keeps previously received ZIP data. If finishing takes too long, WindShare waits for active save operations to finish safely and retains a completed result for saving. A saved copy uses additional device space.

Wait for **Pause** to finish before leaving. A successful pause commits received progress for resumable saving methods. A network interruption while the page remains open is different from a browser crash or forced close: after an unexpected exit, only verified checkpoints can resume.

- **Browser workspace:** checkpoints continue at fixed progress intervals as files grow. Exporting needs space for both the retained result and the saved copy, plus time to write that copy.
- **Direct ZIP:** automatic checkpoints continue as the archive grows, with increasing intervals to limit repeated copying. The unsaved portion can grow between checkpoints. The received-byte counter keeps moving; **Details** shows bytes written into the ZIP and actual restart-safe progress. Pausing saves current progress. Continuing after a checkpoint may copy the saved prefix and require comparable extra destination space.
- **Save to folder:** automatic checkpoints within a file may stop to limit repeated prefix copying. Completed files remain saved; a crash can lose most progress in a large unfinished file. Pausing still commits progress, but continuing may copy that file's saved prefix.

Checkpoint intervals are scheduling targets, not a hard maximum for crash loss; writes, storage failures, and checkpoint completion affect what can resume. Unfinished workspace downloads and results awaiting save do not expire automatically. Clearing site data or browser eviction can remove retained data. Removing a history record is separate from deleting owned unfinished output; exported files remain separate.

For a paused ZIP with complete files, eligible browsers offer **Save partial ZIP**. It exports only complete files to a separate ZIP, uses destination space when requested, and keeps the original task available to continue.
