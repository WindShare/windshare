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

- Share: at least 1.65 seconds of flight followed by a 350 ms landing. Early
  results wait for the entrance; longer waits keep a gentle orbit until ready.
  Copying works during landing. Reduced motion skips both decorative delays.
- Preview latency defaults to 1350 ms. Pass `--share-delay-ms=200` or
  `--share-delay-ms=5000` after `cargo run ... --` to inspect fast and slow results.
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
