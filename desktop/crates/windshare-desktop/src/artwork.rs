use gpui::{Point, point, px, size};
use std::sync::LazyLock;
use windshare_ui::{VectorArtwork, VectorSprite};

const PAPER_ASPECT: f32 = 1.14;

static ARTWORK: LazyLock<[VectorArtwork; 7]> = LazyLock::new(|| {
    [
        include_bytes!("../assets/spirit.svg").as_slice(),
        include_bytes!("../assets/spirit-happy.svg").as_slice(),
        include_bytes!("../assets/paper.svg").as_slice(),
        include_bytes!("../assets/photo.svg").as_slice(),
        include_bytes!("../assets/folder.svg").as_slice(),
        include_bytes!("../assets/spirit-mark.svg").as_slice(),
        include_bytes!("../assets/spirit-super.svg").as_slice(),
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
    BrandMark,
    Super,
}

impl Artwork {
    pub fn preload() {
        // Resolve source geometry before the first animation frame, never during motion.
        LazyLock::force(&ARTWORK);
        tracing::info!(
            event = "desktop.artwork_ready",
            assets = ARTWORK.len(),
            renderer = "vector_mesh"
        );
    }

    pub fn blend_to(
        self,
        other: Self,
        center: Point<f32>,
        width: f32,
        progress: f32,
    ) -> VectorSprite {
        self.sprite(center, width)
            .blend_to(&ARTWORK[other as usize], progress)
    }

    pub fn sprite(self, center: Point<f32>, width: f32) -> VectorSprite {
        let height = match self {
            Self::Spirit | Self::Happy | Self::BrandMark | Self::Super => width,
            Self::Paper | Self::Photo | Self::Folder => width * PAPER_ASPECT,
        };
        ARTWORK[self as usize].sprite(
            size(px(width), px(height)),
            point(px(center.x), px(center.y)),
        )
    }
}
