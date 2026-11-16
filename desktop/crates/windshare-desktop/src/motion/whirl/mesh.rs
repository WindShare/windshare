//! One path per batch of squares. Hundreds of motes cost a single draw, and
//! path rasterization keeps the fractional positions and turns that
//! pixel-snapped quads would quantize into jitter.

use gpui::{Background, Path, Pixels, Point, Window, point, px};

const VERTICES_PER_SQUARE: usize = 6;
/// Below this side a square covers no visible share of a pixel.
const VISIBLE_SIDE: f32 = 0.05;
/// Curve coordinates that keep a whole triangle inside the path shader's fill.
const SOLID: Point<f32> = Point { x: 0., y: 1. };

pub struct Squares {
    path: Option<Path<Pixels>>,
    vertices: usize,
}

impl Squares {
    pub const fn with_capacity(squares: usize) -> Self {
        Self {
            path: None,
            vertices: squares * VERTICES_PER_SQUARE,
        }
    }

    /// `center` is in window coordinates and `turn` in radians, clockwise on screen.
    pub fn push(&mut self, center: Point<f32>, side: f32, turn: f32) {
        if side < VISIBLE_SIDE {
            return;
        }
        let half = side / 2.;
        let (sin, cos) = turn.sin_cos();
        let diagonal = point((cos - sin) * half, (sin + cos) * half);
        let other = point(-(cos + sin) * half, (cos - sin) * half);
        let corner = |offset: Point<f32>| point(px(center.x + offset.x), px(center.y + offset.y));
        let corners = [
            corner(diagonal),
            corner(other),
            corner(-diagonal),
            corner(-other),
        ];
        let vertices = self.vertices;
        // A path's bounds grow from its first point, so that point must be a
        // vertex: an arbitrary origin would stretch the rasterized area.
        let path = self.path.get_or_insert_with(|| {
            let mut path = Path::new(corners[0]);
            path.vertices.reserve(vertices);
            path
        });
        path.push_triangle((corners[0], corners[1], corners[2]), (SOLID, SOLID, SOLID));
        path.push_triangle((corners[0], corners[2], corners[3]), (SOLID, SOLID, SOLID));
    }

    pub fn paint(self, window: &mut Window, color: impl Into<Background>) {
        if let Some(path) = self.path {
            window.paint_path(path, color);
        }
    }
}
