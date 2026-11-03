use anyhow::{Context as _, Result, bail, ensure};
use gpui::{
    App, Bounds, FillOptions, Hsla, IntoElement, Path, PathBuilder, PathStyle, Pixels, Point,
    Radians, Size, TransformationMatrix, Window, canvas, point, prelude::*, px, rgba, size,
};
use std::sync::Arc;
use usvg::{FillRule, Node, Paint, PaintOrder, tiny_skia_path::PathSegment};

// Unit-viewport tessellation keeps curve error below a tenth of a pixel even
// at a 400-device-pixel display size, without tessellating during animation.
const CURVE_TOLERANCE: f32 = 1. / 4096.;

/// Solid-color SVG artwork, parsed once into ordered, reusable vector meshes.
/// Filters, clipping and group compositing are deliberately outside this format.
pub struct VectorArtwork {
    layers: Arc<[Layer]>,
}
struct Layer {
    outline: usvg::tiny_skia_path::Path,
    fill_rule: FillRule,
    mesh: Path<Pixels>,
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

    pub fn sprite(&self, size: Size<Pixels>, center: Point<Pixels>) -> VectorSprite {
        VectorSprite {
            layers: self.layers.clone(),
            blend: None,
            size,
            center,
            rotation: Radians(0.),
        }
    }
}

#[derive(IntoElement)]
#[must_use]
pub struct VectorSprite {
    layers: Arc<[Layer]>,
    blend: Option<ArtworkBlend>,
    size: Size<Pixels>,
    center: Point<Pixels>,
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
            size,
            center,
            rotation,
        } = self;
        canvas(
            |_, _, _| (),
            move |bounds, (), window, _| {
                let center = bounds.origin + center;
                let (sin, cos) = rotation.0.sin_cos();
                let transform = TransformationMatrix {
                    rotation_scale: [
                        [cos * f32::from(size.width), -sin * f32::from(size.height)],
                        [sin * f32::from(size.width), cos * f32::from(size.height)],
                    ],
                    translation: [f32::from(center.x), f32::from(center.y)],
                };
                // Antialias after the final transform. Shrinking a cached SVG mask
                // loses edge coverage because the atlas has no minification levels.
                // Path painting also preserves fractional motion and applies DPI once.
                let mut paint = |layers: &[Layer], opacity: f32| {
                    for layer in layers {
                        window.paint_path(
                            transformed_mesh(&layer.mesh, transform),
                            layer.color.opacity(opacity),
                        );
                    }
                };
                if let Some(blend) = blend {
                    // Fading two complete, overlapping sprites changes body coverage
                    // and edge contrast. Only their differing paint suffix may fade.
                    let shared = layers
                        .iter()
                        .zip(blend.layers.iter())
                        .take_while(|(a, b)| {
                            a.color == b.color
                                && a.fill_rule == b.fill_rule
                                && a.outline == b.outline
                        })
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

fn transformed_mesh(mesh: &Path<Pixels>, transform: TransformationMatrix) -> Path<Pixels> {
    let mut path = mesh.clone();
    for vertex in &mut path.vertices {
        vertex.xy_position = transform.apply(vertex.xy_position);
    }
    let [[xx, xy], [yx, yy]] = transform.rotation_scale;
    path.bounds = Bounds::centered_at(
        transform.apply(mesh.bounds.center()),
        size(
            mesh.bounds.size.width * xx.abs() + mesh.bounds.size.height * xy.abs(),
            mesh.bounds.size.width * yx.abs() + mesh.bounds.size.height * yy.abs(),
        ),
    );
    path
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
    let outline = path
        .transform(usvg::Transform::from_row(
            1. / viewport.width(),
            0.,
            0.,
            1. / viewport.height(),
            -0.5,
            -0.5,
        ))
        .context("invalid artwork viewport")?;
    let fill_rule = match rule {
        FillRule::NonZero => gpui::FillRule::NonZero,
        FillRule::EvenOdd => gpui::FillRule::EvenOdd,
    };
    let mut builder = PathBuilder::fill().with_style(PathStyle::Fill(
        FillOptions::default()
            .with_fill_rule(fill_rule)
            .with_tolerance(CURVE_TOLERANCE),
    ));
    let position = |p: usvg::tiny_skia_path::Point| point(px(p.x), px(p.y));
    for segment in outline.segments() {
        match segment {
            PathSegment::MoveTo(p) => builder.move_to(position(p)),
            PathSegment::LineTo(p) => builder.line_to(position(p)),
            PathSegment::QuadTo(a, b) => builder.curve_to(position(b), position(a)),
            PathSegment::CubicTo(a, b, c) => {
                builder.cubic_bezier_to(position(c), position(a), position(b));
            }
            PathSegment::Close => builder.close(),
        }
    }
    let color = rgba(u32::from_be_bytes([
        color.red,
        color.green,
        color.blue,
        255,
    ]));
    Ok(Layer {
        outline,
        fill_rule: rule,
        mesh: builder.build().context("invalid artwork mesh")?,
        color: Hsla::from(color).opacity(opacity),
    })
}
