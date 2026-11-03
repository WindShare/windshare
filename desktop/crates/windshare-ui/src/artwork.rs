use anyhow::{Context as _, Result, bail, ensure};
use gpui::{
    App, Bounds, Hsla, IntoElement, Pixels, Point, Radians, SharedString, Size,
    TransformationMatrix, Window, canvas, prelude::*, rgba, size,
};
use std::{
    fmt::Write as _,
    hash::{Hash, Hasher},
    sync::Arc,
};
use usvg::{FillRule, Node, Paint, PaintOrder, tiny_skia_path::PathSegment};

/// Solid-color SVG artwork, parsed once into ordered, reusable GPU masks.
/// Filters, clipping and group compositing are deliberately outside this format.
pub struct VectorArtwork {
    layers: Arc<[Layer]>,
}
struct Layer {
    key: SharedString,
    mask: Arc<[u8]>,
    color: Hsla,
}

impl VectorArtwork {
    /// Preserves SVG paint order and stroke outlines without duplicating the source artwork.
    ///
    /// # Errors
    /// Returns an error for malformed SVGs or unsupported compositing/paint features.
    pub fn from_svg(source: &[u8]) -> Result<Self> {
        let tree = usvg::Tree::from_data(source, &usvg::Options::default())?;
        let mut layers = Vec::new();
        collect_layers(tree.root(), tree.size(), &mut layers)?;
        ensure!(!layers.is_empty(), "vector artwork has no visible layers");
        Ok(Self {
            layers: layers.into(),
        })
    }

    pub fn sprite(
        &self,
        raster_size: Size<Pixels>,
        center: Point<Pixels>,
        scale: f32,
    ) -> VectorSprite {
        VectorSprite {
            layers: self.layers.clone(),
            blend: None,
            raster_size,
            center,
            scale,
            rotation: Radians(0.),
        }
    }
}

#[derive(IntoElement)]
#[must_use]
pub struct VectorSprite {
    layers: Arc<[Layer]>,
    blend: Option<ArtworkBlend>,
    raster_size: Size<Pixels>,
    center: Point<Pixels>,
    scale: f32,
    rotation: Radians,
}

struct ArtworkBlend {
    layers: Arc<[Layer]>,
    progress: f32,
}

impl VectorSprite {
    /// Keeps shared background layers opaque while the differing foreground fades.
    pub fn blend_to(mut self, artwork: &VectorArtwork, progress: f32) -> Self {
        self.blend = None;
        if progress >= 1. {
            self.layers = artwork.layers.clone();
        } else if progress > 0. {
            self.blend = Some(ArtworkBlend {
                layers: artwork.layers.clone(),
                progress,
            });
        }
        self
    }

    pub const fn rotate(mut self, rotation: Radians) -> Self {
        self.rotation = rotation;
        self
    }
}

impl RenderOnce for VectorSprite {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let Self {
            layers,
            blend,
            raster_size,
            center,
            scale,
            rotation,
        } = self;
        canvas(
            |_, _, _| (),
            move |bounds, (), window, cx| {
                let factor = window.scale_factor();
                // GPUI snaps the raster quad before applying its transform. Use that
                // same device-aligned quad as the pivot, preserving fractional motion.
                // Keep it at the visible pose because scene culling uses the raw quad.
                let raster_bounds = Bounds::centered_at(bounds.origin + center, raster_size)
                    .map(|value| (value * factor).round() / factor);
                let transform = TransformationMatrix::unit()
                    .translate((bounds.origin + center).scale(factor))
                    .rotate(rotation)
                    .scale(size(
                        scale * (raster_size.width / raster_bounds.size.width),
                        scale * (raster_size.height / raster_bounds.size.height),
                    ))
                    .translate(raster_bounds.center().scale(-factor));
                let mut paint = |layers: &[Layer], opacity: f32| {
                    for layer in layers {
                        if let Err(error) = window.paint_svg(
                            raster_bounds,
                            layer.key.clone(),
                            Some(&layer.mask),
                            transform,
                            layer.color.opacity(opacity),
                            cx,
                        ) {
                            tracing::error!(event = "artwork.paint_failed", asset = %layer.key, %error);
                        }
                    }
                };
                if let Some(blend) = blend {
                    // Fading two complete, overlapping sprites changes body coverage
                    // and edge contrast. Only their differing paint suffix may fade.
                    let shared = layers
                        .iter()
                        .zip(blend.layers.iter())
                        .take_while(|(a, b)| a.key == b.key && a.color == b.color)
                        .count();
                    paint(&layers[..shared], 1.);
                    paint(&layers[shared..], 1. - blend.progress);
                    paint(&blend.layers[shared..], blend.progress);
                } else {
                    paint(&layers, 1.);
                }
            },
        )
        .absolute()
        .inset_0()
        .size_full()
    }
}

fn collect_layers(
    group: &usvg::Group,
    viewport: usvg::Size,
    layers: &mut Vec<Layer>,
) -> Result<()> {
    ensure!(
        group.opacity() == usvg::Opacity::ONE
            && group.blend_mode() == usvg::BlendMode::Normal
            && group.clip_path().is_none()
            && group.mask().is_none()
            && group.filters().is_empty(),
        "vector artwork requires opaque groups without compositing effects"
    );
    for node in group.children() {
        match node {
            Node::Group(child) => collect_layers(child, viewport, layers)?,
            Node::Path(path) if path.is_visible() => {
                let fill = || -> Result<Option<Layer>> {
                    path.fill()
                        .map(|fill| {
                            layer(
                                path.data(),
                                path.abs_transform(),
                                viewport,
                                fill.rule(),
                                fill.paint(),
                                fill.opacity().get(),
                            )
                        })
                        .transpose()
                };
                let stroke = || -> Result<Option<Layer>> {
                    path.stroke()
                        .map(|stroke| {
                            ensure!(
                                stroke.dasharray().is_none(),
                                "dashed artwork is unsupported"
                            );
                            let outline = path
                                .data()
                                .stroke(&stroke.to_tiny_skia(), 1.)
                                .context("invalid artwork stroke")?;
                            layer(
                                &outline,
                                path.abs_transform(),
                                viewport,
                                FillRule::NonZero,
                                stroke.paint(),
                                stroke.opacity().get(),
                            )
                        })
                        .transpose()
                };
                let ordered = match path.paint_order() {
                    PaintOrder::FillAndStroke => [fill()?, stroke()?],
                    PaintOrder::StrokeAndFill => [stroke()?, fill()?],
                };
                layers.extend(ordered.into_iter().flatten());
            }
            Node::Path(_) => {}
            _ => bail!("vector artwork only supports solid paths"),
        }
    }
    Ok(())
}

fn layer(
    path: &usvg::tiny_skia_path::Path,
    transform: usvg::Transform,
    viewport: usvg::Size,
    rule: FillRule,
    paint: &Paint,
    opacity: f32,
) -> Result<Layer> {
    let Paint::Color(color) = paint else {
        bail!("vector artwork only supports solid colors");
    };
    let path = path
        .clone()
        .transform(transform)
        .context("invalid artwork transform")?;
    let mut commands = String::new();
    for segment in path.segments() {
        match segment {
            PathSegment::MoveTo(p) => write!(commands, "M{} {}", p.x, p.y),
            PathSegment::LineTo(p) => write!(commands, "L{} {}", p.x, p.y),
            PathSegment::QuadTo(a, b) => write!(commands, "Q{} {} {} {}", a.x, a.y, b.x, b.y),
            PathSegment::CubicTo(a, b, c) => {
                write!(commands, "C{} {} {} {} {} {}", a.x, a.y, b.x, b.y, c.x, c.y)
            }
            PathSegment::Close => write!(commands, "Z"),
        }?;
    }
    let rule = match rule {
        FillRule::NonZero => "nonzero",
        FillRule::EvenOdd => "evenodd",
    };
    let mask = format!(
        r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {} {}"><path fill="white" fill-rule="{rule}" d="{commands}"/></svg>"#,
        viewport.width(),
        viewport.height()
    );
    let mut hash = std::collections::hash_map::DefaultHasher::new();
    mask.hash(&mut hash);
    let color = rgba(u32::from_be_bytes([
        color.red,
        color.green,
        color.blue,
        255,
    ]));
    Ok(Layer {
        key: format!("vector-artwork/{:016x}", hash.finish()).into(),
        mask: Arc::from(mask.into_bytes()),
        color: Hsla::from(color).opacity(opacity),
    })
}
