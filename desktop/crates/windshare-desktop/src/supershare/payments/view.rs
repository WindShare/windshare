use super::{Coin, Payment, artwork, qr};
use crate::{
    localization::{Text, language},
    supershare::{SuperShare, state::Action},
};
use gpui::{ClipboardItem, Context, Div, div, prelude::*, px};
use std::time::Duration;
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, row};

const MARK_SIDE: f32 = 22.;
const COPY_FEEDBACK: Duration = Duration::from_secs(2);

impl SuperShare {
    pub(in crate::supershare) fn payment_tabs(
        theme: Theme,
        payment: Payment,
        busy: bool,
        narrow: bool,
        cx: &Context<Self>,
    ) -> Div {
        let coin = match payment {
            Payment::Crypto(coin) => coin,
            _ => Coin::default(),
        };
        div()
            .grid()
            .grid_cols(if narrow { 2 } else { 4 })
            .gap(px(8.))
            .children(
                [
                    Payment::Alipay,
                    Payment::OkxPay,
                    Payment::BinancePay,
                    Payment::Crypto(coin),
                ]
                .into_iter()
                .enumerate()
                .map(|(index, method)| {
                    Button::new(("super-payment", index), method.label(language(cx)))
                        .leading(artwork::payment(method, theme, MARK_SIDE))
                        .style(if method == payment {
                            ButtonStyle::ActiveNavigation(Tone::Mint)
                        } else {
                            ButtonStyle::Secondary
                        })
                        .full_width()
                        .large()
                        .disabled(busy)
                        .on_press(cx.listener(move |this, (), window, cx| {
                            this.dispatch(Action::SelectPayment(method), window, cx);
                        }))
                }),
            )
    }

    pub(in crate::supershare) fn payment_destination(
        &self,
        theme: Theme,
        payment: Payment,
        busy: bool,
        narrow: bool,
        cx: &Context<Self>,
    ) -> Div {
        let card = div()
            .w_full()
            .p(px(24.))
            .rounded(px(20.))
            .border_1()
            .border_color(theme.border())
            .bg(theme.surface().opacity(0.84));
        match payment {
            Payment::Crypto(coin) => column()
                .gap(px(16.))
                .child(Self::coin_picker(theme, coin, busy, cx))
                .child(
                    card.child(
                        row()
                            .gap(px(28.))
                            .when(narrow, gpui::Styled::flex_col)
                            .child(Self::qr_frame(theme, payment))
                            .child(self.wallet_details(theme, coin, cx)),
                    ),
                ),
            _ => card.child(
                column()
                    .items_center()
                    .gap(px(14.))
                    .child(Self::qr_frame(theme, payment))
                    .child(
                        row()
                            .gap(px(10.))
                            .child(artwork::payment(payment, theme, MARK_SIDE))
                            .child(theme.heading(payment.label(language(cx)), 18.)),
                    ),
            ),
        }
    }

    fn qr_frame(theme: Theme, payment: Payment) -> Div {
        div()
            .flex_shrink_0()
            .rounded(px(12.))
            .overflow_hidden()
            .border_1()
            .border_color(theme.border())
            .child(qr::code(payment))
    }

    fn coin_picker(theme: Theme, selected: Coin, busy: bool, cx: &Context<Self>) -> Div {
        row()
            .flex_wrap()
            .gap(px(8.))
            .children(Coin::ALL.into_iter().map(|coin| {
                Button::new(("super-coin", coin as usize), coin.ticker())
                    .compact()
                    .leading(artwork::coin(coin, MARK_SIDE))
                    .style(if coin == selected {
                        ButtonStyle::ActiveNavigation(Tone::Mint)
                    } else {
                        ButtonStyle::Ghost
                    })
                    .tooltip(coin.wallet().network)
                    .disabled(busy)
                    .on_press(cx.listener(move |this, (), window, cx| {
                        this.dispatch(Action::SelectPayment(Payment::Crypto(coin)), window, cx);
                    }))
            }))
            .border_b_1()
            .border_color(theme.border())
            .pb(px(12.))
    }

    fn wallet_details(&self, theme: Theme, coin: Coin, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let wallet = coin.wallet();
        let copied = self.copied_address == Some(coin);
        column()
            .flex_1()
            .min_w_0()
            .gap(px(20.))
            .child(
                row().gap(px(12.)).child(artwork::coin(coin, 40.)).child(
                    column()
                        .gap(px(5.))
                        .child(theme.heading(wallet.name, 22.))
                        .child(
                            theme
                                .label(wallet.network, 13.)
                                .text_color(theme.accent_text()),
                        ),
                ),
            )
            .child(
                column()
                    .gap(px(8.))
                    .child(theme.caption(locale.text(Text::SuperWalletAddress)))
                    .child(theme.label(wallet.address, 14.).font_family("Consolas")),
            )
            .child(
                row().child(
                    Button::new(
                        "super-copy-address",
                        locale.text(if copied {
                            Text::SuperAddressCopied
                        } else {
                            Text::SuperCopyAddress
                        }),
                    )
                    .icon(if copied { Icon::Check } else { Icon::Copy })
                    .style(ButtonStyle::Tinted(Tone::Mint))
                    .on_press(cx.listener(move |this, (), _, cx| this.copy_address(coin, cx))),
                ),
            )
    }

    fn copy_address(&mut self, coin: Coin, cx: &mut Context<Self>) {
        cx.write_to_clipboard(ClipboardItem::new_string(coin.wallet().address.to_owned()));
        self.copied_address = Some(coin);
        self.sequence += 1;
        let operation = self.sequence;
        tracing::info!(
            event = "supershare.address_copied",
            session = std::process::id(),
            operation,
            ?coin,
            network = coin.wallet().network,
        );
        cx.notify();
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(COPY_FEEDBACK).await;
            let _ = view.update(cx, |this, cx| {
                if this.sequence == operation {
                    this.copied_address = None;
                    cx.notify();
                }
            });
        })
        .detach();
    }
}
