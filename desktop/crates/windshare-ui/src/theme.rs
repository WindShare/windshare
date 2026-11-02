use gpui::{Hsla, Subscription, Window, WindowAppearance, rgb};

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

/// A frame's palette, resolved from its native window rather than shared mutable state.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Theme {
    Light,
    Dark,
}

impl From<WindowAppearance> for Theme {
    fn from(appearance: WindowAppearance) -> Self {
        match appearance {
            WindowAppearance::Light | WindowAppearance::VibrantLight => Self::Light,
            WindowAppearance::Dark | WindowAppearance::VibrantDark => Self::Dark,
        }
    }
}

/// Keep this subscription alive for the window's lifetime.
pub fn follow_system_theme(window: &Window) -> Subscription {
    trace_appearance(window, "initial");
    window.observe_window_appearance(|window, _| {
        trace_appearance(window, "system_changed");
        // Entity views and custom canvases may retain their last frame without a
        // notification. Invalidate the whole window, including focused inputs.
        window.refresh();
    })
}

fn trace_appearance(window: &Window, reason: &str) {
    let appearance = window.appearance();
    tracing::info!(
        operation = "desktop.theme_resolved",
        session = std::process::id(),
        window = ?window.window_handle(),
        source = "system",
        reason,
        ?appearance,
        theme = ?Theme::from(appearance),
    );
}

impl Theme {
    fn color(self, light: u32, dark: u32) -> Hsla {
        rgb(match self {
            Self::Light => light,
            Self::Dark => dark,
        })
        .into()
    }

    pub fn text(self) -> Hsla {
        self.color(0x28_3c_36, 0xe3_eb_e5)
    }
    pub fn muted_text(self) -> Hsla {
        self.color(0x62_71_67, 0xa2_b0_a7)
    }
    pub fn border(self) -> Hsla {
        self.color(0xe7_eb_e5, 0x34_43_3b)
    }
    pub fn surface(self) -> Hsla {
        self.color(0xff_ff_ff, 0x1d_27_22)
    }
    pub fn canvas(self) -> Hsla {
        self.color(0xf9_fa_f7, 0x14_1c_18)
    }
    pub fn sidebar(self) -> Hsla {
        self.color(0xf0_f5_ef, 0x18_22_1c)
    }
    pub fn navigation_hover(self) -> Hsla {
        self.color(0xe4_e8_e5, 0x2b_34_30)
    }
    pub fn inset(self) -> Hsla {
        self.color(0xef_f2_ec, 0x11_19_15)
    }
    pub fn accent_text(self) -> Hsla {
        self.color(0x37_64_53, 0xa4_d8_ba)
    }
    pub fn action_surface(self) -> Hsla {
        self.color(0xb4_eb_cd, 0x9c_d9_b8)
    }
    pub fn action_hover(self) -> Hsla {
        self.color(0x9f_e2_bf, 0xb2_e7_ca)
    }
    pub fn action_text(self) -> Hsla {
        self.color(0x28_50_3d, 0x14_32_24)
    }
    pub fn notification_surface(self) -> Hsla {
        self.color(0xe2_f9_ec, 0x23_3b_2d)
    }
    pub fn notification_border(self) -> Hsla {
        self.color(0xb9_e8_cc, 0x46_6e_55)
    }
    pub fn focus_ring(self) -> Hsla {
        self.accent_text()
    }
    pub fn selection(self) -> Hsla {
        self.color(0xb5_d8_c2, 0x3a_5e_49)
    }
    pub fn switch_track(self, checked: bool) -> Hsla {
        if checked {
            self.color(0x37_64_53, 0x9c_d9_b8)
        } else {
            self.color(0xc4_cd_c1, 0x4c_5c_51)
        }
    }
    pub fn switch_thumb(self, checked: bool) -> Hsla {
        if checked {
            self.color(0xff_ff_ff, 0x18_32_24)
        } else {
            self.color(0xff_ff_ff, 0xd3_df_d6)
        }
    }
    pub fn overlay(self) -> Hsla {
        match self {
            Self::Light => rgb(0x28_3c_36).opacity(0.18).into(),
            Self::Dark => rgb(0x00_00_00).opacity(0.58).into(),
        }
    }
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
    pub fn navigation_surface(self, theme: Theme) -> Hsla {
        match self {
            // Mint needs more separation from the green sidebar than other tones.
            Self::Mint => theme.color(0xd2_ea_dc, 0x2c_46_36),
            _ => self.surface(theme),
        }
    }
    pub fn surface(self, theme: Theme) -> Hsla {
        let (light, dark) = match self {
            Self::Mint => (0xe9_f3_eb, 0x28_3b_2f),
            Self::Lavender => (0xf0_ed_f9, 0x34_2e_44),
            Self::Peach => (0xfb_f0_e5, 0x41_32_27),
            Self::Blue => (0xea_f1_f8, 0x29_38_45),
            Self::Rose => (0xf9_ea_ea, 0x44_2d_33),
            Self::Neutral => (0xf3_f5_f0, 0x2a_35_2e),
        };
        theme.color(light, dark)
    }
    pub fn accent(self, theme: Theme) -> Hsla {
        let (light, dark) = match self {
            Self::Mint => (0x9e_c7_ad, 0x6c_a9_83),
            Self::Lavender => (0xbe_b0_df, 0xa1_8a_c3),
            Self::Peach => (0xe9_ba_91, 0xc9_98_70),
            Self::Blue => (0xa8_c7_dd, 0x7d_a7_c6),
            Self::Rose => (0xde_aa_a9, 0xc2_87_94),
            Self::Neutral => (0xc4_cd_c1, 0x8a_9e_8e),
        };
        theme.color(light, dark)
    }
    pub fn text(self, theme: Theme) -> Hsla {
        let (light, dark) = match self {
            Self::Mint => (0x42_69_4f, 0xb5_de_c3),
            Self::Lavender => (0x6d_54_8e, 0xd2_c1_eb),
            Self::Peach => (0x90_60_3a, 0xe9_c1_9d),
            Self::Blue => (0x45_69_82, 0xb5_d5_eb),
            Self::Rose => (0x98_4d_57, 0xed_b7_c2),
            Self::Neutral => (0x5d_6d_62, 0xc2_cf_c5),
        };
        theme.color(light, dark)
    }
}
