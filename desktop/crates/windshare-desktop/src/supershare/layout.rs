use crate::layout::{STATUSBAR_HEIGHT, TITLEBAR_HEIGHT};
use gpui::Window;
use windshare_ui::{COMPACT_WIDTH, SIDEBAR_COMPACT_WIDTH, SIDEBAR_WIDTH};

pub(super) const PURCHASE_MAX_WIDTH: f32 = 580.;
pub(super) const COLUMN_GAP: f32 = 64.;
pub(super) const STACKED_HERO_GAP: f32 = 24.;
const CONTENT_MAX_WIDTH: f32 = 1160.;
const COLUMN_LAYOUT_MIN_WIDTH: f32 = 960.;
const DETAILS_WIDTH_SHARE: f32 = 0.52;
const EMBLEM_MAX_SIZE: f32 = 480.;
const STACKED_EMBLEM_WIDTH_SHARE: f32 = 0.38;
const STACKED_EMBLEM_MIN_SIZE: f32 = 180.;
const STACKED_EMBLEM_MAX_SIZE: f32 = 300.;
const TITLE_WIDTH_PER_EM: f32 = 6.2;
const TITLE_MAX_SIZE: f32 = 92.;

#[derive(Clone, Copy)]
pub(super) enum OfferLayout {
    Columns { details_width: f32 },
    Stacked,
}

#[derive(Clone, Copy)]
pub(super) struct Stage {
    pub gutter: f32,
    pub height: f32,
    pub content_width: f32,
    pub layout: OfferLayout,
    pub emblem_size: f32,
    pub title_size: f32,
}

impl Stage {
    pub fn for_window(window: &Window) -> Self {
        let width = f32::from(window.viewport_size().width);
        let height = f32::from(window.viewport_size().height);
        let compact = width < COMPACT_WIDTH;
        let gutter = if compact { 32. } else { 56. };
        let sidebar = if compact {
            SIDEBAR_COMPACT_WIDTH
        } else {
            SIDEBAR_WIDTH
        };
        // The canvas fills the window, but its composition must remain a single visual group.
        let content_width = (-2.0_f32)
            .mul_add(gutter, width - sidebar)
            .min(CONTENT_MAX_WIDTH);
        let (layout, emblem_size, title_width) = if content_width >= COLUMN_LAYOUT_MIN_WIDTH {
            let details_width = (content_width * DETAILS_WIDTH_SHARE).min(PURCHASE_MAX_WIDTH);
            (
                OfferLayout::Columns { details_width },
                (content_width - details_width - COLUMN_GAP).min(EMBLEM_MAX_SIZE),
                details_width,
            )
        } else {
            let emblem_size = (content_width * STACKED_EMBLEM_WIDTH_SHARE)
                .clamp(STACKED_EMBLEM_MIN_SIZE, STACKED_EMBLEM_MAX_SIZE);
            (
                OfferLayout::Stacked,
                emblem_size,
                content_width - emblem_size - STACKED_HERO_GAP,
            )
        };
        Self {
            gutter,
            height: height - TITLEBAR_HEIGHT - STATUSBAR_HEIGHT,
            content_width,
            layout,
            emblem_size,
            title_size: (title_width / TITLE_WIDTH_PER_EM).min(TITLE_MAX_SIZE),
        }
    }
}
