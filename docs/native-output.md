# Native output

Windows, Linux, and macOS share one best-effort resume lifecycle in `osfs/internal/output`. All private scratch state lives under `<destination>/.windshare-tmp/<operation-id>/`, keeping user output trees free of sidecar identity files. Platform files supply atomic no-replace publication (`renameat2`/`renameatx_np`/`NtSetInformationFile`), exclusive OS locks (`flock`/`LockFileEx`), no-follow directory traversal, and handle-bound timestamps. Recovery does not require hard links, filesystem-specific object IDs, or extended attributes.

Two-level locking coordinates concurrent receivers: a short-lived destination-container lock (`flock` on the destination directory on Unix; a container-identity `Local\` named mutex on Windows) serializes `.windshare-tmp` lookup, creation, and removal without leaving lock files behind, while each active task holds an exclusive `<operation-id>/task.lock` that the kernel releases automatically on crash.

Each operation keeps an append-only, CRC32-checksummed `state.log` (automatically compacted as records churn) recording the frozen intent, compatible-name mappings, partial range progress, and delivery receipts. Continuation skips files already delivered by that task even if their public copies were moved, deleted, replaced, or the source changed. Reopening a source file reuses partial ranges when comparison metadata agrees and blocks only affected files on observed change or unavailable evidence.

Durability is tiered by file size to avoid small-file I/O amplification while protecting large transfers across restarts:

- **Small files (`< 8 MiB`)**: staged in `.windshare-tmp/<op>/`, closed, published via one atomic no-replace rename with zero per-file `fsync`, and logged to `state.log` (flushed in batches every 64 files / 2 seconds and on pause/exit).
- **Large files (`>= 8 MiB`)**: range checkpoints flush `.part` data before updating `state.log` every 8 MiB or 2 seconds idle, and on explicit checkpoint/pause/commit.

`get` automatically continues a matching task. `get --redownload` reserves a separate collision-safe destination and leaves the original continuation target intact; `resume retry --operation <id>` selects either task. Completion or `resume discard` removes `.windshare-tmp/<op>/` (and `.windshare-tmp/` when empty) while preserving all published and foreign files.

Source names are preserved when the destination accepts them. Names rejected by the destination or colliding with `.windshare-tmp*` at the destination container root receive a stable compatible spelling retaining usable extensions; saved mappings are persisted in `state.log`, reused on resume, and emitted via structured diagnostics without writing report files beside the destination.

See [CLI recovery](cli.md#resume) and [validation](validation.md).
