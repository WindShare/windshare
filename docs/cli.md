# CLI

> WindShare is in rapid development, and the CLI may change along with the implementation.
> This page only outlines the CLI's shape and vocabulary. The code under [`cmd/wind/`](../cmd/wind/)
> and [`engine/`](../engine/) is the source of truth for specifics.

`wind` shares local files and directories, downloads capability links, enrolls or imports relay settings,
and inspects or discards unfinished downloads. Capability links and split keys are written to stdout;
progress, prompts, and diagnostics go to stderr. See [installation](installation.md) and [TURN services](turn.md).

## Maintenance

- Keep only what is needed to understand the CLI's shape and vocabulary; added detail is a regression, not an improvement.
- State current facts only: no recommendations, constraint rules, or plans.
- Leave constants, limits, exit codes, timing, and platform edge cases to the code; link to it instead of copying.
- When the shape or vocabulary changes, replace or delete the affected lines, and keep the page about its current size.

## Share

```text
wind share <path...> [--relay <url>]... [--front-url <url>] [--block-size <bytes>] [--split-key] [--browser-trace]
```

- Commits the selected roots, registers the descriptor with active relays, prints the capability link once a relay admits sessions, and serves catalog pages and file blocks on demand until stopped.
- Default stdout output is `Link: <link>`. `--split-key` prints `Bare link: <link-without-key>` and `Key: <key>`.
- `--front-url` and repeatable `--relay` override the saved sharing defaults for that run; `--browser-trace` sets the link query flag that enables browser receiver diagnostics.

### Sharing defaults

```text
wind relay add <url> [--name <device>] [--no-browser]
wind relay onboard <url> [--name <device>] [--no-browser]
wind relay cancel <url>
wind config import <file|-> [--front-url <url>] [-v|--verbose]
```

- `relay add` discovers relay access, runs browser device approval when required, and saves the profile while preserving active-relay and frontend selections.
- `relay onboard` enrolls a private relay, saves its advertised frontend and managed TURN reference, and selects only that relay in `active_relays`.
- `relay cancel` abandons an unfinished enrollment and revokes its issued credential.
- `config import` validates and replaces the saved sharing configuration from a JSON file or stdin (`-`).
- Settings live in `windshare/config.json` under the user configuration directory (or `WINDSHARE_CONFIG`). Rebuildable runtime state lives alongside it in `config.json.state/setup/` (pending enrollment) and `config.json.state/turn/` (managed TURN discovery cache).

```json
{
  "front_url": "https://your-site.example",
  "profiles": [{ "url": "https://your-site.example", "access_key": "your-device-key" }],
  "active_relays": ["https://your-site.example"]
}
```

Optional `managed_turn_policies` and manual `turn` sections live in the same file; see [Cloudflare deployment](deploy/cloudflare-relay.md) and [TURN services](turn.md).

## Download

```text
wind get <link> [-o <directory>] [--only <path>]... [--redownload] [--key <key>] [--connectivity auto|relay-only|p2p-only] [--wait-timeout <duration>]
```

- Joins the share, resolves the selected catalog paths (`--only`, repeatable), and writes the result under `-o` (default `.`). A bare link uses `--key` or prompts for the key on stdin.
- Terminal result states are `success`, `partial`, `paused`, and `failed`.

| Selection | Saved as |
|---|---|
| One file | `<filename>` |
| One directory | `<directory>/...` |
| Part of one directory | `<directory>-selection/...` |
| Multiple roots | `windshare/...` |

| `--connectivity` | Behavior |
|---|---|
| `auto` | Schedules content across available relay, direct WebRTC, and TURN lanes. |
| `relay-only` | Transfers content only through application relays; creates no ICE connections. |
| `p2p-only` | Uses relays for signaling and transfers content only over direct WebRTC paths. |

## Resume

```text
wind resume retry [-o <directory>] [--operation <id>] [--only <path>]... <link>
wind resume list -o <directory>
wind resume discard -o <directory> --item <N>
```

- `get` automatically reopens matching progress. `resume retry` searches `-o` (default `.`) for the same link and selection, optionally choosing `--operation`, and refuses to start a new download when no operation matches.
- Recoverable interactive failures offer retry, discard, or exit. Retry keeps the operation and verified ranges. Invalid selections require correcting `--only`; completed files and saved partials remain intact. Ctrl+C exits; unattended runs report retained progress and the appropriate next step without prompting.
- `get --redownload` starts a separate task in a collision-safe destination. The original task and its automatic continuation remain intact; `resume retry --operation <id>` selects either task.
- Delivered files stay completed within a task even after being moved or deleted. Saved partials resume only when their source revision matches. Checkpoints survive normal exit; a crash may lose the latest unsaved ranges. See [native output](native-output.md).
- `resume list` shows recovery state and blocked items. Capability loss keeps existing operations visible as `needs-attention` with `recovery-unavailable`.
- `resume discard` re-lists the inventory and confirms the selected operation before removing proven-owned unfinished state. Published and unrecognized files remain.

## Diagnostics

- `-v`, `--verbose`: writes milestone and connection diagnostics to stderr (`share`, `get`, `config import`).
- `--trace <file>`: writes one run's structured NDJSON trace to a new file (`share`, `get`).
- `--trace-dir <directory>`: writes a run-specific NDJSON trace file in `<directory>` (`share`, `get`).

Traces record lifecycle, connectivity, catalog, content, and provider diagnostics while redacting links, keys, credentials, and file content.

## Code

| Topic | Packages |
|---|---|
| Command dispatch, flags, exit codes | [`cmd/wind`](../cmd/wind/), [`cmd/wind/internal/cli`](../cmd/wind/internal/cli/) |
| Configuration import and storage | [`cmd/wind/internal/configcommand`](../cmd/wind/internal/configcommand/), [`engine/relayconfig`](../engine/relayconfig/) |
| Relay enrollment | [`engine/relaysetup`](../engine/relaysetup/) |
| Resume inventory and discard | [`cmd/wind/internal/resumecommand`](../cmd/wind/internal/resumecommand/), [`engine/recovery`](../engine/recovery/), [`adapters/filesystem`](../adapters/filesystem/) |
| Terminal events and rendering | [`cmd/wind/internal/clievent`](../cmd/wind/internal/clievent/), [`cmd/wind/internal/commandprojection`](../cmd/wind/internal/commandprojection/), [`cmd/wind/internal/humanoutput`](../cmd/wind/internal/humanoutput/), [`cmd/wind/internal/terminalcanvas`](../cmd/wind/internal/terminalcanvas/) |
| NDJSON run trace | [`cmd/wind/internal/runtrace`](../cmd/wind/internal/runtrace/), [`core/testvectors`](../core/testvectors/) |
| Share, receive, and TURN runtime | [`engine`](../engine/), [`osfs`](../osfs/) |
