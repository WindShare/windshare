use crate::{
    desktop::Desktop,
    model::{Filter, PreviewAction, Share, ShareState},
};
use gpui::{Div, Stateful, div, prelude::*, px, rgb};
use windshare_ui::{
    ButtonStyle, Icon, Tone, badge, border, caption, card, column, dot, heading, icon_tile, label,
    muted, row,
};

impl Desktop {
    pub fn share_card(share: &Share, cx: &Context<Self>) -> Stateful<Div> {
        let id = share.id;
        let status = match share.state {
            ShareState::Live { viewers } if viewers > 0 => format!("{viewers} 人正在接收"),
            ShareState::Live { .. } => "等待接收".to_owned(),
            ShareState::Paused => "已暂停".to_owned(),
        };
        card()
            .id(("share-card", id))
            .flex_1()
            .min_w_0()
            .p(px(20.))
            .gap(px(12.))
            .cursor_pointer()
            .hover(|s| s.border_color(share.tone.accent()))
            .on_click(cx.listener(move |this, _, window, cx| {
                this.dispatch(PreviewAction::Detail(id), window, cx);
            }))
            .child(
                row()
                    .justify_between()
                    .child(icon_tile(share.symbol, share.tone, 44.))
                    .child(
                        Self::button(
                            ("share-menu", id),
                            "",
                            Icon::More,
                            ButtonStyle::Ghost,
                            PreviewAction::Detail(id),
                            cx,
                        )
                        .compact()
                        .tooltip("分享详情"),
                    ),
            )
            .child(
                column()
                    .gap(px(5.))
                    .child(heading(share.name.clone(), 18.).truncate())
                    .child(caption(format!("{}   ·   {}", share.detail, share.size))),
            )
            .child(
                row()
                    .justify_between()
                    .pt(px(6.))
                    .border_t_1()
                    .border_color(border())
                    .child(
                        row()
                            .gap(px(6.))
                            .child(dot(if share.live() {
                                Tone::Mint
                            } else {
                                Tone::Neutral
                            }))
                            .child(caption(status)),
                    )
                    .child(
                        Self::button(
                            ("share-copy", id),
                            "",
                            Icon::Link,
                            ButtonStyle::Ghost,
                            PreviewAction::CopyLink(id),
                            cx,
                        )
                        .compact()
                        .tooltip("复制分享链接"),
                    ),
            )
    }
    fn visible_shares(&self, cx: &Context<Self>) -> Vec<&Share> {
        let query = self.search.read(cx).value().to_lowercase();
        self.shares
            .iter()
            .filter(|share| {
                share.name.to_lowercase().contains(&query)
                    && match self.filter {
                        Filter::All => true,
                        Filter::Live => share.live(),
                        Filter::Paused => !share.live(),
                    }
            })
            .collect()
    }
    pub fn shares_page(&self, compact: bool, cx: &Context<Self>) -> Div {
        let shares = self.visible_shares(cx);
        let count = shares.len();
        let columns = if compact { 2 } else { 3 };
        column()
            .gap(px(24.))
            .child(
                row()
                    .justify_between()
                    .gap_4()
                    .child(
                        row()
                            .gap(px(4.))
                            .p(px(4.))
                            .rounded(px(11.))
                            .bg(rgb(0xef_f2_ec))
                            .children(
                                [
                                    (Filter::All, "全部"),
                                    (Filter::Live, "分享中"),
                                    (Filter::Paused, "已暂停"),
                                ]
                                .into_iter()
                                .map(|(filter, title)| {
                                    Self::button(
                                        ("filter", filter as usize),
                                        title,
                                        if filter == Filter::All {
                                            Icon::Grid
                                        } else if filter == Filter::Live {
                                            Icon::Link
                                        } else {
                                            Icon::Pause
                                        },
                                        if filter == self.filter {
                                            ButtonStyle::Secondary
                                        } else {
                                            ButtonStyle::Ghost
                                        },
                                        PreviewAction::Filter(filter),
                                        cx,
                                    )
                                    .compact()
                                }),
                            ),
                    )
                    .child(
                        row()
                            .gap_2()
                            .child(
                                div()
                                    .w(px(if compact { 175. } else { 220. }))
                                    .child(self.search.clone()),
                            )
                            .child(
                                Self::button(
                                    "layout",
                                    "",
                                    if self.grid { Icon::List } else { Icon::Grid },
                                    ButtonStyle::Secondary,
                                    PreviewAction::ToggleGrid,
                                    cx,
                                )
                                .tooltip("切换布局"),
                            ),
                    ),
            )
            .child(
                row()
                    .justify_between()
                    .child(caption(format!("共 {count} 个分享")))
                    .child(caption("最近分享在前")),
            )
            .when(count == 0, |el| el.child(Self::empty_shares(cx)))
            .when(count > 0 && self.grid, |el| {
                el.children(shares.chunks(columns).map(|chunk| {
                    let mut cards = row().gap(px(18.)).items_stretch();
                    for share in chunk {
                        cards = cards.child(Self::share_card(share, cx));
                    }
                    for _ in chunk.len()..columns {
                        cards = cards.child(div().flex_1());
                    }
                    cards
                }))
            })
            .when(count > 0 && !self.grid, |el| {
                el.child(
                    card()
                        .overflow_hidden()
                        .children(shares.iter().map(|share| Self::share_list_row(share, cx))),
                )
            })
    }
    fn share_list_row(share: &Share, cx: &Context<Self>) -> Div {
        row()
            .gap(px(12.))
            .p(px(20.))
            .border_b_1()
            .border_color(border())
            .child(icon_tile(share.symbol, share.tone, 40.))
            .child(
                column()
                    .flex_1()
                    .gap(px(5.))
                    .child(heading(share.name.clone(), 17.))
                    .child(caption(format!("{} · {}", share.detail, share.size))),
            )
            .child(badge(
                if share.live() {
                    "分享中"
                } else {
                    "已暂停"
                },
                if share.live() {
                    Tone::Mint
                } else {
                    Tone::Neutral
                },
            ))
            .child(
                Self::button(
                    ("list-link", share.id),
                    "复制链接",
                    Icon::Link,
                    ButtonStyle::Secondary,
                    PreviewAction::CopyLink(share.id),
                    cx,
                )
                .compact(),
            )
            .child(
                Self::button(
                    ("list-detail", share.id),
                    "",
                    Icon::Chevron,
                    ButtonStyle::Ghost,
                    PreviewAction::Detail(share.id),
                    cx,
                )
                .compact()
                .tooltip("分享详情"),
            )
    }
    fn empty_shares(cx: &Context<Self>) -> Div {
        card()
            .h(px(330.))
            .items_center()
            .justify_center()
            .gap(px(12.))
            .child(icon_tile(Icon::Search, Tone::Lavender, 58.))
            .child(heading("没有找到这个分享", 22.))
            .child(label("换个关键词，或创建一个新分享。", 16.).text_color(muted()))
            .child(Self::button(
                "empty-new",
                "新建分享",
                Icon::Plus,
                ButtonStyle::Primary,
                PreviewAction::NewShare,
                cx,
            ))
    }
}
