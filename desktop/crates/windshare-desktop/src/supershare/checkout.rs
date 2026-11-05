use super::{
    SuperShare,
    state::{Action, Coin, Confirmation, Payment, Plan},
    visuals,
};
use crate::localization::{Text, language};
use gpui::{Context, Div, div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

impl SuperShare {
    pub(super) fn checkout_page(
        &self,
        theme: Theme,
        plan: Plan,
        payment: Payment,
        confirmation: Confirmation,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        column()
            .w_full()
            .max_w(px(720.))
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
                    .child(Self::payment_tabs(payment, cx))
                    .child(Self::payment_destination(theme, payment, cx))
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

    fn payment_tabs(payment: Payment, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let coin = if let Payment::Crypto(coin) = payment {
            coin
        } else {
            Coin::UsdtTrc20
        };
        row().gap(px(10.)).children(
            [
                (
                    "super-alipay",
                    locale.text(Text::SuperAlipay),
                    Payment::Alipay,
                ),
                ("super-paypal", "PayPal", Payment::PayPal),
                (
                    "super-crypto",
                    locale.text(Text::SuperCrypto),
                    Payment::Crypto(coin),
                ),
            ]
            .into_iter()
            .map(|(id, label, method)| {
                div().flex_1().child(
                    Self::button(id, label, Action::SelectPayment(method), cx)
                        .style(if method == payment {
                            ButtonStyle::ActiveNavigation(Tone::Mint)
                        } else {
                            ButtonStyle::Ghost
                        })
                        .full_width()
                        .align_start(),
                )
            }),
        )
    }

    fn payment_destination(theme: Theme, payment: Payment, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let (symbol, label, tone) = match payment {
            Payment::Alipay => (Icon::Grid, Text::SuperQrPending, Tone::Blue),
            Payment::PayPal => (Icon::External, Text::SuperAccountPending, Tone::Blue),
            Payment::Crypto(_) => (Icon::Copy, Text::SuperAddressPending, Tone::Mint),
        };
        column()
            .gap(px(18.))
            .when(matches!(payment, Payment::Crypto(_)), |el| {
                el.child(Self::coin_picker(payment, cx))
            })
            .child(
                column()
                    .h(px(204.))
                    .items_center()
                    .justify_center()
                    .gap(px(18.))
                    .rounded(px(16.))
                    .bg(theme.surface().opacity(0.35))
                    .child(
                        row()
                            .size(px(72.))
                            .justify_center()
                            .rounded(px(20.))
                            .border_1()
                            .border_color(theme.border())
                            .bg(theme.surface())
                            .child(icon(symbol, 30.).text_color(tone.text(theme))),
                    )
                    .child(
                        theme
                            .label(locale.text(label), 15.)
                            .text_color(theme.muted_text()),
                    ),
            )
    }

    fn coin_picker(payment: Payment, cx: &Context<Self>) -> Div {
        row()
            .flex_wrap()
            .gap(px(8.))
            .children(Coin::ALL.into_iter().map(|coin| {
                windshare_ui::Button::new(("super-coin", coin as usize), coin.label())
                    .compact()
                    .style(if payment == Payment::Crypto(coin) {
                        ButtonStyle::ActiveNavigation(Tone::Mint)
                    } else {
                        ButtonStyle::Secondary
                    })
                    .on_press(cx.listener(move |this, (), window, cx| {
                        this.dispatch(Action::SelectPayment(Payment::Crypto(coin)), window, cx);
                    }))
            }))
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
