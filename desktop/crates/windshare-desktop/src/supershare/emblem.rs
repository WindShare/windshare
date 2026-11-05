use super::visuals::Finish;
use crate::artwork::Artwork;
use gpui::{
    Animation, AnimationExt, App, Div, IntoElement, Path, PathBuilder, Pixels, Window, canvas, div,
    linear_color_stop, linear_gradient, point, prelude::*, px,
};
use std::{f32::consts::TAU, sync::Arc, time::Duration};
use windshare_ui::{Icon, Theme, icon};

const FLOAT_PERIOD: Duration = Duration::from_secs(7);
const FLOAT_DISTANCE: f32 = 7.;
const DISC_INSET: f32 = 0.12;
const INNER_RING_INSET: f32 = 0.155;
const BORDER_WIDTH: f32 = 1.;
const ARTWORK_RATIO: f32 = 0.62;

pub(super) fn emblem(theme: Theme, span: f32, still: bool) -> Div {
    let finish = Finish::new(theme);
    let disc = Disc {
        finish,
        span,
        offset: 0.,
    };
    let disc = if still {
        disc.into_any_element()
    } else {
        disc.with_animation(
            "supershare-emblem",
            Animation::new(FLOAT_PERIOD).repeat(),
            |mut disc, delta| {
                disc.offset = (delta * TAU).sin() * FLOAT_DISTANCE;
                disc
            },
        )
        .into_any_element()
    };
    div()
        .relative()
        .size(px(span))
        .flex_shrink_0()
        .child(
            div()
                .absolute()
                .inset_0()
                .size(px(span))
                .rounded_full()
                .border_1()
                .border_color(finish.edge.opacity(0.18)),
        )
        .child(disc)
        .children([
            sparkle(span * 0.10, span * 0.19, 23., finish.foil),
            sparkle(span * 0.86, span * 0.65, 17., finish.foil),
            sparkle(span * 0.71, span * 0.05, 11., finish.ink.opacity(0.5)),
            sparkle(span * 0.25, span * 0.91, 12., finish.ink.opacity(0.4)),
        ])
}

#[derive(IntoElement)]
struct Disc {
    finish: Finish,
    span: f32,
    offset: f32,
}

struct DiscMesh {
    span: Pixels,
    paths: [Path<Pixels>; 3],
}

impl RenderOnce for Disc {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let Self {
            finish,
            span,
            offset,
        } = self;
        // Layout offsets and quads both snap to device pixels. Slow decorative
        // motion must move mesh vertices and artwork centers only, after layout.
        div()
            .relative()
            .size(px(span))
            .flex_shrink_0()
            .child(
                canvas(
                    move |_, window, _| {
                        window.with_global_id("supershare-disc-mesh".into(), |id, window| {
                            window.with_element_state(id, |cached: Option<Arc<DiscMesh>>, _| {
                                let mesh = cached
                                    .filter(|mesh| mesh.span == px(span))
                                    .unwrap_or_else(|| Arc::new(DiscMesh::new(span)));
                                (mesh.clone(), mesh)
                            })
                        })
                    },
                    move |bounds, mesh, window, _| {
                        let origin = bounds.origin + point(px(0.), px(offset));
                        let fills = [
                            linear_gradient(
                                140.,
                                linear_color_stop(finish.pearl.opacity(0.82), 0.),
                                linear_color_stop(finish.mint.opacity(0.18), 1.),
                            ),
                            finish.pearl.opacity(0.75).into(),
                            finish.edge.opacity(0.25).into(),
                        ];
                        for (mesh, fill) in mesh.paths.iter().zip(fills) {
                            let mut path = mesh.clone();
                            for vertex in &mut path.vertices {
                                vertex.xy_position += origin;
                            }
                            path.bounds.origin += origin;
                            window.paint_path(path, fill);
                        }
                    },
                )
                .absolute()
                .inset_0()
                .size_full(),
            )
            .child(Artwork::Super.sprite(
                point(span * 0.5, span.mul_add(0.5, offset)),
                span * ARTWORK_RATIO,
            ))
    }
}

impl DiscMesh {
    fn new(span: f32) -> Self {
        let radius = span * (0.5 - DISC_INSET);
        let inner_radius = span * (0.5 - INNER_RING_INSET);
        let paths = [
            circle(PathBuilder::fill(), span, radius),
            circle(
                PathBuilder::stroke(px(BORDER_WIDTH)),
                span,
                radius - BORDER_WIDTH / 2.,
            ),
            circle(
                PathBuilder::stroke(px(BORDER_WIDTH)),
                span,
                inner_radius - BORDER_WIDTH / 2.,
            ),
        ];
        tracing::debug!(
            event = "supershare.disc_mesh_rebuilt",
            session = std::process::id(),
            span,
        );
        Self {
            span: px(span),
            paths,
        }
    }
}

fn circle(mut builder: PathBuilder, span: f32, radius: f32) -> Path<Pixels> {
    let center = point(px(span / 2.), px(span / 2.));
    let radius = px(radius);
    let radii = point(radius, radius);
    let right = center + point(radius, px(0.));
    builder.move_to(right);
    builder.arc_to(radii, px(0.), false, true, center - point(radius, px(0.)));
    builder.arc_to(radii, px(0.), false, true, right);
    builder.close();
    builder.build().expect("emblem circles are valid")
}

fn sparkle(x: f32, y: f32, size: f32, color: gpui::Hsla) -> Div {
    div()
        .absolute()
        .left(px(x))
        .top(px(y))
        .child(icon(Icon::Spark, size).text_color(color))
}
