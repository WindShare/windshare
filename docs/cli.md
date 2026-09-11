# CLI

See [installation and optional first setup](installation.md).

## Share

```text
wind share <path...>
wind share <path...> --split-key
```

After connecting to the relay, `share` prints:

```text
Link: <link>
```

`--split-key` prints the link and key separately:

```text
Bare link: <link-without-key>
Key: <key>
```

Links and keys go to stdout. Status and errors go to stderr.

Windows sources are admitted by file identity and write-excluding handle capabilities, including eligible removable and network volumes. Filesystems with weak change metadata keep the same revision while its handle remains open; reopening creates a new revision, so old download ranges are never mixed with potentially changed content.

## Download

```text
wind get <link>
wind get -o <directory> <link>
wind get --only <path> <link>
wind get <bare-link> --key <key>
wind get --connectivity auto|relay-only|p2p-only <link>
```

`-o` selects the output directory and defaults to the current directory. `--only` may be repeated to select multiple paths.

| Selection | Saved as |
|---|---|
| One file | `<filename>` |
| One directory | `<directory>/...` |
| Part of one directory | `<directory>-selection/...` |
| Multiple roots | `windshare/...` |

Existing files are not overwritten. A name collision creates a suffixed destination.

| Mode | Behavior |
|---|---|
| `auto` | Starts on the first available path; direct WebRTC and relay may carry content together. |
| `relay-only` | Transfers content through the relay. |
| `p2p-only` | Uses relays for setup, transfers content only directly, and stops if direct recovery is exhausted. |

Connection recovery keeps the same output and verified progress while the shared file revision remains unchanged.

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
