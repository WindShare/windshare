# CLI receive output

## Get

```text
windshare get <capability-link>
windshare get -o <directory> <capability-link>
```

Without `-o`, WindShare saves into the current directory. With `-o`, the directory is always a container for the result, not the result root itself. The container may already exist; WindShare can also create a missing container from its nearest existing safe parent.

| Selection | Layout inside the container |
|---|---|
| One file | `<filename>` |
| One complete directory | `<directory>/...` |
| Part of one directory | `<directory>-selection/...` |
| Multiple roots or a synthetic selection | `windshare/...` |

New public files and directories use their public parent's ordinary inherited permissions. WindShare checks the permissions needed for each mutation; it does not require an exclusive download directory or rewrite ACLs or modes. Unsafe links, ancestry changes, or insufficient access fail closed.

WindShare never overwrites or replaces an existing object, follows a link, or merges with an unrelated top-level result. A new operation chooses a stable short suffix after a name collision. Retrying the same active operation reuses its reserved name. A later collision inside that result blocks only the affected item or subtree and does not trigger another rename.

## Resume modes

A destination is `resumable` only when it proves safe no-replace publication, operation recovery, verified-range recovery, and crash cleanup. Repeating the same `get` continues the one matching active operation, keeps its result name and completed files, and retries only unfinished items after a permission, collision, or session failure.

A destination that proves safe publication and crash cleanup, but not full recovery, runs `live-only` after one warning. Completed files remain after interruption, but the next `get` starts a new operation with a new name and downloads the selection again. WindShare rejects the destination before requesting content if safe publication or crash cleanup cannot be proved.

## Inspect and discard destination state

```text
windshare resume list -o <directory>
windshare resume discard -o <directory> --item <N>
```

Resume state belongs to the selected destination and does not expire automatically. WindShare does not maintain a global download history, scan public output files, or claim completed results. `resume list` pages that destination's operation records and reports:

| Result | Meaning |
|---|---|
| `incomplete` | The active operation has no usable partial file, but can retry with its frozen selection and name. |
| `resumable` | The active operation has verified partial ranges that can continue. |
| `cleanup-pending` | Published results stay in place, but verified WindShare-owned cleanup still needs work. |
| `operation-needs-attention` | Root, registry, lease, or operation ownership is uncertain, so the whole operation stops. |
| `item-blocked` | One file or subtree is locally ambiguous. Independent siblings may continue and the operation remains active. |

`resume discard` refreshes the numbered list and requires exact confirmation on an interactive terminal. It removes only unfinished WindShare-owned state whose operation ownership and native identity still match. It never removes a published file or an unknown object; uncertainty is preserved for attention instead of guessed away.

Interactive progress is refreshed on stderr. Redirected stderr receives no periodic progress, and stdout is never used for progress.
