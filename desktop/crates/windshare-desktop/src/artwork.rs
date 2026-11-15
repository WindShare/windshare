use gpui::{Point, point, px, size};
use std::sync::LazyLock;
use windshare_ui::{VectorArtwork, VectorSprite};

static ARTWORK: LazyLock<[VectorArtwork; 4]> = LazyLock::new(|| {
    [
        include_bytes!("../assets/spirit.svg").as_slice(),
        include_bytes!("../assets/spirit-happy.svg").as_slice(),
        include_bytes!("../assets/spirit-mark.svg").as_slice(),
        include_bytes!("../assets/spirit-super.svg").as_slice(),
    ]
    .map(|source| VectorArtwork::from_svg(source).expect("bundled vector artwork must be valid"))
});

#[derive(Clone, Copy)]
pub enum Artwork {
    Spirit,
    Happy,
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
        ARTWORK[self as usize].sprite(
            size(px(width), px(width)),
            point(px(center.x), px(center.y)),
        )
    }
}
