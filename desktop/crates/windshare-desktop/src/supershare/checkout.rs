use super::{
    SuperShare,
    layout::Stage,
    payments::Payment,
    state::{Action, Confirmation, Plan},
    visuals,
};
use crate::localization::{Text, language};
use gpui::{Context, Div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

const CHECKOUT_MAX_WIDTH: f32 = 760.;
const STACKED_PAYMENT_WIDTH: f32 = 640.;

impl SuperShare {
    pub(super) fn checkout_page(
        &self,
        theme: Theme,
        stage: Stage,
        plan: Plan,
        payment: Payment,
        confirmation: Confirmation,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        column()
            .w_full()
            .max_w(px(CHECKOUT_MAX_WIDTH))
            .mx_auto()
            .gap(px(22.))
            .child(Self::page_navigation(theme, cx))
            .child(
                column()
                    .gap(px(28.))
                    .child(
                        row()
                            .justify_between()
                            .gap(px(20.))
                            .child(
                                column()
                                    .gap(px(6.))
                                    .child(theme.heading(locale.text(Text::SuperPayment), 28.))
                                    .child(theme.label(locale.text(plan.label()), 16.)),
                            )
                            .child(visuals::price(theme, plan.price(), true)),
                    )
                    .child(theme.separator())
                    .child(Self::payment_tabs(
                        theme,
                        payment,
                        confirmation.busy(),
                        stage.content_width < STACKED_PAYMENT_WIDTH,
                        cx,
                    ))
                    .child(self.payment_destination(
                        theme,
                        payment,
                        confirmation.busy(),
                        stage.content_width < STACKED_PAYMENT_WIDTH,
                        cx,
                    ))
                    .child(self.confirmation_view(theme, confirmation, cx)),
            )
    }

    pub(super) fn page_navigation(theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        row()
            .justify_between()
            .child(
                Self::button("super-back", locale.text(Text::SuperBack), Action::Back, cx)
                    .style(ButtonStyle::Quiet)
                    .compact(),
            )
            .child(
                row()
                    .gap(px(8.))
                    .child(icon(Icon::Spark, 16.).text_color(visuals::Finish::new(theme).foil))
                    .child(
                        theme
                            .heading("SuperShare", 18.)
                            .font_family(windshare_ui::FONT),
                    ),
            )
    }

    fn confirmation_view(
        &self,
        theme: Theme,
        confirmation: Confirmation,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let label = match confirmation {
            Confirmation::Ready => Text::SuperPaid,
            Confirmation::Checking { .. } => Text::SuperConfirming,
            Confirmation::Unconfirmed => Text::SuperRetry,
        };
        column()
            .gap(px(14.))
            .child(
                column()
                    .min_h(px(34.))
                    .items_center()
                    .justify_center()
                    .gap(px(9.))
                    .when(confirmation.busy(), |el| {
                        el.child(visuals::progress(theme, self.reduced_motion))
                    })
                    .when(confirmation == Confirmation::Unconfirmed, |el| {
                        el.child(
                            row()
                                .gap(px(8.))
                                .child(icon(Icon::Refresh, 17.).text_color(Tone::Peach.text(theme)))
                                .child(
                                    theme
                                        .label(locale.text(Text::SuperUnconfirmed), 15.)
                                        .text_color(Tone::Peach.text(theme)),
                                ),
                        )
                    }),
            )
            .child(
                Self::button("super-confirm", locale.text(label), Action::Confirm, cx)
                    .style(ButtonStyle::Contrast)
                    .large()
                    .disabled(confirmation.busy())
                    .focus_handle(&self.primary_focus),
            )
    }
}
