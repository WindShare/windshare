use gpui::{
    Bounds, ContentMask, Hsla, IntoElement, Path, PathBuilder, Pixels, canvas, point, prelude::*,
    px,
};
use std::{
    f32::consts::{PI, TAU},
    sync::Arc,
};
use windshare_ui::{Theme, Tone};

const WIND_END: f32 = 1.75;
const WIND_LENGTH: f32 = 0.88;
const WIND_SEGMENTS: u16 = 96;
const HORIZONTAL_TRAVEL: f32 = 1.18;
const HORIZONTAL_ORIGIN: f32 = -0.09;

struct WindMesh {
    bounds: Bounds<Pixels>,
    theme: Theme,
    strokes: Vec<(Path<Pixels>, Hsla)>,
}

pub fn wind_field(head: f32, strength: f32) -> impl IntoElement {
    canvas(
        |bounds, window, cx| {
            let theme = Theme::current(window, cx);
            window.with_global_id("share-wind-mesh".into(), |id, window| {
                window.with_element_state(id, |cached: Option<Arc<WindMesh>>, _| {
                    let mesh = cached
                        .filter(|mesh| mesh.bounds == bounds && mesh.theme == theme)
                        .unwrap_or_else(|| Arc::new(WindMesh::new(bounds, theme)));
                    (mesh.clone(), mesh)
                })
            })
        },
        move |bounds, mesh, window, _| {
            if strength <= 0. {
                return;
            }
            // The curve is fixed in scene coordinates. Only its visible interval moves,
            // so neither trigonometry nor stroke tessellation belongs in the frame loop.
            let x = |t: f32| {
                bounds.left() + bounds.size.width * t.mul_add(HORIZONTAL_TRAVEL, HORIZONTAL_ORIGIN)
            };
            let mask = ContentMask {
                bounds: Bounds::from_corners(
                    point(x((head - WIND_LENGTH).max(0.)), bounds.top()),
                    point(x(head), bounds.bottom()),
                ),
            };
            window.with_content_mask(Some(mask), |window| {
                for (path, color) in &mesh.strokes {
                    window.paint_path(path.clone(), color.opacity(strength));
                }
            });
        },
    )
    .absolute()
    .inset_0()
    .size_full()
}

impl WindMesh {
    fn new(bounds: Bounds<Pixels>, theme: Theme) -> Self {
        let mut strokes = Vec::new();
        for (lane, tone) in (0_u16..).zip([Tone::Mint, Tone::Lavender, Tone::Peach]) {
            for (width, color) in [
                (17., tone.surface(theme).opacity(0.78)),
                (1.5, tone.accent(theme).opacity(0.72)),
            ] {
                let mut path = PathBuilder::stroke(px(width));
                for step in 0..=WIND_SEGMENTS {
                    let t = WIND_END * f32::from(step) / f32::from(WIND_SEGMENTS);
                    let lane_phase = f32::from(lane) * 0.8;
                    let wave = t.mul_add(TAU, lane_phase).sin() * 0.045;
                    let arch = (t * PI).sin() * 0.24;
                    let lane_offset = f32::from(lane) * 0.075;
                    let p = bounds.origin
                        + point(
                            bounds.size.width * t.mul_add(HORIZONTAL_TRAVEL, HORIZONTAL_ORIGIN),
                            bounds.size.height * (0.48 - arch + lane_offset + wave),
                        );
                    if step == 0 {
                        path.move_to(p);
                    } else {
                        path.line_to(p);
                    }
                }
                match path.build() {
                    Ok(path) => strokes.push((path, color)),
                    Err(error) => {
                        tracing::error!(event = "share.wind_mesh_failed", lane, width, ?error);
                    }
                }
            }
        }
        Self {
            bounds,
            theme,
            strokes,
        }
    }
}
