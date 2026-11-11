mod detail;
mod link;
mod link_views;
mod result;

pub use detail::{detail_width, enlarged_qr_side};
pub use link::{LinkDisclosure, ShareLink};

use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    flow_state::{SampleBundle, ShareFlow},
    layout::FlowLayout,
    model::PreviewAction,
    motion::{SpiritMood, scaled_spirit},
    share_animation::{
        SHARE_CHOICE_SECTION_GAP, SHARE_CHOICE_TITLE_HEIGHT, ShareGeometry, ShareScene,
    },
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

const SHARE_CHOICE_BUTTON_MIN_WIDTH: f32 = 160.;

impl Desktop {
    pub fn sharing_page(&mut self, layout: FlowLayout, window: &Window, cx: &Context<Self>) -> Div {
        let theme = Theme::from(window.appearance());
        let geometry = ShareGeometry::new(window, layout);
        let still = self.reduced_motion() || cx.reduce_motion();
        let active = window.is_window_active() && window.is_visible();
        match self.share_flow {
            ShareFlow::Choose => column()
                .h(px(geometry.height))
                .justify_center()
                .child(self.choose_share(theme, layout, still, cx)),
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
                } else {
                    self.preparing_scene(theme, run.bundle, scene, cx)
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
                            PreviewAction::SelectFiles,
                            cx,
                        )
                        .large()
                        .min_width(px(SHARE_CHOICE_BUTTON_MIN_WIDTH))
                        .focus_handle(&self.primary_focus),
                    )
                    .child(
                        Self::button(
                            "choose-folder",
                            locale.text(Text::ChooseFolder),
                            Icon::Folder,
                            ButtonStyle::Secondary,
                            PreviewAction::SelectFolder,
                            cx,
                        )
                        .large()
                        .min_width(px(SHARE_CHOICE_BUTTON_MIN_WIDTH)),
                    ),
            )
    }

    fn preparing_scene(
        &self,
        theme: Theme,
        bundle: SampleBundle,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let visibility = scene.preparation_visibility();
        div()
            .relative()
            .w_full()
            .h(px(scene.geometry.height))
            .child(scene.scenery(self.reduced_motion()))
            .child(preparation_message(
                theme,
                bundle.name(),
                visibility,
                locale,
            ))
            .child(
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
                            PreviewAction::ResetShare,
                            cx,
                        )
                        .compact()
                        .focus_handle(&self.primary_focus),
                    ),
            )
    }
}

fn preparation_message(theme: Theme, name: &str, visibility: f32, locale: Language) -> Div {
    column()
        .absolute()
        .bottom(px(44.))
        .w_full()
        .items_center()
        .gap(px(8.))
        .opacity(visibility)
        .child(theme.heading(locale.text(Text::PreparingShare), 26.))
        .child(
            theme
                .label(name.to_owned(), 16.)
                .text_color(theme.muted_text()),
        )
}
