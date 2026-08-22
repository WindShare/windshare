# CLI

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
| `auto` | Uses direct WebRTC and relay as available. |
| `relay-only` | Transfers content through the relay. |
| `p2p-only` | Uses the relay for setup, transfers content directly, and stops if the direct connection fails. |

The final result is `success`, `partial`, `paused`, or `failed`. Exit codes are: `0` success, `1` runtime failure, `2` invalid command, `3` network failure, and `4` shared content changed.

## Diagnostics

- `-v` or `--verbose` prints additional connection and protocol status to stderr.
- `--trace <file>` creates one NDJSON trace. The file must not already exist.
- `--trace-dir <directory>` creates one run-specific NDJSON trace in that directory.
- `--trace` and `--trace-dir` cannot be used together.

Traces may contain filenames, local paths, and connection details. They exclude links, keys, credentials, and file content.

## Resume

Running the same compatible `get` again in the same output directory resumes verified data.

```text
wind resume list -o <directory>
wind resume discard -o <directory> --item <N>
```

`resume list` shows unfinished downloads. `resume discard` requires interactive confirmation and removes the selected unfinished state. Completed and unrecognized files are not removed.
