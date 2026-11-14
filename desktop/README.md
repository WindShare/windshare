# WindShare desktop

Native GPUI sharing through the Go engine and saved CLI relay/frontend settings.
The default app selects real files and folders, creates links, and retains shares
until explicitly stopped or the app closes. Sample flows are available with
`--preview` only.

## Run

From the repository root, with stable Rust and native GPUI prerequisites:

```sh
make desktop
# Compact window (960 × 760):
make desktop DESKTOP_ARGS='--compact'
# Start with an existing selection:
make desktop DESKTOP_ARGS='--live-share "/path/to/file" "/path/to/folder"'
# Sample UI and animation development:
make desktop-preview
```

The build also requires Node.js, Go and a C compiler. Use the Make targets to stage the
native library beside the executable using Cargo's reported artifacts. Windows requires
MinGW-w64 GCC on PATH for the Go library, the Rust MSVC target, Visual Studio C++ build tools and a
DirectX 11 GPU. Windows is the locally verified platform.

Choose files or folders in the native picker, or drop a mixed selection into the
window as one share. Links are immediately available for copying, opening in a
browser, or scanning. My shares retains preparing, active and finished tasks across
navigation and control reconnects; New share does not stop existing shares.
The engine supplies selected-root names, types and known file sizes. Directory totals
remain unknown; selecting a folder does not scan descendants or copy sources.

When configuration is missing, enter a relay URL and, if the relay does not provide
one, a frontend URL in the app. Restricted relays may require browser approval.
The selection resumes automatically after setup. Existing CLI settings are reused;
after changing them externally, choose **Use saved settings**. `WINDSHARE_CONFIG`
overrides the shared configuration path. Setup preserves saved relay choices and
disabled defaults. Cancelling stops admission and revokes unfinished device approval;
committed settings survive, and failed cancellation can be retried.

The built-in network uses provisioned release trust roots, a signed directory and
usable nodes. The production release manifest remains unprovisioned; use saved
relays or `make development-build` with the [development network](../deploy/network/development/README.md) for local use.
Reuse the existing development network's trust identity across source rebuilds.
Temporary network failures expose the owner's retry cooldown. Official account
provisioning is a separate P2 dependency, required only when anonymous admission is refused.

Closing with unfinished shares or setup asks whether to keep running or quit and stop
all shares. Dismissing keeps sharing; confirming joins the engine off the UI thread.
Sharing again after completion creates a new task and link from the retained selection.

`make desktop-package` creates a release archive under `dist/desktop`, including
the executable, Go library, license and instructions. The generated C header stays
in the build output. Packaging checks
`--host-check` from the relocated directory. Keep the library beside the executable.
Logs default to the OS cache directory under `WindShare/logs`: host files rotate
at 4 MiB with eight retained files, independently for the host and desktop.
Logging is asynchronous and bounded; unavailable log storage does not prevent sharing.
`WINDSHARE_TRACE=1` enables detailed engine diagnostics; `RUST_LOG` controls Rust traces.

## Interface and preview

The default app and `--preview` share the original UI, spirit interactions, flight,
landing, result card, link disclosure, enlarged QR, and searchable grid/list.
Native readiness controls link access; a link returned during the introduction is
immediately usable. Reduced motion and inactive-window scheduling also apply to
real shares. Receive, transfer, component and SuperShare samples require `--preview`.

Dev builds optimize GPUI's core and
Windows renderer for high-refresh animation while keeping application crates unoptimized.

- Preferences offers System default, Light and Dark appearance. Changes apply immediately;
  the choice is saved at `WindShare/desktop/appearance` in the OS config directory.
  System default follows appearance changes while the app is open.
- Share: the result card pairs copying with a local QR code; narrow windows stack them.
  Long links use middle elision and expand in place. QR size follows payload density;
  click to enlarge. Share details reuse the same controls, with inline copy feedback.
  The 1350 ms flight and 720 ms landing fade into the card. Pending links keep
  an indeterminate activity strip until the link fades in over 180 ms; completed
  results stop animation. Copy stays disabled until a result exists; an early
  result exposes copy/open controls during the introduction. Reduced motion skips
  the introduction and keeps the waiting indicator static.
- Preview latency defaults to 1900 ms. Pass `--share-delay-ms=200` or
  `--share-delay-ms=5000` via `make desktop-preview DESKTOP_ARGS=...` to inspect fast and slow results.
  Motion uses actual completion events, never the configured delay.
- Animated color SVGs reuse vector meshes, antialiased at their final display size.
  Wind meshes are cached; invisible artwork is omitted. Result content uses
  natural layout independently of the animation coordinates. Inactive share
  windows stop decorative frames. Normal traces report phase and callback timing.
- Floating spirits follow the pointer, spring back and smile on click or Enter/Space.
  Hit targets stay fixed; reduced motion keeps smile feedback without movement.
- SuperShare: click the top-left logo to open the edge-to-edge purchase canvas,
  with monthly/long-term selection, payment methods,
  two-step local activation, and a pearl-gold brand mark. Alipay, OKX Pay and
  Binance Pay use bundled QR codes; crypto supports seven wallets with network labels,
  locally generated QR codes and address copying. Activation lasts only for this preview session.
  Activation leads to sharing; reopening via the logo shows enabled status with a return-to-payment button.
  Redemption uses local fixtures: `SUPER-PREVIEW` succeeds, `OFFLINE-PREVIEW` shows
  the connection error, and other codes show an invalid-code result. No payments
  or official redemption requests are sent.
- Receive: paste a link or use the sample, inspect files, preview receiving.
- My shares: search, filter, grid/list, details and copy. Real shares stop permanently;
  preview fixtures retain simulated pause/resume.
- Preferences: appearance, language, notifications, transfer details and reduced motion.
  English and 简体中文 switch immediately without resetting inputs or tasks. The
  default follows the system locale at startup, with English for unsupported locales.
  The language choice is saved in the OS config directory at `WindShare/desktop/language`.
- Task panels keep a stable height; compact layouts retain labeled navigation.
  Body text is 16 px, with contrasting text on themed, softly tinted surfaces.
- `Ctrl+N`: new share; `Ctrl+K`: search; `Esc`: dismiss dialog.
  Controls support Tab and Enter/Space.

For frame profiling, run `make desktop-preview DESKTOP_CARGO_ARGS='--features motion-profile'`.
The CSV in the OS temporary directory (`windshare-motion-<pid>.csv`) records CPU
layout/prepaint/paint durations, presentation callback intervals and share phases.
Override its path with `WINDSHARE_MOTION_PROFILE`. This is not GPU completion timing;
compare the same build profile, window size and display refresh rate.

## Structure

- `crates/windshare-ui`: owned tokens, icons, buttons, native IME input, badges,
  cards, progress and toggles. No third-party component library.
- `crates/windshare-host`: native ABI ownership, reconnecting control client and task snapshots.
  Cargo builds `cmd/windshare-host`; `host/desktop` owns the engine and local endpoint.
  Committed engine changes push coalesced full snapshots; GPUI wakes on model changes.
  Reconnects reconcile the current session; a five-second fallback runs only while work is active.
- `crates/windshare-desktop/src/live`: native selection, engine snapshot projection and
  joined app lifetime. `Desktop` and the existing view/motion modules serve both real
  and preview sessions; there is no separate production UI.
- Brand artwork adapts `web/public/brand/windshare.svg` into mint and smile-eye
  variants with transparent wind lines and drifting paper illustrations; the
  original stays unchanged.

GPUI is pinned to `gpui-pre = 0.3.11`, matching the
[GPUI Kit snapshot](https://github.com/longbridge/gpui-kit/blob/main/Cargo.toml).
Windows Chinese typography uses Microsoft YaHei UI; Manrope is bundled for the
wordmark and Latin display text under its [OFL license](crates/windshare-ui/assets/fonts/OFL.txt).

Validate with `make desktop-check` and `make sloc`. Visual and interaction checks
run in the native window; no fragile UI structure or copy tests are added.

## Design

Modern minimalist UI with a soft flat design and a low-saturation pastel color palette. Designed in a lightweight productivity tool style, featuring generous whitespace for an airy, breathable layout. Visually light and comfortable, maintaining a clean and restrained overall aesthetic.
