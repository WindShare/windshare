# CLI

## Share

```text
windshare share <path...>
windshare share <path...> --split-key
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
windshare get <capability-link>
windshare get -o <directory> <capability-link>
windshare get --connectivity p2p-only <capability-link>
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
| `auto` | Attempts direct WebRTC and admits relay content when fallback policy requires it. |
| `relay-only` | Skips peer setup and transfers content through the application relay. |
| `p2p-only` | Uses the relay only for bootstrap, control, and signaling; direct-path failure stops the download. |

While authenticated discovery is open, progress reports discovered files and ready bytes without a percentage or ETA. A percentage requires complete discovery and exact counters. Rate uses newly verified unique bytes; ETA additionally requires a stable nonzero rate, remaining work, and no known non-success file outcome. Verified content waiting for publication is shown as finalizing.

The final result is `success`, `partial`, `paused`, or `failed`; source drift is reported separately and exits with code 4. Output lists downloaded, resumed, paused, collision, item-blocked, and failed outcomes separately. Only success uses `Download completed`, and the destination is the authority-selected published path.

## Output and diagnostics

- Capability information is always written to stdout. It is never copied to stderr, verbose output, or trace.
- Human status, progress, warnings, errors, and results are written to stderr. Redirected stderr has no ANSI or dynamic refresh and, by default, keeps only warnings, errors, and final results.
- `-v` and `--verbose` add static handshake, reconnect, lane, fallback, and failed protocol-operation diagnostics without changing capability output, results, or exit codes.
- `--trace <file>` writes versioned private-safe NDJSON to a separate file. Protocol-operation records correlate request and response delivery by random operation ID without recording request bodies. Successful per-block and streaming-response milestones are omitted, and trace recording never waits for file I/O. `--trace=-` is rejected.
- Trace open failure prevents relay or output mutation. A later write, flush, or queue failure warns once that the trace is incomplete but does not cancel or reclassify the transfer.

Trace records use full semantic run/session/operation identifiers and normalized relay authority. They exclude capability links and keys, tokens, private keys, filenames, catalog and local paths, command lines, environment values, raw content, and unfiltered provider text. The private process-event pipe used by repository tests is a separate correctness authority.

## Resume state

A destination is `resumable` only when it proves safe no-replace publication, operation recovery, verified-range recovery, and crash cleanup. Repeating the same `get` continues the matching active operation. A `live-only` destination preserves completed files after interruption but starts a new named operation on the next `get`.

```text
windshare resume list -o <directory>
windshare resume discard -o <directory> --item <N>
```

`resume list` reports destination-owned `incomplete`, `resumable`, `cleanup-pending`, `operation-needs-attention`, and `item-blocked` state. `resume discard` requires exact interactive confirmation and removes only identity-matched unfinished WindShare state; published files and unknown objects are preserved.
