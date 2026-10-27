# Native output

Native downloads share one output and recovery pipeline across Windows, Linux, and macOS. The actual destination determines the strongest available mode.

| Mode | Progress lifetime | Publication |
|---|---|---|
| Restart recovery | Verified checkpoints survive process termination | Atomic no-replace publication of the staged file |
| Live-only | Verified ranges and handles survive attempts within one process | The same no-replace publication; unfinished stages are released on exit |

Restart identity currently has versioned evidence for local ext4 on Linux, fixed NTFS on Windows, and local APFS on macOS. These provide **process-restart**, not power-loss, guarantees. Other destinations may use live-only output when their native primitives pass the required probes. Missing cleanup evidence does not disable independently established recovery.

`get` and `resume retry` reuse the operation's reservation, staged file, and verified ranges. Publication creates another name for the same data object; it does not copy the file. Temporary I/O errors retain evidence for retry. Incomplete checkpoint installation images remain untouched without revoking valid checkpoints. Collisions and uncertain ownership block affected items; unknown objects are never deleted. Lost recovery capability keeps prior operations visible and prevents silent duplicate downloads.

The shared owners are `destinationauthority` (bound destination and reservations), `outputruntime` (operation lifetime), `outputsession` (attempt settlement), `fileexecution` (range/checkpoint/publication lifecycle), `checkpointstore` (durable records), and `resumeauthority` (inventory and discard). Native adapters supply handles, identity, permissions, locking, and atomic namespace operations. Core progress explicitly distinguishes transient writes, retained process-local ranges, and restart checkpoints.

Metadata remains bounded. Live-only operations retain at most 4,096 file objects until operation completion or exit; restart mode stores bounded records on disk. Published files remain when unfinished output is discarded.

[Verified CI run](https://github.com/WindShare/windshare/actions/runs/40472469916): macOS 15.7.9 on arm64 and amd64, with actual download directories on APFS. Both runners passed file/folder CLI downloads, SIGKILL recovery, native semantic suites, and both Darwin builds. Ordinary CI runs the reusable [workflow](../.github/workflows/native-output.yml), which retains OS/filesystem evidence and traces. See [validation](validation.md) and [CLI recovery](cli.md#resume).
