# R0 feasibility evidence

These tests freeze feasibility boundaries without creating a production v2 path.

| Boundary | R0 result |
|---|---|
| Windows NTFS/ReFS source | Supported by an open handle that denies `FILE_SHARE_WRITE`; volume/file identity survives path replacement. |
| Linux local regular file | Supported with an open-FD `device+inode+size+mtime(ns)+ctime(ns)` token checked around reads. |
| macOS local regular file | Same POSIX token contract; compile-checked, runtime evidence remains a platform CI responsibility. |
| Other/network/pseudo filesystems | `unsupported_stability` until the exact backend proves equivalent identity and mutation behavior. |
| Catalog metadata spill | Every injected write/flush/install failure aborts the transaction and removes temporary visibility. |
| Output durability | Crash cuts publish only after reopen verification; Chromium OPFS survives reload and a fresh browser process using the same profile. FSA remains `None` until reauthorization/identity evidence exists; no Web backend claims `PowerLoss` in R0. |
| Direct stream/ZIP | `None`, no rollback or file-failure isolation after the first byte; later failure aborts the job. |

Focused commands:

```text
go test -count=1 ./spikes/r0contract
pnpm -C web exec playwright test --config test/browser/r0-storage.playwright.config.ts
```
