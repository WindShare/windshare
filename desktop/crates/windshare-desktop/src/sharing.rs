mod activity;
mod detail;
mod link;
mod link_views;
mod preview;
mod qr;
mod result;

pub use detail::{detail_width, enlarged_qr_side};
pub use link::{LinkDisclosure, ShareLink};

use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    flow_state::ShareFlow,
    layout::FlowLayout,
    model::Action,
    motion::{SpiritMood, SpiritState},
    share_animation::{
        SHARE_CHOICE_SECTION_GAP, SHARE_CHOICE_TITLE_HEIGHT, ShareGeometry, ShareScene,
    },
};
use gpui::{Div, Window, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

const SHARE_CHOICE_BUTTON_MIN_WIDTH: f32 = 160.;

impl Desktop {
    pub fn sharing_page(
        &mut self,
        layout: FlowLayout,
        window: &Window,
        cx: &Context<Self>,
    ) -> gpui::AnyElement {
        let theme = Theme::current(window, cx);
        let geometry = ShareGeometry::new(window, layout);
        let still = self.reduced_motion() || cx.reduce_motion();
        let active = window.is_window_active() && window.is_visible();
        match self.share_flow {
            ShareFlow::Choose => column()
                .id("share-stage")
                .relative()
                .drag_over::<gpui::ExternalPaths>(move |style, _, _, _| {
                    style.bg(Tone::Mint.surface(theme)).rounded(px(24.))
                })
                .min_h(px(geometry.height))
                .justify_center()
                .child(self.choose_share(theme, geometry, cx))
                .child(geometry.spirit(SpiritState::Floating(SpiritMood::Idle), still))
                .into_any_element(),
            ShareFlow::Running(run) => {
                if let Some(share) = self
                    .shares
                    .iter()
                    .find(|share| share.id == run.selection_id)
                    && matches!(share.state, crate::model::ShareState::AwaitingAuthorization)
                {
                    return self
                        .authorization_share(theme, share, cx)
                        .into_any_element();
                }
                if let Some(share) = self
                    .shares
                    .iter()
                    .find(|share| share.id == run.selection_id)
                    && let crate::model::ShareState::AwaitingConfiguration(blocking)
                    | crate::model::ShareState::AwaitingNetwork(blocking) = share.state
                {
                    return self
                        .blocked_share(theme, share, blocking, cx)
                        .into_any_element();
                }
                let now = std::time::Instant::now();
                let frame = run.sample(now, still);
                let animating = active && !still && frame.needs_animation();
                self.motion_probe.observe(run, frame, now, animating);
                if animating {
                    window.request_animation_frame();
                }
                let scene = ShareScene::new(geometry, frame);
                self.running_share(theme, run, scene, layout, cx)
                    .id("share-stage")
                    .into_any_element()
            }
        }
    }

    fn choose_share(&self, theme: Theme, geometry: ShareGeometry, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .items_center()
            .child(geometry.spirit_space())
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

fn preparation_message(
    theme: Theme,
    name: &str,
    visibility: f32,
    locale: Language,
    ready: bool,
) -> Div {
    column()
        .absolute()
        .bottom_0()
        .w_full()
        .items_center()
        .gap(px(8.))
        .opacity(visibility)
        .child(theme.heading(
            locale.text(if ready {
                Text::LinkReady
            } else {
                Text::PreparingShare
            }),
            26.,
        ))
        .child(
            theme
                .label(name.to_owned(), 16.)
                .text_color(theme.muted_text()),
        )
}
