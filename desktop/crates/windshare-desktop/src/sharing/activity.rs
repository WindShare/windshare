use crate::share_timing::{LinkPresentation, lerp, smooth};
use gpui::{Div, Path, Point, canvas, div, point, prelude::*, px};
use std::{f32::consts::PI, sync::LazyLock};
use windshare_ui::{Theme, Tone};

const TRACK_WIDTH: f32 = 220.;
const TRACK_HEIGHT: f32 = 3.;
const SEGMENT_FRACTION: f32 = 0.3;
const CAP_STEPS: u16 = 10;
static CAPS: LazyLock<Vec<Point<f32>>> = LazyLock::new(|| {
    (0..=CAP_STEPS * 2)
        .map(|step| {
            let angle = PI * f32::from(step) / f32::from(CAP_STEPS);
            let (sin, cos) = angle.sin_cos();
            point(sin, -cos)
        })
        .collect()
});

pub(super) fn link_activity(theme: Theme, presentation: LinkPresentation) -> Div {
    div()
        .relative()
        .w(px(TRACK_WIDTH))
        .max_w_full()
        .h(px(TRACK_HEIGHT))
        .rounded_full()
        .overflow_hidden()
        .bg(Tone::Mint.surface(theme))
        .opacity(presentation.waiting_visibility())
        .child(
            canvas(
                |_, _, _| (),
                move |bounds, (), window, _| {
                    let width = f32::from(bounds.size.width);
                    let segment = width * SEGMENT_FRACTION;
                    let radius = TRACK_HEIGHT / 2.;
                    let offset = lerp(-segment, width, smooth(presentation.activity_phase()));
                    let origin = bounds.origin + point(px(offset), px(radius));
                    // Translate the mesh after layout so slow motion retains subpixel
                    // positions at high refresh rates. The track and hit regions stay still.
                    let mut path = Path::new(origin + point(px(segment - radius), px(-radius)));
                    for (index, cap) in CAPS.iter().enumerate() {
                        let center = if index <= usize::from(CAP_STEPS) {
                            segment - radius
                        } else {
                            radius
                        };
                        path.line_to(
                            origin + point(px(radius.mul_add(cap.x, center)), px(radius * cap.y)),
                        );
                    }
                    window.paint_path(path, theme.accent_text().opacity(0.8));
                },
            )
            .size_full(),
        )
}
