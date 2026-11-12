use crate::localization::{Text, language};
use crate::{
    desktop::Desktop,
    model::{Action, Transfer, TransferState},
};
use gpui::{Div, prelude::*, px};
use windshare_ui::{ButtonStyle, FONT, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn transfers_page(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let active = self
            .transfers
            .iter()
            .filter(|item| item.state == TransferState::Running)
            .count();
        theme
            .card()
            .p(px(28.))
            .gap(px(24.))
            .child(theme.section_header(
                locale.text(Text::FileTransfers),
                &locale.active_transfers(active),
            ))
            .children(self.transfers.iter().enumerate().map(|(index, transfer)| {
                column()
                    .gap(px(24.))
                    .child(self.transfer_row(theme, index, transfer, cx))
                    .when(index + 1 < self.transfers.len(), |el| {
                        el.child(theme.separator())
                    })
            }))
    }

    fn transfer_row(
        &self,
        theme: Theme,
        index: usize,
        transfer: &Transfer,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let paused = transfer.state == TransferState::Paused;
        let complete = transfer.state == TransferState::Complete;
        row()
            .gap(px(18.))
            .child(theme.icon_tile(transfer.symbol, transfer.tone, 48.))
            .child(
                column()
                    .flex_1()
                    .min_w_0()
                    .gap(px(12.))
                    .child(
                        row()
                            .justify_between()
                            .gap(px(12.))
                            .child(
                                column()
                                    .gap(px(6.))
                                    .child(theme.heading(transfer.name, 17.))
                                    .child(theme.caption(transfer.detail(locale))),
                            )
                            .child(
                                theme
                                    .label(
                                        format!("{:.0}%", (transfer.progress * 100.).trunc()),
                                        15.,
                                    )
                                    .font_family(FONT)
                                    .text_color(transfer.tone.text(theme)),
                            ),
                    )
                    .child(theme.progress(
                        transfer.progress,
                        if paused { Tone::Neutral } else { transfer.tone },
                    ))
                    .child(
                        row()
                            .justify_between()
                            .child(theme.caption(if complete {
                                locale.text(Text::Complete)
                            } else if paused {
                                locale.text(Text::Paused)
                            } else {
                                locale.text(Text::Transferring)
                            }))
                            .when(self.settings[1] && !paused && !complete, |el| {
                                el.child(theme.caption(
                                    locale.transfer_rate(
                                        transfer.rate_mbps,
                                        transfer.remaining_seconds,
                                    ),
                                ))
                            }),
                    ),
            )
            .child(
                Self::button(
                    ("transfer-toggle", index),
                    "",
                    if complete {
                        Icon::Check
                    } else if paused {
                        Icon::Play
                    } else {
                        Icon::Pause
                    },
                    ButtonStyle::Ghost,
                    Action::ToggleTransfer(index),
                    cx,
                )
                .disabled(complete)
                .tooltip(if complete {
                    locale.text(Text::TransferComplete)
                } else if paused {
                    locale.text(Text::ResumeTransfer)
                } else {
                    locale.text(Text::PauseTransfer)
                }),
            )
    }
}
