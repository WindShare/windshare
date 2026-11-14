---
name: gpui-performance
description: Diagnose and optimize WindShare Rust/GPUI desktop animation stutter, frame cost, and artwork rendering. Use when investigating uneven motion or changing animated GPUI rendering.
---

# GPUI performance

Start with `desktop/Cargo.lock` and the pinned GPUI source. Pixel snapping, atlas behavior, and invalidation are version/backend dependent.

## Measure the symptom

Run from the repository root:

```sh
cargo run --manifest-path desktop/Cargo.toml --locked --features motion-profile
```

The [profiler](../../../desktop/crates/windshare-desktop/src/profiling/mod.rs) writes `windshare-motion-<pid>.csv` in the OS temporary directory; override with `WINDSHARE_MOTION_PROFILE`.

Compare the same build profile, window size, DPI, refresh rate, and animation phase. Separate startup and idle gaps from steady motion. Report sample count and CPU draw/layout/prepaint/paint percentiles. Presentation callback intervals are scheduling evidence, not GPU completion or display FPS. Regular callbacks with visibly stepped movement suggest coordinate quantization, not insufficient frame rate.

## Apply the relevant fix

- **Move artwork after layout.** GPUI layout positions, element offsets, and quads can snap to device pixels. Keep layout stable; move mesh vertices or sprite centers inside canvas painting. Update transformed bounds for culling and let `paint_path` apply DPI once. Keep text and hit targets settled; opacity can carry their reveal. See [VectorSprite](../../../desktop/crates/windshare-ui/src/artwork.rs), [rounded_surface](../../../desktop/crates/windshare-ui/src/surface.rs), and [SuperShare emblem](../../../desktop/crates/windshare-desktop/src/supershare/emblem.rs).

- **Cache geometry, vary presentation.** Parse/preload bundled SVGs before animation. Retain meshes with `LazyLock`/`Arc` or stable `with_element_state` IDs; animate transforms, opacity, or clipping. Cache keys include geometry dependencies such as bounds, and theme when colors are cached; exclude animation phase. Follow frame invalidation into static ancestors/backgrounds: a small moving logo can otherwise rebuild window-sized curves. See [wind mesh](../../../desktop/crates/windshare-desktop/src/motion/wind.rs) and [backdrop](../../../desktop/crates/windshare-desktop/src/supershare/backdrop.rs).

- **Preserve final-size edge quality.** The earlier cached SVG-mask approach was superseded: shrinking an atlas mask without minification levels degraded edges. Reuse tessellated vector meshes and antialias after transformation. Choose curve tolerance for the largest device-space size. Preserve SVG paint order, stroke outlines, and fill rules. For expression transitions, paint shared layers once at full opacity and fade only differing layers; fading two whole sprites changes body coverage.

- **Give motion one clock and owner.** Sample elapsed time instead of counting frames or restarting on pointer events. Preserve the animated entity across presentation states. Keep interaction spring pose and ambient float offset separate, with stable hit regions. Separate result readiness from choreography: usable content follows readiness; pending activity continues after the finite introduction. See [spirit](../../../desktop/crates/windshare-desktop/src/motion/spirit/mod.rs) and [share timing](../../../desktop/crates/windshare-desktop/src/share_timing.rs).

- **Request frames only while needed.** Use GPUI frame scheduling while motion is visible, active, enabled, and unfinished. Stop decorative demand when hidden, inactive, reduced-motion, or complete; resume without integrating a long inactive time step. Bound timing samples and log operation/phase transitions. Keep profiling file writes off the UI thread.

- **Check development code generation.** GPUI transforms/submits vertices on the CPU. The [workspace](../../../desktop/Cargo.toml) uses `opt-level = 2` for `gpui-pre` and `gpui-pre-windows` in dev builds while leaving application crates unoptimized. Verify these overrides before attributing dev-only cost to the animation design.

## Verify

After code changes, run `make desktop-check` and `make sloc`. Compare native-window captures and timings; exercise resize/DPI, theme, navigation, reduced motion, and inactive/resumed states as relevant. UI structure/copy snapshot tests do not establish smoothness. Distinguish measured CPU savings from visual continuity improvements.

Historical basis: `d314a169` (subpixel motion), `f462b947` (cached meshes, surfaces, profiling), `f5b983eb` (continuous timing/cadence), `3c58a0c3` (stable transitions), `5067079c` (final-size antialiasing), `f7a25b42` (dev optimization), and `89bcc7d1` (readiness and waiting activity). Prefer current implementations when older commits differ.
