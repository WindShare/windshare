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
    rgb(0x28_3c_36).into()
}
pub fn muted() -> Hsla {
    rgb(0x62_71_67).into()
}
pub fn border() -> Hsla {
    rgb(0xe7_eb_e5).into()
}
pub fn paper() -> Hsla {
    rgb(0xff_ff_ff).into()
}
pub fn canvas_color() -> Hsla {
    rgb(0xf9_fa_f7).into()
}
pub fn primary() -> Hsla {
    rgb(0x37_64_53).into()
}
pub fn action_surface() -> Hsla {
    rgb(0xb4_eb_cd).into()
}
pub fn action_hover() -> Hsla {
    rgb(0x9f_e2_bf).into()
}
pub fn notification_surface() -> Hsla {
    rgb(0xe2_f9_ec).into()
}
pub fn notification_border() -> Hsla {
    rgb(0xb9_e8_cc).into()
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
            Self::Mint => 0xe9_f3_eb,
            Self::Lavender => 0xf0_ed_f9,
            Self::Peach => 0xfb_f0_e5,
            Self::Blue => 0xea_f1_f8,
            Self::Rose => 0xf9_ea_ea,
            Self::Neutral => 0xf3_f5_f0,
        })
        .into()
    }
    pub fn accent(self) -> Hsla {
        rgb(match self {
            Self::Mint => 0x9e_c7_ad,
            Self::Lavender => 0xbe_b0_df,
            Self::Peach => 0xe9_ba_91,
            Self::Blue => 0xa8_c7_dd,
            Self::Rose => 0xde_aa_a9,
            Self::Neutral => 0xc4_cd_c1,
        })
        .into()
    }
    pub fn text(self) -> Hsla {
        rgb(match self {
            Self::Mint => 0x42_69_4f,
            Self::Lavender => 0x6d_54_8e,
            Self::Peach => 0x90_60_3a,
            Self::Blue => 0x45_69_82,
            Self::Rose => 0x98_4d_57,
            Self::Neutral => 0x5d_6d_62,
        })
        .into()
    }
}
