use crate::{
    desktop::Desktop,
    model::{PreviewAction, Transfer, TransferState},
};
use gpui::{Div, prelude::*, px};
use windshare_ui::{ButtonStyle, FONT, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn transfers_page(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let active = self
            .transfers
            .iter()
            .filter(|item| item.state == TransferState::Running)
            .count();
        theme
            .card()
            .p(px(28.))
            .gap(px(24.))
            .child(theme.section_header("文件传输", &format!("{active} 项进行中")))
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
                                    .child(theme.caption(transfer.detail)),
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
                                "已完成"
                            } else if paused {
                                "已暂停"
                            } else {
                                "正在传输"
                            }))
                            .when(self.settings[1] && !paused && !complete, |el| {
                                el.child(theme.caption(format!(
                                    "{:.1} MB/s · 剩余 {} 秒",
                                    transfer.rate_mbps, transfer.remaining_seconds
                                )))
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
                    PreviewAction::ToggleTransfer(index),
                    cx,
                )
                .disabled(complete)
                .tooltip(if complete {
                    "传输完成"
                } else if paused {
                    "继续传输"
                } else {
                    "暂停传输"
                }),
            )
    }
}
