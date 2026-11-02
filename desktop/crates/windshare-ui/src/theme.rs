use gpui::{Hsla, rgb};

pub const FONT: &str = "Manrope";
#[cfg(target_os = "windows")]
pub const UI_FONT: &str = "Microsoft YaHei UI";
#[cfg(not(target_os = "windows"))]
pub const UI_FONT: &str = "Noto Sans CJK SC";
pub const INPUT_FONT_SIZE: f32 = 16.;
pub const INPUT_LINE_HEIGHT: f32 = 26.;
pub const SIDEBAR_WIDTH: f32 = 216.;
pub const SIDEBAR_COMPACT_WIDTH: f32 = 184.;
pub const COMPACT_WIDTH: f32 = 1120.;
pub const RADIUS: f32 = 18.;
pub const MOTION_MS: u64 = 220;

pub fn ink() -> Hsla {
    rgb(0x283c36).into()
}
pub fn muted() -> Hsla {
    rgb(0x627167).into()
}
pub fn border() -> Hsla {
    rgb(0xe7ebe5).into()
}
pub fn paper() -> Hsla {
    rgb(0xffffff).into()
}
pub fn canvas_color() -> Hsla {
    rgb(0xf9faf7).into()
}
pub fn primary() -> Hsla {
    rgb(0x376453).into()
}
pub fn action_surface() -> Hsla {
    rgb(0xb4ebcd).into()
}
pub fn action_hover() -> Hsla {
    rgb(0x9fe2bf).into()
}
pub fn notification_surface() -> Hsla {
    rgb(0xe2f9ec).into()
}
pub fn notification_border() -> Hsla {
    rgb(0xb9e8cc).into()
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tone {
    Mint,
    Lavender,
    Peach,
    Blue,
    Rose,
    Neutral,
}

impl Tone {
    pub fn surface(self) -> Hsla {
        rgb(match self {
            Self::Mint => 0xe9f3eb,
            Self::Lavender => 0xf0edf9,
            Self::Peach => 0xfbf0e5,
            Self::Blue => 0xeaf1f8,
            Self::Rose => 0xf9eaea,
            Self::Neutral => 0xf3f5f0,
        })
        .into()
    }
    pub fn accent(self) -> Hsla {
        rgb(match self {
            Self::Mint => 0x9ec7ad,
            Self::Lavender => 0xbeb0df,
            Self::Peach => 0xe9ba91,
            Self::Blue => 0xa8c7dd,
            Self::Rose => 0xdeaaa9,
            Self::Neutral => 0xc4cdc1,
        })
        .into()
    }
    pub fn text(self) -> Hsla {
        rgb(match self {
            Self::Mint => 0x42694f,
            Self::Lavender => 0x6d548e,
            Self::Peach => 0x90603a,
            Self::Blue => 0x456982,
            Self::Rose => 0x984d57,
            Self::Neutral => 0x5d6d62,
        })
        .into()
    }
}
