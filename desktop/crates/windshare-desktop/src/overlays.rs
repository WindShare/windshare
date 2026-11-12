use crate::localization::{Text, language};
use crate::{
    desktop::Desktop,
    model::{Action, Overlay},
    motion::{SpiritMood, spirit_scene},
    sharing::{detail_width, enlarged_qr_side},
};
use gpui::{
    Animation, AnimationExt, Div, FocusHandle, MouseButton, Role, Window, ease_out_quint,
    prelude::*, px,
};
use std::time::Duration;
use windshare_ui::{ButtonStyle, Icon, MOTION_MS, Theme, Tone, column, row};

pub const OVERLAY_MARGIN: f32 = 40.;
pub const OVERLAY_PADDING: f32 = 30.;
const DEFAULT_OVERLAY_WIDTH: f32 = 540.;

pub struct OverlayFrame {
    pub content: Overlay,
    pub return_focus: Option<FocusHandle>,
    pub scope: FocusHandle,
    pub sequence: usize,
}

impl Desktop {
    pub fn overlay_view(&self, window: &Window, cx: &Context<Self>) -> impl IntoElement {
        // Keep lower layers mounted: their stable element state owns the focus handles
        // that an enlarged QR must return to. Each layer has its own keyboard scope.
        column()
            .id("overlay-stack")
            .absolute()
            .inset_0()
            .size_full()
            .children(
                self.overlays
                    .iter()
                    .map(|frame| self.overlay_frame(frame, window, cx)),
            )
    }

    fn overlay_frame(
        &self,
        frame: &OverlayFrame,
        window: &Window,
        cx: &Context<Self>,
    ) -> impl IntoElement {
        let locale = language(cx);
        let theme = Theme::from(window.appearance());
        let (title, width, content) = match &frame.content {
            Overlay::Detail(id) => (
                locale.text(Text::ShareDetails),
                detail_width(window),
                self.share_detail(theme, *id, window, cx),
            ),
            Overlay::ShareQr(id) => (
                locale.text(Text::ScanToReceive),
                self.shares
                    .iter()
                    .find(|share| share.id == *id)
                    .map_or(DEFAULT_OVERLAY_WIDTH, |share| {
                        OVERLAY_PADDING.mul_add(2., enlarged_qr_side(share, window))
                    }),
                self.share_qr_detail(theme, *id, window, cx),
            ),
            Overlay::Help => (
                locale.text(Text::About),
                DEFAULT_OVERLAY_WIDTH,
                self.about_preview(theme, cx),
            ),
        };
        let modal = theme
            .card()
            .id(("dialog", frame.sequence))
            .role(Role::Dialog)
            .aria_label(title)
            .w(px(width))
            .max_w(window.viewport_size().width - px(OVERLAY_MARGIN * 2.))
            .max_h(window.viewport_size().height - px(OVERLAY_MARGIN * 2.))
            .p(px(OVERLAY_PADDING))
            .gap(px(26.))
            .overflow_y_scroll()
            .shadow_sm()
            .track_focus(&frame.scope)
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
                            Action::Close,
                            cx,
                        )
                        .tooltip(locale.text(Text::CloseDialog)),
                    ),
            )
            .child(content);
        row()
            .id(("overlay", frame.sequence))
            .absolute()
            .inset_0()
            .size_full()
            .items_center()
            .justify_center()
            .bg(theme.overlay())
            .occlude()
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, _, window, cx| this.dispatch(Action::Close, window, cx)),
            )
            .child(
                modal.with_animation(
                    ("dialog-enter", frame.sequence),
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
                Action::Close,
                cx,
            ))
    }
}
