use gpui::{Window, px};
use windshare_ui::{COMPACT_WIDTH, SIDEBAR_COMPACT_WIDTH, SIDEBAR_WIDTH};

pub const MAX_PAGE_WIDTH: f32 = 960.;
const PANEL_BORDER_WIDTH: f32 = 1.;
pub const fn page_gutter(compact: bool) -> f32 {
    if compact { 28. } else { 48. }
}

pub fn task_width(window: &Window, layout: FlowLayout) -> f32 {
    let viewport = f32::from(window.viewport_size().width);
    let compact = viewport < COMPACT_WIDTH;
    let sidebar = if compact {
        SIDEBAR_COMPACT_WIDTH
    } else {
        SIDEBAR_WIDTH
    };
    let horizontal_gutters = 2. * page_gutter(compact);
    let panel_width = (viewport - sidebar - horizontal_gutters).min(MAX_PAGE_WIDTH);
    let horizontal_padding = 2. * layout.panel_padding();
    let horizontal_borders = 2. * PANEL_BORDER_WIDTH;
    panel_width - horizontal_padding - horizontal_borders
}

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
    pub const fn page_top(self) -> f32 {
        match self {
            Self::Compact => 24.,
            Self::Roomy => 40.,
        }
    }
    pub const fn panel_padding(self) -> f32 {
        match self {
            Self::Compact => 24.,
            Self::Roomy => 30.,
        }
    }
    pub const fn stage_height(self) -> f32 {
        match self {
            Self::Compact => 360.,
            Self::Roomy => 384.,
        }
    }
    pub const fn scene_scale(self) -> f32 {
        match self {
            Self::Compact => 0.72,
            Self::Roomy => 1.,
        }
    }
}
