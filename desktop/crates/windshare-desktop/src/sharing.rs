use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    flow_state::{CopyFeedback, SampleBundle, ShareFlow, ShareRun},
    layout::FlowLayout,
    model::PreviewAction,
    motion::{SpiritMood, reassurance, scaled_spirit},
    share_animation::{
        SHARE_CHOICE_SECTION_GAP, SHARE_CHOICE_TITLE_HEIGHT, SHARE_LINK_HEIGHT, ShareGeometry,
        ShareScene,
    },
    share_timing::lerp,
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{
    Button, ButtonStyle, FONT, Icon, Theme, Tone, column, icon, rounded_surface, row,
};

const SHARE_CHOICE_BUTTON_MIN_WIDTH: f32 = 160.;

impl Desktop {
    pub fn sharing_page(&mut self, layout: FlowLayout, window: &Window, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let theme = Theme::from(window.appearance());
        let geometry = ShareGeometry::new(window, layout);
        let still = self.reduced_motion() || cx.reduce_motion();
        let active = window.is_window_active();
        let content = match self.share_flow {
            ShareFlow::Choose => column()
                .h(px(geometry.height))
                .justify_center()
                .child(self.choose_share(theme, layout, still, cx)),
            ShareFlow::Running(run) => {
                let now = std::time::Instant::now();
                let frame = run.sample(now, still);
                self.motion_probe.observe(run, frame, now, active && !still);
                if active && !still {
                    window.request_animation_frame();
                }
                let scene = ShareScene::new(geometry, frame);
                if still && run.share_id().is_none() {
                    self.quiet_preparation(theme, run.bundle, layout, cx)
                } else if scene.landing() {
                    self.result_share(theme, run, scene, cx)
                } else {
                    self.preparing_scene(theme, run.bundle, scene, cx)
                }
            }
        };
        column()
            .gap(px(22.))
            .child(
                theme
                    .card()
                    .rounded(px(24.))
                    .p(px(layout.panel_padding()))
                    .overflow_hidden()
                    .child(content),
            )
            .child(
                row()
                    .justify_center()
                    .flex_wrap()
                    .gap(px(22.))
                    .child(reassurance(
                        theme,
                        Icon::Shield,
                        locale.text(Text::Encrypted),
                    ))
                    .child(reassurance(
                        theme,
                        Icon::Globe,
                        locale.text(Text::NoInstall),
                    ))
                    .child(reassurance(
                        theme,
                        Icon::Monitor,
                        locale.text(Text::FilesStayLocal),
                    )),
            )
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
            .child(scene.scenery(theme, self.reduced_motion()))
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

    fn quiet_preparation(
        &self,
        theme: Theme,
        bundle: SampleBundle,
        layout: FlowLayout,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        column()
            .h(px(layout.stage_height()))
            .items_center()
            .justify_center()
            .gap(px(16.))
            .child(scaled_spirit(
                "share-preparing-spirit",
                SpiritMood::Sending,
                Tone::Mint,
                layout.scene_scale(),
                true,
            ))
            .child(theme.heading(locale.text(Text::PreparingShare), 26.))
            .child(
                theme
                    .label(bundle.name(), 16.)
                    .text_color(theme.muted_text()),
            )
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
            )
    }

    fn result_share(
        &self,
        theme: Theme,
        run: ShareRun,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let g = scene.geometry;
        let title = scene.title_visibility();
        let summary = scene.summary_visibility();
        let footer = scene.footer_visibility();
        let result = scene.frame.result_visibility;
        let pending = run.share_id().is_none();
        let bundle = run.bundle;
        // Small layout translations snap text to whole pixels. Keep typography and
        // hit targets settled while opacity and decorative geometry carry the reveal.
        div()
            .relative()
            .w_full()
            .h(px(g.height))
            .child(scene.scenery(theme, self.reduced_motion()))
            .child(
                column()
                    .absolute()
                    .left(px(g.content_left))
                    .top(px(g.content_top + 18.))
                    .w(px(g.content_width - 196.))
                    .h(px(72.))
                    .opacity(title)
                    .when(result < 1., |el| {
                        el.child(result_title(
                            theme,
                            locale.text(Text::PreparingLink),
                            locale.text(Text::PreparingLinkDetail),
                            1. - result,
                        ))
                    })
                    .when(result > 0., |el| {
                        el.child(result_title(
                            theme,
                            locale.text(Text::LinkReady),
                            locale.text(Text::LinkReadyDetail),
                            result,
                        ))
                    }),
            )
            .child(self.link_reveal(theme, run, scene, cx))
            .child(
                row()
                    .absolute()
                    .left(px(g.content_left))
                    .top(px(g.content_top + 212.))
                    .w(px(g.content_width))
                    .h(px(62.))
                    .gap(px(12.))
                    .p(px(12.))
                    .rounded(px(12.))
                    .border_1()
                    .border_color(theme.border())
                    .bg(theme.surface())
                    .opacity(summary)
                    .child(theme.icon_tile(bundle.icon(), bundle.tone(), 38.))
                    .child(theme.label(bundle.name(), 16.).flex_1())
                    .child(theme.caption(format!(
                        "{} · {}",
                        bundle.contents().description(locale),
                        bundle.size()
                    ))),
            )
            .child(
                row()
                    .absolute()
                    .top(px(g.content_top + 286.))
                    .w_full()
                    .justify_center()
                    .opacity(footer)
                    .child(reassurance(
                        theme,
                        Icon::Monitor,
                        locale.text(Text::KeepOpenToReceive),
                    )),
            )
            .child(
                row()
                    .absolute()
                    .top(px(g.content_top + 316.))
                    .w_full()
                    .justify_center()
                    .opacity(footer)
                    .child(
                        Self::button(
                            "share-again",
                            if pending {
                                locale.text(Text::Cancel)
                            } else {
                                locale.text(Text::ShareAgain)
                            },
                            if pending { Icon::Close } else { Icon::Plus },
                            ButtonStyle::Ghost,
                            PreviewAction::ResetShare,
                            cx,
                        )
                        .compact()
                        .when(pending, |button| button.focus_handle(&self.primary_focus)),
                    ),
            )
    }

    fn link_reveal(
        &self,
        theme: Theme,
        run: ShareRun,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let g = scene.geometry;
        let expansion = scene.link_expansion();
        let result = scene.frame.result_visibility;
        let share = run
            .share_id()
            .and_then(|id| self.shares.iter().find(|share| share.id == id));
        let copied = matches!(self.copy_feedback, CopyFeedback::Copied { share_id, .. }
            if Some(share_id) == run.share_id());
        let enabled = share.is_some() && scene.control_visibility() > 0.95;
        let control = Button::new(
            "ready-copy",
            if share.is_none() {
                locale.text(Text::WaitingLink)
            } else if copied {
                locale.text(Text::Copied)
            } else {
                locale.text(Text::CopyLink)
            },
        )
        .icon(if copied { Icon::Check } else { Icon::Copy })
        .style(ButtonStyle::Primary)
        .large()
        .min_width(px(128.))
        .focus_handle(&self.primary_focus)
        .disabled(!enabled);
        let control = if let Some(share) = share {
            let id = share.id;
            control.on_press(cx.listener(move |this, (), window, cx| {
                this.dispatch(PreviewAction::CopyLink(id), window, cx);
            }))
        } else {
            control
        };
        div()
            .absolute()
            .left(px(g.content_left))
            .top(px(g.link_top()))
            .w(px(g.content_width))
            .h(px(SHARE_LINK_HEIGHT))
            .child(
                rounded_surface(
                    gpui::size(
                        px(lerp(60., g.content_width, expansion)),
                        px(SHARE_LINK_HEIGHT),
                    ),
                    px(15.),
                    Tone::Mint.surface(theme),
                )
                .opacity(scene.control_visibility()),
            )
            .child(
                row()
                    .size_full()
                    .gap(px(12.))
                    .p(px(12.))
                    .pl(px(18.))
                    .child(
                        icon(Icon::Link, 21.)
                            .text_color(theme.accent_text())
                            .opacity(scene.link_visibility()),
                    )
                    .child(
                        div()
                            .relative()
                            .h(px(24.))
                            .flex_1()
                            .min_w_0()
                            .opacity(scene.link_visibility())
                            .when(result < 1., |el| el.child(link_placeholder(theme, scene)))
                            .when_some(share, |el, share| {
                                el.child(
                                    theme
                                        .label(share.link(), 16.)
                                        .absolute()
                                        .inset_0()
                                        .w_full()
                                        .font_family(FONT)
                                        .truncate()
                                        .opacity(result),
                                )
                            }),
                    )
                    .child(div().opacity(scene.control_visibility()).child(control)),
            )
    }
}

fn result_title(theme: Theme, title: &'static str, subtitle: &'static str, visibility: f32) -> Div {
    column()
        .absolute()
        .inset_0()
        .gap(px(8.))
        .opacity(visibility)
        .child(theme.heading(title, 26.))
        .child(theme.label(subtitle, 16.).text_color(theme.muted_text()))
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

fn link_placeholder(theme: Theme, scene: ShareScene) -> Div {
    let result = scene.frame.result_visibility;
    let breath_variation = 0.15 * scene.frame.breath;
    let placeholder_opacity = (1. - result) * (0.65 + breath_variation);
    row()
        .absolute()
        .inset_0()
        .gap(px(8.))
        .opacity(placeholder_opacity)
        .child(
            div()
                .w(px(138.))
                .h(px(8.))
                .rounded_full()
                .bg(Tone::Mint.accent(theme).opacity(0.35)),
        )
        .child(
            div()
                .w(px(44.))
                .h(px(8.))
                .rounded_full()
                .bg(Tone::Mint.accent(theme).opacity(0.20)),
        )
}
