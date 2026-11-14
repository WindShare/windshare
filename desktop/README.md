# WindShare desktop

> This page outlines the desktop application's shape and vocabulary. The code under [`crates/`](crates/) and [`host/desktop/`](../host/desktop/) is the source of truth for specifics.

Native GPUI client (`windshare`) that shares local files and folders through the Go engine and saved CLI settings (`WINDSHARE_CONFIG`). Default runs and `--preview` share one UI: default runs enable `Share files`, `My shares`, and `Preferences`, while `--preview` also unlocks sample `Receive files`, `Transfers`, `Components`, and `SuperShare` views backed by local fixtures.

## Build and run

From the repository root, with stable Rust, Go, Node.js, a C compiler, and GPUI prerequisites (Windows is the verified platform: MSVC target, Visual Studio C++ build tools, MinGW-w64 GCC on `PATH`, DirectX 11 GPU):

```sh
make desktop
make desktop DESKTOP_ARGS='--compact'
make desktop DESKTOP_ARGS='--live-share <path...>'
make desktop-preview
make desktop-preview DESKTOP_ARGS='--share-delay-ms=<ms>'
```

Make targets build [`cmd/windshare-host`](../cmd/windshare-host/) (`c-shared`) and stage the Go library beside the executable ([`build.mjs`](build.mjs)); the generated C header stays in the build output. Window dimensions and preview defaults live in [`main.rs`](crates/windshare-desktop/src/main.rs).

| Command | Behavior |
|---|---|
| `make desktop-package` | Packages the executable, adjacent Go library, license, and instructions under `dist/desktop`, then checks `--host-check` from the staged directory ([`package.mjs`](package.mjs)). |
| `make development-build` | Builds CLI, desktop, and Web against the [development network](../deploy/network/development/README.md). |
| `make desktop-preview DESKTOP_CARGO_ARGS='--features motion-profile'` | Writes CPU `layout`/`prepaint`/`paint`, presentation-interval, and share-phase CSV samples (not GPU completion timing) to `windshare-motion-<pid>.csv` in the OS temp dir or `WINDSHARE_MOTION_PROFILE` ([`profiling/mod.rs`](crates/windshare-desktop/src/profiling/mod.rs)). |
| `make desktop-check`, `make sloc` | Runs Rust formatting, Clippy, desktop/host ABI tests, build checks, and source-size limits. |

## Behavior

- **Sharing**: Native pickers and drag-and-drop submit mixed files and folders as one share without scanning descendants or copying sources; the UI displays selected-root names, types, and known file sizes while directory totals remain unknown. Links become usable on first relay admission with copy and QR actions. `My shares` provides search, filter, and grid/list views, retaining preparing, live, and finished tasks across navigation and control reconnects; `Stop sharing` ends a share permanently, and `Share again` starts a new task from the retained selection.
- **Configuration**: Reuses `WINDSHARE_CONFIG` and built-in network defaults, preserving existing relay choices and disabled defaults. In-app setup (`Self-hosted relay` in Preferences or from a blocked share) saves relay/frontend URLs, handles optional browser device approval, and resumes blocked selections. From a blocked share or its setup dialog, `Use saved settings` reloads external edits; temporary network failures expose the engine's retry cooldown, and cancelling setup revokes unfinished approval while keeping committed settings.
- **Preferences and navigation**: Persists appearance (`system`, `light`, `dark`) and language (`system`, `en`, `zh-CN`) under `WindShare/desktop/` in the OS config directory; `Preferences` also toggles notifications, reduced motion, and (in `--preview`) transfer details. `Ctrl+N` starts a new share, `Ctrl+K` searches shares, `Esc` closes overlays, and controls support `Tab` and `Enter`/`Space`.
- **Preview (`--preview`)**: `--share-delay-ms=<ms>` adjusts simulated share latency; clicking the top-left logo opens `SuperShare` with local redemption fixtures (`SUPER-PREVIEW` succeeds, `OFFLINE-PREVIEW` shows a connection error, other codes fail; see [`supershare/state.rs`](crates/windshare-desktop/src/supershare/state.rs)) and no network or payment requests.
- **Logs and shutdown**: Rotating desktop and host logs write asynchronously via bounded queues under `WindShare/logs` in the OS cache directory ([`logging/`](crates/windshare-desktop/src/logging/), [`logstore/`](../host/desktop/logstore/)); unavailable log storage does not prevent sharing. `WINDSHARE_TRACE=1` enables host diagnostics and `RUST_LOG` controls Rust traces. Closing with unfinished shares or setup prompts to keep running or stop shares and quit, joining the engine off the UI thread.

## Code

| Crate / package | Role |
|---|---|
| [`crates/windshare-desktop`](crates/windshare-desktop/) | GPUI shell, pages, `live/` engine bridge, animations, localization, preferences, logging, and profiling (`motion-profile`) |
| [`crates/windshare-host`](crates/windshare-host/) | C ABI loader (loads the sibling Go library), reconnecting loopback WebSocket client, and coalesced task model waking GPUI on changes |
| [`crates/windshare-ui`](crates/windshare-ui/) | Owned tokens, primitives, native IME input, QR rendering, and Manrope font ([OFL](crates/windshare-ui/assets/fonts/OFL.txt)); Windows Chinese text uses Microsoft YaHei UI |
| [`cmd/windshare-host`](../cmd/windshare-host/), [`host/desktop`](../host/desktop/) | Go `c-shared` lifecycle ABI, engine ownership, and control server pushing coalesced snapshots (with active-work refresh fallback) |

[`Cargo.toml`](Cargo.toml) pins GPUI dependencies and optimizes GPUI crates in `dev` builds while keeping application crates unoptimized.

## Design

Modern minimalist UI with a soft flat design and a low-saturation pastel color palette. Designed in a lightweight productivity tool style, featuring generous whitespace for an airy, breathable layout. Visually light and comfortable, maintaining a clean and restrained overall aesthetic.
