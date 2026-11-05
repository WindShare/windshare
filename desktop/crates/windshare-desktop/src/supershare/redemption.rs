use super::{
    SuperShare,
    state::{Action, Redemption},
    visuals,
};
use crate::localization::{Text, language};
use gpui::{Context, Div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

impl SuperShare {
    pub(super) fn redemption_page(
        &self,
        theme: Theme,
        status: Redemption,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let busy = matches!(status, Redemption::Checking { .. });
        column()
            .w_full()
            .max_w(px(640.))
            .mx_auto()
            .gap(px(24.))
            .child(Self::page_navigation(theme, cx))
            .child(
                column()
                    .py(px(36.))
                    .gap(px(26.))
                    .items_center()
                    .child(visuals::brand_mark(84.))
                    .child(theme.heading(locale.text(Text::SuperRedeemLink), 30.))
                    .child(
                        column()
                            .w_full()
                            .gap(px(18.))
                            .child(self.code.clone())
                            .child(
                                Self::button(
                                    "super-redeem",
                                    locale.text(if busy {
                                        Text::SuperRedeeming
                                    } else {
                                        Text::SuperRedeem
                                    }),
                                    Action::SubmitCode,
                                    cx,
                                )
                                .style(ButtonStyle::Contrast)
                                .large()
                                .disabled(busy || self.code.read(cx).value().trim().is_empty())
                                .focus_handle(&self.primary_focus),
                            )
                            .child(self.redemption_feedback(theme, status, cx)),
                    ),
            )
    }

    fn redemption_feedback(&self, theme: Theme, status: Redemption, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let message = match status {
            Redemption::Invalid => Some(Text::SuperCodeInvalid),
            Redemption::Unavailable => Some(Text::SuperCodeUnavailable),
            _ => None,
        };
        column()
            .h(px(32.))
            .items_center()
            .justify_center()
            .when(matches!(status, Redemption::Checking { .. }), |el| {
                el.child(visuals::progress(theme, self.reduced_motion))
            })
            .when_some(message, |el, text| {
                el.child(
                    row()
                        .gap(px(8.))
                        .child(icon(Icon::Refresh, 16.).text_color(Tone::Peach.text(theme)))
                        .child(
                            theme
                                .label(locale.text(text), 15.)
                                .text_color(Tone::Peach.text(theme)),
                        ),
                )
            })
    }
}
