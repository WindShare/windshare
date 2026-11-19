mod detail;
mod diagnostics;
mod intro;
mod link;
mod link_views;
mod preview;
mod qr;
mod qr_slot;
mod result;

pub use detail::{detail_width, enlarged_qr_side};
pub use intro::{IntroAnchors, Perch};
pub use link::{LinkDisclosure, LinkKind, ShareLink, SharePublication};

use crate::{
    desktop::Desktop,
    flow_state::ShareFlow,
    layout::FlowLayout,
    localization::{Text, language},
    model::{Action, ShareState},
    motion::{SpiritMood, SpiritScene, scaled_spirit},
};
use gpui::{Div, Window, canvas, prelude::*, px};
use std::rc::Rc;
use windshare_ui::{Button, ButtonStyle, Icon, LARGE_BUTTON_HEIGHT, Theme, Tone, column, row};

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
                    ShareState::AwaitingAuthorization => self
                        .authorization_share(theme, share, cx)
                        .into_any_element(),
                    ShareState::AwaitingConfiguration(blocking)
                    | ShareState::AwaitingNetwork(blocking) => self
                        .blocked_share(theme, share, blocking, cx)
                        .into_any_element(),
                    _ => self
                        .share_result(theme, share, layout, Self::share_intro(run, cx), window, cx)
                        .id("share-stage")
                        .into_any_element(),
                }
            }
        }
    }

    fn choose_share(&self, theme: Theme, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let scene_scale = layout.scene_scale();
        let perch = Rc::clone(&self.intro_anchors.perch);
        let spirit = Rc::clone(&self.intro_anchors.spirit);
        column()
            .relative()
            .items_center()
            .child(
                scaled_spirit(
                    "share-spirit",
                    SpiritMood::Idle,
                    Tone::Mint,
                    scene_scale,
                    cx.reduce_motion(),
                )
                .anchor(&spirit),
            )
            .child(self.share_prompt(theme, true, cx))
            .child(
                canvas(
                    move |prompt, _, _| {
                        perch.set(spirit.get().map(|spirit| Perch {
                            prompt,
                            scene_scale,
                            spirit,
                        }));
                    },
                    |_, (), _, _| {},
                )
                .absolute()
                .inset_0(),
            )
    }

    /// The choose page's heading and actions. A copy that is not `live` keeps
    /// their look but answers nothing: it fades out of a run that has begun.
    fn share_prompt(&self, theme: Theme, live: bool, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let choice = |id: &'static str, text, symbol, style, action| {
            let label = locale.text(text);
            let button = if live {
                Self::button(id, label, symbol, style, action, cx)
                    .disabled(self.runtime.selection_disabled())
            } else {
                Button::new(id, label).icon(symbol).style(style)
            };
            button.large().min_width(px(SHARE_CHOICE_BUTTON_MIN_WIDTH))
        };
        let files = choice(
            "choose-files",
            Text::ChooseFiles,
            Icon::Plus,
            ButtonStyle::Primary,
            Action::SelectFiles,
        );
        let folder = choice(
            "choose-folder",
            Text::ChooseFolder,
            Icon::Folder,
            ButtonStyle::Secondary,
            Action::SelectFolder,
        );
        column()
            .items_center()
            .child(
                theme
                    .heading(locale.text(Text::ChooseShare), 26.)
                    .line_height(px(SHARE_CHOICE_TITLE_HEIGHT))
                    .mb(px(SHARE_CHOICE_SECTION_GAP)),
            )
            .child(
                row()
                    .gap(px(12.))
                    .child(if live {
                        files.focus_handle(&self.primary_focus)
                    } else {
                        files
                    })
                    .child(folder),
            )
    }
}
