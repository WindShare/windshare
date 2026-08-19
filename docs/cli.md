# CLI

## Share

```text
wind share <path...>
wind share <path...> --split-key
```

`share` registers with the relay before publishing capability information. Standard output contains only the copyable capability:

```text
Link: <complete-capability-link>
```

With `--split-key`, it contains exactly:

```text
Bare link: <link-without-key>
Key: <key>
```

The link is available before directory descendants are scanned. Human readiness, relay state, warnings, and the final `Sharing stopped` result use standard error. A capability publication failure stops the share and never reports ready.

## Get

```text
wind get <capability-link>
wind get -o <directory> <capability-link>
wind get --connectivity p2p-only <capability-link>
```

Without `-o`, WindShare saves into the current directory. The directory is a container for the result:

| Selection | Layout inside the container |
|---|---|
| One file | `<filename>` |
| One complete directory | `<directory>/...` |
| Part of one directory | `<directory>-selection/...` |
| Multiple roots or a synthetic selection | `windshare/...` |

WindShare never overwrites an existing object, follows a link, or merges unrelated results. A new operation chooses a stable suffix after a name collision; the same active operation reuses its reserved name. Unsafe ancestry, links, or insufficient access fail closed.

`--connectivity` controls the content path:

| Mode | Behavior |
|---|---|
| `auto` | Attempts direct WebRTC and may admit relay content adaptively; `Direct + Relay` means both paths are usable, not that fallback occurred. |
| `relay-only` | Skips peer setup and transfers content through the application relay. |
| `p2p-only` | Uses the relay only for bootstrap, control, and signaling; direct-path failure stops the download. |

While authenticated discovery is open, progress reports discovered files and ready bytes without a percentage or ETA. A percentage requires complete discovery and exact counters. Rate uses newly verified unique bytes; ETA additionally requires a stable nonzero rate, remaining work, and no known non-success file outcome. Verified content waiting for publication is shown as finalizing.

The final result is `success`, `partial`, `paused`, or `failed`. Authenticated sender evidence change on reopen or during a read invalidates that revision and exits with code 4; a stored checkpoint for another revision is instead item-local `revision-conflict`, preserves prior output, and lets independent siblings continue. Output lists downloaded, resumed, paused, collision, item-blocked, and failed outcomes separately. A collision-only partial says existing destinations prevented completion and is never reported as `unexpected`. Only success uses `Download completed`, and the destination is the authority-selected published path.

## Output and diagnostics

- Capability information is always written to stdout. It is never copied to stderr, verbose output, or trace.
- Human status, progress, warnings, errors, and results are written to stderr. Redirected stderr has no ANSI or dynamic refresh and, by default, keeps only warnings, errors, and final results.
- `-v` and `--verbose` add static handshake, reconnect, lane, fallback, and failed protocol-operation diagnostics without changing capability output, results, or exit codes.
- `--trace <file>` claims that exact path for one new versioned private-safe NDJSON file. An existing path is preserved and the command fails before relay or output mutation; append, overwrite, and `--trace=-` are unsupported.
- `--trace-dir <directory>` creates the directory when needed, writes one compactly named run-specific NDJSON file, and reports its generated path on stderr. Each trace file belongs to one command invocation; WindShare never rotates or deletes traces, so retention remains user-owned.
- Protocol-operation records omit successful block/streaming milestones. `content_path` reports usable paths, while terminal `lane_settlement` records summarize authenticated blocks and bytes delivered by each relay or direct lane.
- Trace recording never waits for file I/O. Projection, queue, write, or flush loss emits bounded `observer_loss` evidence when possible and one `Trace is incomplete` warning, without cancelling or reclassifying the transfer.

Trace records use full semantic run/session/operation identifiers and normalized relay authority. They exclude capability links and keys, tokens, private keys, filenames, catalog and local paths, command lines, environment values, raw content, and unfiltered provider text. The private process-event pipe used by repository tests is a separate correctness authority.

## Resume state

A destination is `resumable` only when it proves safe no-replace publication, operation recovery, verified-range recovery, and crash cleanup. Repeating the same compatible `get` continues the matching active operation. If the sender released its file handle after the resume grace, an exact source-evidence match reopens the same revision with a new lease, so verified ranges still resume; temporary inability to compare remains retryable, while proven change invalidates the revision instead of silently replacing it. For repeat-run tracing, use a run directory so every attempt keeps separate evidence:

```text
wind get -o <directory> <same-capability-link> --trace-dir <trace-directory>
```

A `live-only` destination preserves completed files after interruption but starts a new named operation on the next `get`.

```text
wind resume list -o <directory>
wind resume discard -o <directory> --item <N>
```

`resume list` reports destination-owned `incomplete`, `resumable`, `cleanup-pending`, `operation-needs-attention`, and `item-blocked` state. Item-local reasons distinguish `revision-conflict`, `owned-object-unknown`, and `checkpoint-invalid`; only uncertain root, registry, lease, or operation ownership stops the whole operation. Failures use closed `stage`, optional `reconciliation_stage`, and optional `native_error_class` fields so destination binding, inventory, operation acquisition, checkpoint reconciliation, native durability, and authority close remain distinguishable without exposing paths or provider text. `resume discard` requires exact interactive confirmation and removes only identity-matched unfinished WindShare state; published files and unknown objects are preserved.
