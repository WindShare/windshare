# WindShare desktop UI

Native GPUI UI preview with independent **Share** and **Receive** screens.
All files, links and transfers are samples; no network, engine or filesystem
integration is included. Preview links use the reserved `windshare.example` domain.

## Run

From the repository root, with stable Rust and native GPUI prerequisites:

```sh
make desktop
# Compact window (960 × 760):
cargo run --manifest-path desktop/Cargo.toml --locked -- --compact
```

Windows requires the MSVC target, Visual Studio C++ build tools and a DirectX 11
GPU. Windows is the locally verified platform.

- Share: the original 1350 ms flight and 720 ms landing play continuously.
  Late results keep the settled scene breathing, then crossfade the link in
  over 180 ms. Copy stays disabled until a result exists. Reduced motion skips
  decorative delays.
- Preview latency defaults to 1900 ms. Pass `--share-delay-ms=200` or
  `--share-delay-ms=5000` after `cargo run ... --` to inspect fast and slow results.
  Motion uses actual completion events, never the configured delay.
- Animated color SVGs reuse cached paint masks and fractional GPU transforms;
  motion never goes through pixel-snapped layout or image bounds. Inactive share
  windows stop decorative frames. Traces report phases and render callback cadence,
  not GPU presentation FPS.
- Receive: paste a link or use the sample, inspect files, preview receiving.
- My shares: search, filter, grid/list, pause and copy.
- Preferences: notifications, transfer details and reduced motion.
- Task panels keep a stable height; compact layouts retain labeled navigation.
  Body text is 16 px, with contrasting text on pale macaron surfaces.
- `Ctrl+N`: new share; `Ctrl+K`: search; `Esc`: dismiss dialog.
  Controls support Tab and Enter/Space.

## Structure

- `crates/windshare-ui`: owned tokens, icons, buttons, native IME input, badges,
  cards, progress and toggles. No third-party component library.
- `crates/windshare-desktop`: presentation state, timed preview flows, screens
  and motion. Operation identities prevent canceled timers from replacing new state.
- Brand artwork adapts `web/public/brand/windshare.svg` into mint and smile-eye
  variants with transparent wind lines and drifting paper illustrations; the
  original stays unchanged.

GPUI is pinned to `gpui-pre = 0.3.10`, matching the
[GPUI Kit snapshot](https://github.com/longbridge/gpui-kit/blob/main/Cargo.toml).
Windows Chinese typography uses Microsoft YaHei UI; Manrope is bundled for the
wordmark and Latin display text under its [OFL license](crates/windshare-ui/assets/fonts/OFL.txt).

Validate with `make desktop-check` and `make sloc`. Visual and interaction checks
run in the native window; no fragile UI structure or copy tests are added.
