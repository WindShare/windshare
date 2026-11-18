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
| `make desktop-package` | Builds a signed Windows identity package, Explorer DLL, executable, Go library, icons and current-user installer under `dist/desktop`; verifies the relocated host ([`package.mjs`](package.mjs)). |
| `make development-build` | Builds CLI, desktop, and Web against the [development network](../deploy/network/development/README.md). |
| `make desktop-preview DESKTOP_CARGO_ARGS='--features motion-profile'` | Writes CPU `layout`/`prepaint`/`paint`, presentation-interval, and share-phase CSV samples (not GPU completion timing) to `windshare-motion-<pid>.csv` in the OS temp dir or `WINDSHARE_MOTION_PROFILE` ([`profiling/mod.rs`](crates/windshare-desktop/src/profiling/mod.rs)). |
| `make desktop-check`, `make sloc` | Runs Rust formatting, Clippy, desktop/host ABI tests, build checks, and source-size limits. |

## Windows installation

Windows 11 packaging requires the Windows SDK tools (`mt`, `makepri`, `makeappx`, `signtool`). Ordinary builds need no signing or registration.

- Development: `make desktop-package` signs with a temporary development certificate. On a machine already in Developer Mode, run `powershell.exe -File install.ps1 -DevelopmentRegistration` from the extracted archive.
- Release: set `WINDSHARE_INSTALL_CHANNEL=release`, `WINDSHARE_PACKAGE_PUBLISHER`, `WINDSHARE_PACKAGE_VERSION` (four components), and `WINDSHARE_SIGNING_THUMBPRINT` before packaging. The signing certificate must match the publisher and be trusted by Windows. Run `powershell.exe -File install.ps1`.
- Remove: run the installed `uninstall.ps1`. Updates and removal wait for active app/shell ownership to end; `-WaitTimeoutSeconds` bounds the wait. Saved configuration remains.

Installation keeps binaries, visual assets and `resources.pri` together in immutable version directories under `%LOCALAPPDATA%/Programs/WindShare/<channel>`. Development and release have distinct package, COM and instance identities. Official network provisioning remains a separate [release dependency](../deploy/network/README.md).

## Behavior

- **Windows entry points**: Explorer's **Share with WindShare** submits the entire file/folder selection to one application instance. Windows Share accepts independently stored local items in Desktop, Documents, Downloads, Pictures, Music or Videos on fixed disks; temporary, cloud/reparse and unknown sources require saving locally first. A new share restores the window, dismisses pending pickers/quit prompts, and keeps previous tasks running.
- **Sharing**: Native pickers and drag-and-drop submit mixed files and folders as one share without scanning descendants or copying sources; the UI displays selected-root names, types, and known file sizes while directory totals remain unknown. The home spirit floats and responds to the pointer and clicks. Shares use the Whirl entrance by default; Preferences also offers the original Paper entrance. Each run keeps its selected entrance while waiting for relay admission; the system's reduced-motion preference skips decorative transitions. `My shares` provides search, filter, and grid/list views, retaining preparing, live, and finished tasks across navigation and control reconnects; `Stop sharing` ends a share permanently, and `Share again` starts a new task from the retained selection.
- **Sharing network**: Preferences shows the current relay selection and official relay account. Automatic selection supplements enabled custom relays with the built-in network. Choose **Custom relays only** to limit sharing to your selection. From **Choose relays**, add a relay or change the receiver website. Account-enabled services offer **Sign in to this service**. Match the device code in your browser to authorize this relay. Node owners can still pair through the management console. A blocked share can retry with saved settings.
- **Official relay account**: First use sets up the account on this device. Set a username and password to use its allowance on another device, or sign in to an existing account for new shares.
- **Preferences and navigation**: Persists appearance (`system`, `light`, `dark`), language (`system`, `en`, `zh-CN`), and share animation (`whirl`, `paper`) under `WindShare/desktop/` in the OS config directory; `Preferences` also toggles notifications and (in `--preview`) transfer details. `Ctrl+N` starts a new share, `Ctrl+K` searches shares, `Esc` closes overlays, and controls support `Tab` and `Enter`/`Space`.
- **Preview (`--preview`)**: `--share-delay-ms=<ms>` adjusts simulated share latency; clicking the top-left logo opens `SuperShare` with local redemption fixtures (`SUPER-PREVIEW` succeeds, `OFFLINE-PREVIEW` shows a connection error, other codes fail; see [`supershare/state.rs`](crates/windshare-desktop/src/supershare/state.rs)) and no network or payment requests.
- **Logs and shutdown**: Rotating desktop and host logs write asynchronously via bounded queues under `WindShare/logs` in the OS cache directory ([`logging/`](crates/windshare-desktop/src/logging/), [`logstore/`](../host/desktop/logstore/)); unavailable log storage does not prevent sharing. `WINDSHARE_TRACE=1` enables host diagnostics and `RUST_LOG` controls Rust traces. Closing with unfinished shares or setup prompts to keep running or stop shares and quit, joining the engine off the UI thread.

## Code

| Crate / package | Role |
|---|---|
| [`crates/windshare-desktop`](crates/windshare-desktop/) | GPUI shell, pages, `live/` engine bridge, animations, localization, preferences, logging, and profiling (`motion-profile`) |
| [`crates/windshare-host`](crates/windshare-host/) | C ABI loader (loads the sibling Go library), reconnecting loopback WebSocket client, and coalesced task model waking GPUI on changes |
| [`crates/windshare-activation`](crates/windshare-activation/) | Native share intents, session-scoped instance election/IPC, cancellable picker and Windows Share acquisition |
| [`crates/windshare-shell`](crates/windshare-shell/) | Explorer COM command capturing complete selections and handing off to the dispatcher |
| [`crates/windshare-ui`](crates/windshare-ui/) | Owned tokens, primitives, native IME input, QR rendering, and Manrope font ([OFL](crates/windshare-ui/assets/fonts/OFL.txt)); Windows Chinese text uses Microsoft YaHei UI |
| [`cmd/windshare-host`](../cmd/windshare-host/), [`host/desktop`](../host/desktop/) | Go `c-shared` lifecycle ABI, engine ownership, and control server pushing coalesced snapshots (with active-work refresh fallback) |

[`Cargo.toml`](Cargo.toml) pins GPUI dependencies and optimizes GPUI crates in `dev` builds while keeping application crates unoptimized.

## Design

Modern minimalist UI with a soft flat design and a low-saturation pastel color palette. Designed in a lightweight productivity tool style, featuring generous whitespace for an airy, breathable layout. Visually light and comfortable, maintaining a clean and restrained overall aesthetic.
