use gpui::{FontWeight, Styled, px};

pub const FONT: &str = "Manrope";
#[cfg(target_os = "windows")]
pub const UI_FONT: &str = "Microsoft YaHei UI";
#[cfg(not(target_os = "windows"))]
pub const UI_FONT: &str = "Noto Sans CJK SC";
pub const INPUT_FONT_SIZE: f32 = 16.;
pub const INPUT_LINE_HEIGHT: f32 = 26.;

#[derive(Clone, Copy)]
pub enum ControlText {
    Compact,
    Regular,
    Large,
    Status,
}

impl ControlText {
    pub fn apply<E: Styled>(self, element: E) -> E {
        let (size, line_height) = match self {
            Self::Compact => (14., 20.),
            Self::Regular => (15., 22.),
            Self::Large => (16., 24.),
            Self::Status => (13., 20.),
        };
        // YaHei UI maps semibold to its bold face, which crowds small CJK text.
        // Medium preserves a lighter face there and real medium on other fonts.
        let weight = match self {
            Self::Status => FontWeight::NORMAL,
            _ => FontWeight::MEDIUM,
        };
        element
            .font_family(UI_FONT)
            .text_size(px(size))
            .line_height(px(line_height))
            .font_weight(weight)
            .whitespace_nowrap()
    }
}
