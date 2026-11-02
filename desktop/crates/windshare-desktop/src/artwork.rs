use gpui::{IntoElement, Point, point, px, size};
use std::sync::LazyLock;
use windshare_ui::VectorArtwork;

// Cover the largest flight pose with one atlas size throughout the choreography.
const SPIRIT_RASTER_SIZE: f32 = 224.;
const PAPER_RASTER_WIDTH: f32 = 100.;
const PAPER_ASPECT: f32 = 1.14;

static ARTWORK: LazyLock<[VectorArtwork; 5]> = LazyLock::new(|| {
    [
        include_bytes!("../assets/spirit.svg").as_slice(),
        include_bytes!("../assets/spirit-happy.svg").as_slice(),
        include_bytes!("../assets/paper.svg").as_slice(),
        include_bytes!("../assets/photo.svg").as_slice(),
        include_bytes!("../assets/folder.svg").as_slice(),
    ]
    .map(|source| VectorArtwork::from_svg(source).expect("bundled vector artwork must be valid"))
});

#[derive(Clone, Copy)]
pub enum Artwork {
    Spirit,
    Happy,
    Paper,
    Photo,
    Folder,
}

impl Artwork {
    pub fn preload() {
        // Resolve source geometry before the first animation frame, never during motion.
        LazyLock::force(&ARTWORK);
        tracing::info!(event = "desktop.artwork_ready", assets = ARTWORK.len());
    }

    pub fn sprite(self, center: Point<f32>, width: f32) -> impl IntoElement {
        let (raster_width, raster_height) = match self {
            Self::Spirit | Self::Happy => (SPIRIT_RASTER_SIZE, SPIRIT_RASTER_SIZE),
            Self::Paper | Self::Photo | Self::Folder => {
                (PAPER_RASTER_WIDTH, PAPER_RASTER_WIDTH * PAPER_ASPECT)
            }
        };
        ARTWORK[self as usize].sprite(
            size(px(raster_width), px(raster_height)),
            point(px(center.x), px(center.y)),
            width / raster_width,
        )
    }
}
