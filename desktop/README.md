# WindShare desktop UI

Native GPUI UI preview with independent **Share** and **Receive** screens.
All files, links and transfers are samples; no network, engine or filesystem
transfer integration is included. Language preferences are saved locally; preview
links use the reserved `windshare.example` domain.

## Run

From the repository root, with stable Rust and native GPUI prerequisites:

```sh
make desktop
# Compact window (960 × 760):
cargo run --manifest-path desktop/Cargo.toml --locked -- --compact
```

Windows requires the MSVC target, Visual Studio C++ build tools and a DirectX 11
GPU. Windows is the locally verified platform. Dev builds optimize GPUI's core and
Windows renderer for high-refresh animation while keeping application crates unoptimized.

- Light and dark palettes follow the system appearance, including changes while open.
- Share: the result card pairs copying with a local QR code; narrow windows stack them.
  Long links use middle elision and expand in place. QR size follows payload density;
  click to enlarge. Share details reuse the same controls, with inline copy feedback.
  The 1350 ms flight and 720 ms landing fade into the card. Pending links keep
  an indeterminate activity strip until the link fades in over 180 ms; completed
  results stop animation. Copy stays disabled until a result exists. Reduced
  motion skips the introduction and keeps the waiting indicator static.
- Preview latency defaults to 1900 ms. Pass `--share-delay-ms=200` or
  `--share-delay-ms=5000` after `cargo run ... --` to inspect fast and slow results.
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
- My shares: search, filter, grid/list, pause and copy.
- Preferences: language, notifications, transfer details and reduced motion.
  English and 简体中文 switch immediately without resetting inputs or tasks. The
  default follows the system locale at startup, with English for unsupported locales.
  The language choice is saved in the OS config directory at `WindShare/desktop/language`.
- Task panels keep a stable height; compact layouts retain labeled navigation.
  Body text is 16 px, with contrasting text on themed, softly tinted surfaces.
- `Ctrl+N`: new share; `Ctrl+K`: search; `Esc`: dismiss dialog.
  Controls support Tab and Enter/Space.

For frame profiling, run `cargo run --manifest-path desktop/Cargo.toml --features motion-profile`.
The CSV in the OS temporary directory (`windshare-motion-<pid>.csv`) records CPU
layout/prepaint/paint durations, presentation callback intervals and share phases.
Override its path with `WINDSHARE_MOTION_PROFILE`. This is not GPU completion timing;
compare the same build profile, window size and display refresh rate.

## Structure

- `crates/windshare-ui`: owned tokens, icons, buttons, native IME input, badges,
  cards, progress and toggles. No third-party component library.
- `crates/windshare-desktop`: presentation state, timed preview flows, screens
  and motion. Operation identities prevent canceled timers from replacing new state.
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
