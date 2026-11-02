use crate::localization::{Text, language};
use crate::{
    desktop::Desktop,
    model::{Overlay, PreviewAction},
    motion::{SpiritMood, spirit_scene},
};
use gpui::{
    Animation, AnimationExt, Div, MouseButton, Role, Window, ease_out_quint, prelude::*, px,
};
use std::time::Duration;
use windshare_ui::{ButtonStyle, FONT, Icon, MOTION_MS, Theme, Tone, column, row};

impl Desktop {
    pub fn overlay_view(&self, window: &Window, cx: &Context<Self>) -> impl IntoElement {
        let locale = language(cx);
        let theme = Theme::from(window.appearance());
        let overlay = self
            .overlay
            .clone()
            .expect("overlay is only rendered when open");
        let (title, content) = match overlay {
            Overlay::Detail(id) => (
                locale.text(Text::ShareDetails),
                self.share_detail(theme, id, cx),
            ),
            Overlay::Help => (locale.text(Text::About), self.about_preview(theme, cx)),
        };
        let modal = theme
            .card()
            .id("dialog")
            .role(Role::Dialog)
            .aria_label(title)
            .w(px(540.))
            .max_h(window.viewport_size().height - px(80.))
            .p(px(30.))
            .gap(px(26.))
            .overflow_y_scroll()
            .shadow_sm()
            .track_focus(&self.modal_focus)
            .tab_group()
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child(
                row()
                    .justify_between()
                    .gap(px(20.))
                    .child(theme.heading(title, 26.))
                    .child(
                        Self::button(
                            "dialog-close",
                            "",
                            Icon::Close,
                            ButtonStyle::Ghost,
                            PreviewAction::Close,
                            cx,
                        )
                        .tooltip(locale.text(Text::CloseDialog)),
                    ),
            )
            .child(content);
        row()
            .id("overlay")
            .absolute()
            .inset_0()
            .size_full()
            .items_center()
            .justify_center()
            .bg(theme.overlay())
            .occlude()
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, _, window, cx| this.dispatch(PreviewAction::Close, window, cx)),
            )
            .child(
                modal.with_animation(
                    ("dialog-enter", self.dialog_sequence),
                    Animation::new(Duration::from_millis(if self.reduced_motion() {
                        1
                    } else {
                        MOTION_MS
                    }))
                    .with_easing(ease_out_quint()),
                    gpui::Styled::opacity,
                ),
            )
    }

    fn share_detail(&self, theme: Theme, id: usize, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return column();
        };
        column()
            .gap(px(24.))
            .child(
                row()
                    .gap(px(16.))
                    .child(theme.icon_tile(share.symbol, share.tone, 58.))
                    .child(
                        column()
                            .gap(px(7.))
                            .child(theme.heading(share.name.clone(), 22.))
                            .child(theme.caption(format!(
                                "{} · {}",
                                share.contents.description(locale),
                                share.size
                            ))),
                    ),
            )
            .child(
                row()
                    .justify_between()
                    .child(theme.caption(locale.created(&share.age.description(locale))))
                    .child(theme.badge(
                        if share.live() {
                            locale.text(Text::SharingActive)
                        } else {
                            locale.text(Text::Paused)
                        },
                        if share.live() {
                            Tone::Mint
                        } else {
                            Tone::Neutral
                        },
                    )),
            )
            .child(
                column()
                    .gap(px(12.))
                    .child(theme.label(locale.text(Text::ShareLink), 16.))
                    .child(
                        theme
                            .label(share.link(), 15.)
                            .font_family(FONT)
                            .p(px(16.))
                            .rounded(px(12.))
                            .bg(Tone::Mint.surface(theme)),
                    )
                    .child(
                        Self::button(
                            "detail-copy",
                            locale.text(Text::CopyShareLink),
                            Icon::Copy,
                            ButtonStyle::Primary,
                            PreviewAction::CopyLink(id),
                            cx,
                        )
                        .large(),
                    ),
            )
            .child(theme.caption(locale.text(Text::KeepOpen)))
            .child(Self::button(
                "detail-toggle",
                if share.live() {
                    locale.text(Text::PauseShare)
                } else {
                    locale.text(Text::ResumeShare)
                },
                if share.live() {
                    Icon::Pause
                } else {
                    Icon::Play
                },
                ButtonStyle::Secondary,
                PreviewAction::ToggleShare(id),
                cx,
            ))
    }

    fn about_preview(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .gap(px(20.))
            .items_center()
            .child(spirit_scene(
                "about-spirit",
                SpiritMood::Happy,
                Tone::Mint,
                self.reduced_motion(),
            ))
            .child(theme.heading(locale.text(Text::AboutTagline), 24.))
            .child(
                theme
                    .label(locale.text(Text::AboutDescription), 16.)
                    .text_color(theme.muted_text())
                    .line_height(px(28.))
                    .text_center(),
            )
            .child(theme.separator())
            .child(
                theme
                    .caption(locale.text(Text::AboutPreview))
                    .text_center()
                    .line_height(px(24.)),
            )
            .child(Self::button(
                "about-done",
                locale.text(Text::GotIt),
                Icon::Check,
                ButtonStyle::Primary,
                PreviewAction::Close,
                cx,
            ))
    }
}
