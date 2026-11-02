use gpui::{Hsla, IntoElement, Path, Pixels, Point, Size, canvas, point, prelude::*};
use std::{f32::consts::FRAC_PI_2, sync::LazyLock};

const CORNER_SEGMENTS: u16 = 8;
static QUARTER_ARC: LazyLock<Vec<Point<f32>>> = LazyLock::new(|| {
    (0..=CORNER_SEGMENTS)
        .map(|step| {
            let angle = FRAC_PI_2 * f32::from(step) / f32::from(CORNER_SEGMENTS);
            let (sin, cos) = angle.sin_cos();
            point(cos, sin)
        })
        .collect()
});

/// A decorative rounded surface whose geometry retains fractional animation coordinates.
pub fn rounded_surface(
    size: Size<Pixels>,
    radius: Pixels,
    color: Hsla,
) -> impl IntoElement + Styled {
    canvas(
        |_, _, _| (),
        move |bounds, (), window, _| {
            let left = bounds.origin.x;
            let top = bounds.origin.y;
            let right = left + size.width;
            let bottom = top + size.height;
            let radius = radius.min(size.width / 2.).min(size.height / 2.);
            // Quad bounds are pixel-snapped, while analytic curve triangles are not
            // rendered consistently across GPUI backends. A convex mesh preserves both
            // fractional edges and round corners without per-frame tessellation.
            let mut path = Path::new(point(left + radius, top));
            for (corner, center) in [
                point(right - radius, top + radius),
                point(right - radius, bottom - radius),
                point(left + radius, bottom - radius),
                point(left + radius, top + radius),
            ]
            .into_iter()
            .enumerate()
            {
                for arc in QUARTER_ARC.iter() {
                    let offset = match corner {
                        0 => point(arc.y, -arc.x),
                        1 => *arc,
                        2 => point(-arc.y, arc.x),
                        _ => point(-arc.x, -arc.y),
                    };
                    path.line_to(center + point(radius * offset.x, radius * offset.y));
                }
            }
            window.paint_path(path, color);
        },
    )
    .absolute()
    .inset_0()
    .size_full()
}
