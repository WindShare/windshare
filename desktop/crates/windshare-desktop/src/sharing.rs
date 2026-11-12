mod detail;
mod link;
mod link_views;
mod preview;
mod result;

pub use detail::{detail_width, enlarged_qr_side};
pub use link::{LinkDisclosure, ShareLink};

use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareRun},
    layout::FlowLayout,
    model::Action,
    motion::{SpiritMood, scaled_spirit},
    share_animation::{
        SHARE_CHOICE_SECTION_GAP, SHARE_CHOICE_TITLE_HEIGHT, ShareGeometry, ShareScene,
    },
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

const SHARE_CHOICE_BUTTON_MIN_WIDTH: f32 = 160.;

impl Desktop {
    pub fn sharing_page(
        &mut self,
        layout: FlowLayout,
        window: &Window,
        cx: &Context<Self>,
    ) -> gpui::AnyElement {
        let theme = Theme::from(window.appearance());
        let geometry = ShareGeometry::new(window, layout);
        let still = self.reduced_motion() || cx.reduce_motion();
        let active = window.is_window_active() && window.is_visible();
        match self.share_flow {
            ShareFlow::Choose => column()
                .id("share-drop-zone")
                .drag_over::<gpui::ExternalPaths>(move |style, _, _, _| {
                    style.bg(Tone::Mint.surface(theme)).rounded(px(24.))
                })
                .h(px(geometry.height))
                .justify_center()
                .child(self.choose_share(theme, layout, still, cx))
                .into_any_element(),
            ShareFlow::Running(run) => {
                let now = std::time::Instant::now();
                let frame = run.sample(now, still);
                let animating = active && !still && frame.needs_animation();
                self.motion_probe.observe(run, frame, now, animating);
                // Pending work needs visible activity even after the introduction.
                // Completed results wake only for user input or feedback deadlines.
                if animating {
                    window.request_animation_frame();
                }
                let scene = ShareScene::new(geometry, frame);
                if scene.landing() {
                    self.result_share(theme, run, scene, layout, cx)
                        .into_any_element()
                } else {
                    self.preparing_scene(theme, run, scene, cx)
                        .into_any_element()
                }
            }
        }
    }

    fn choose_share(
        &self,
        theme: Theme,
        layout: FlowLayout,
        still: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        column()
            .items_center()
            .gap(px(SHARE_CHOICE_SECTION_GAP))
            .child(scaled_spirit(
                "share-choose-spirit",
                SpiritMood::Idle,
                Tone::Mint,
                layout.scene_scale(),
                still,
            ))
            .child(
                theme
                    .heading(locale.text(Text::ChooseShare), 26.)
                    .line_height(px(SHARE_CHOICE_TITLE_HEIGHT)),
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

    fn preparing_scene(
        &self,
        theme: Theme,
        run: ShareRun,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let visibility = scene.preparation_visibility();
        let Some(share) = self
            .shares
            .iter()
            .find(|share| share.id == run.selection_id)
        else {
            return column();
        };
        let ready = share.usable_link().is_some();
        div()
            .relative()
            .w_full()
            .h(px(scene.geometry.height))
            .child(scene.scenery(self.reduced_motion()))
            .child(preparation_message(
                theme,
                &share.name,
                visibility,
                locale,
                ready,
            ))
            .when(ready, |el| el.child(self.early_link(share, cx)))
            .when(!ready, |el| {
                el.child(
                    row()
                        .absolute()
                        .bottom_0()
                        .w_full()
                        .justify_center()
                        .opacity(visibility)
                        .child(
                            Self::button(
                                "cancel-share",
                                locale.text(Text::Cancel),
                                Icon::Close,
                                ButtonStyle::Ghost,
                                Action::ResetShare,
                                cx,
                            )
                            .compact()
                            .focus_handle(&self.primary_focus),
                        ),
                )
            })
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
        .bottom(px(44.))
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
