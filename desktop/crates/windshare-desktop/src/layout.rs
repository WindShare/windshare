use gpui::{Window, px};

const SHORT_WINDOW_HEIGHT: f32 = 800.;
pub const RECEIVE_STAGE_HEIGHT: f32 = 430.;

#[derive(Clone, Copy)]
pub enum FlowLayout {
    Compact,
    Roomy,
}

impl FlowLayout {
    pub fn for_window(window: &Window) -> Self {
        if window.viewport_size().height < px(SHORT_WINDOW_HEIGHT) {
            Self::Compact
        } else {
            Self::Roomy
        }
    }
    pub fn page_top(self) -> f32 {
        match self {
            Self::Compact => 24.,
            Self::Roomy => 40.,
        }
    }
    pub fn panel_padding(self) -> f32 {
        match self {
            Self::Compact => 24.,
            Self::Roomy => 30.,
        }
    }
    pub fn stage_height(self) -> f32 {
        match self {
            Self::Compact => 360.,
            Self::Roomy => 384.,
        }
    }
    pub fn scene_scale(self) -> f32 {
        match self {
            Self::Compact => 0.72,
            Self::Roomy => 1.,
        }
    }
}
