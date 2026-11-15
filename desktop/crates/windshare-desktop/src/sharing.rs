mod detail;
mod link;
mod link_views;
mod preview;
mod qr;
mod result;
mod result_delay;

pub use detail::{detail_width, enlarged_qr_side};
pub use link::{LinkDisclosure, ShareLink};

use crate::{
    desktop::Desktop,
    flow_state::ShareFlow,
    layout::FlowLayout,
    localization::{Text, language},
    model::{Action, ShareState},
    motion::{SpiritMood, SpiritScene, scaled_spirit},
};
use gpui::{Div, Window, prelude::*, px};
use std::time::Instant;
use windshare_ui::{ButtonStyle, Icon, LARGE_BUTTON_HEIGHT, Theme, Tone, column, row};

const SHARE_CHOICE_BUTTON_MIN_WIDTH: f32 = 160.;
const SHARE_CHOICE_SECTION_GAP: f32 = 24.;
const SHARE_CHOICE_TITLE_HEIGHT: f32 = 36.;

fn stage_height(layout: FlowLayout) -> f32 {
    layout.stage_height().max(
        SpiritScene::height(layout.scene_scale())
            + SHARE_CHOICE_TITLE_HEIGHT
            + SHARE_CHOICE_SECTION_GAP
            + LARGE_BUTTON_HEIGHT,
    )
}

impl Desktop {
    pub fn sharing_page(
        &self,
        layout: FlowLayout,
        window: &Window,
        cx: &Context<Self>,
    ) -> gpui::AnyElement {
        let theme = Theme::current(window, cx);
        match self.share_flow {
            ShareFlow::Choose => column()
                .id("share-stage")
                .drag_over::<gpui::ExternalPaths>(move |style, _, _, _| {
                    style.bg(Tone::Mint.surface(theme)).rounded(px(24.))
                })
                .min_h(px(stage_height(layout)))
                .justify_center()
                .child(self.choose_share(theme, layout, cx))
                .into_any_element(),
            ShareFlow::Running(run) => {
                let Some(share) = self.shares.iter().find(|share| share.id == run.share_id) else {
                    return column().into_any_element();
                };
                match share.state {
                    ShareState::AwaitingAuthorization => {
                        return self
                            .authorization_share(theme, share, cx)
                            .into_any_element();
                    }
                    ShareState::AwaitingConfiguration(blocking)
                    | ShareState::AwaitingNetwork(blocking) => {
                        return self
                            .blocked_share(theme, share, blocking, cx)
                            .into_any_element();
                    }
                    _ => {}
                }
                if !share.finished() && !run.remaining(Instant::now()).is_zero() {
                    return column()
                        .id("share-stage")
                        .min_h(px(stage_height(layout)))
                        .items_center()
                        .justify_center()
                        .gap(px(8.))
                        .child(theme.heading(language(cx).text(Text::PreparingShare), 26.))
                        .child(theme.caption(share.name.clone()))
                        .into_any_element();
                }
                self.share_result(theme, share, layout, window, cx)
                    .id("share-stage")
                    .into_any_element()
            }
        }
    }

    fn choose_share(&self, theme: Theme, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .items_center()
            .child(scaled_spirit(
                "share-spirit",
                SpiritMood::Idle,
                Tone::Mint,
                layout.scene_scale(),
                true,
            ))
            .child(
                theme
                    .heading(locale.text(Text::ChooseShare), 26.)
                    .line_height(px(SHARE_CHOICE_TITLE_HEIGHT))
                    .mb(px(SHARE_CHOICE_SECTION_GAP)),
            )
            .child(
                row()
                    .gap(px(12.))
                    .child(
                        Self::button(
                            "choose-files",
                            locale.text(Text::ChooseFiles),
                            Icon::Plus,
                            ButtonStyle::Primary,
                            Action::SelectFiles,
                            cx,
                        )
                        .large()
                        .min_width(px(SHARE_CHOICE_BUTTON_MIN_WIDTH))
                        .disabled(self.runtime.selection_disabled())
                        .focus_handle(&self.primary_focus),
                    )
                    .child(
                        Self::button(
                            "choose-folder",
                            locale.text(Text::ChooseFolder),
                            Icon::Folder,
                            ButtonStyle::Secondary,
                            Action::SelectFolder,
                            cx,
                        )
                        .large()
                        .min_width(px(SHARE_CHOICE_BUTTON_MIN_WIDTH))
                        .disabled(self.runtime.selection_disabled()),
                    ),
            )
    }
}
