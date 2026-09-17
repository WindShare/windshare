# CLI

See [installation and optional first setup](installation.md).

## Share

```text
wind share <path...>
wind share <path...> --split-key
```

After at least one relay confirms publication, `share` prints:

```text
Link: <link>
```

`--split-key` prints the link and key separately:

```text
Bare link: <link-without-key>
Key: <key>
```

Links and keys go to stdout. Status and errors go to stderr.

If every relay is temporarily unreachable, `share` waits and retries until one is ready; Ctrl+C cancels waiting. Each relay recovers independently, including those unavailable at startup. Later outages retain the original share link. Status shows how many relays currently admit new receivers, ongoing recovery, and successful reconnection; healthy direct transfers can continue while relay access recovers. Stopping or exiting the sender ends the share.

Windows sources are admitted by file identity and write-excluding handle capabilities, including eligible removable and network volumes. Filesystems with weak change metadata keep the same revision while its handle remains open; reopening creates a new revision, so old download ranges are never mixed with potentially changed content.

## Download

```text
wind get <link>
wind get -o <directory> <link>
wind get --only <path> <link>
wind get <bare-link> --key <key>
wind get --connectivity auto|relay-only|p2p-only <link>
wind get --wait-timeout 2m <link>
```

`-o` selects the output directory and defaults to the current directory. `--only` may be repeated to select multiple paths.

| Selection | Saved as |
|---|---|
| One file | `<filename>` |
| One directory | `<directory>/...` |
| Part of one directory | `<directory>-selection/...` |
| Multiple roots | `windshare/...` |

Existing files are not overwritten. A name collision creates a suffixed destination. Names are shortened
on Unicode character boundaries to fit the output limit; extensions are preserved unless they leave no
room for a filename character and the collision suffix.

| Mode | Behavior |
|---|---|
| `auto` | Starts on the first available path; direct WebRTC and relay may carry content together. |
| `relay-only` | Transfers content through the relay. |
| `p2p-only` | Uses relays for setup, transfers content only directly, and stops if direct recovery is exhausted. |

Temporary outages reconnect automatically, keeping the same download, output, and verified progress while the shared file revision remains unchanged. After fast retries, recovery continues at a lower rate; healthy direct paths can keep transferring while a relay recovers. Authentication, changed content, and output failures retain their own failure handling.

By default, the first connection waits up to 10 seconds; an established download waits through later connection outages until cancelled. `--wait-timeout <duration>` (for example, `30s` or `2m`) limits the initial connection and each later full connection outage separately. Healthy transfer time does not consume this limit. `0` selects the defaults; negative durations are invalid. Press Ctrl+C to cancel waiting. If the first connection expires, retry the original command or increase the wait timeout; temporary unavailability does not prove the link has expired.

The final result is `success`, `partial`, `paused`, or `failed`. Exit codes are: `0` success, `1` runtime failure, `2` invalid command, `3` network failure, and `4` shared content changed.

## Diagnostics

- `-v` or `--verbose` prints additional connection and protocol status to stderr.
- `--trace <file>` creates one NDJSON trace. The file must not already exist.
- `--trace-dir <directory>` creates one run-specific NDJSON trace in that directory.
- `--trace` and `--trace-dir` cannot be used together.

Traces may contain filenames, local paths, and connection details. They exclude links, keys, credentials, and file content.

## Resume

On destinations with recovery support, running the same compatible `get` again in the same output directory resumes staged data.
Safe destinations without recovery support can still receive files while WindShare remains open; the CLI explains this limit before receiving content. Normal cleanup uses retained file handles. After an unexpected exit, unrecognized unfinished files are left for manual cleanup, never reopened or deleted by name alone.
Previously delivered files keep their completion receipts without rereading or revalidating their current contents.
Resume trusts private staged data to remain unchanged; it does not detect arbitrary external edits.

```text
wind resume list -o <directory>
wind resume discard -o <directory> --item <N>
```

`resume list` shows unfinished downloads. `resume discard` requires interactive confirmation and removes the selected unfinished state. Completed and unrecognized files are not removed.
