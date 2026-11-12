use crate::localization::{Text, language};
use crate::{
    desktop::Desktop,
    model::{Action, Filter, Share},
};
use gpui::{Div, Stateful, div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn share_card(theme: Theme, share: &Share, cx: &Context<Self>) -> Stateful<Div> {
        let locale = language(cx);
        let id = share.id;
        let status = share.status_description(locale);
        theme
            .card()
            .id(("share-card", id))
            .flex_1()
            .min_w_0()
            .p(px(20.))
            .gap(px(12.))
            .cursor_pointer()
            .hover(|s| s.border_color(share.tone.accent(theme)))
            .on_click(cx.listener(move |this, _, window, cx| {
                this.dispatch(Action::Detail(id), window, cx);
            }))
            .child(
                row()
                    .justify_between()
                    .child(theme.icon_tile(share.symbol, share.tone, 44.))
                    .child(
                        Self::button(
                            ("share-menu", id),
                            "",
                            Icon::More,
                            ButtonStyle::Ghost,
                            Action::Detail(id),
                            cx,
                        )
                        .compact()
                        .tooltip(locale.text(Text::ShareDetails)),
                    ),
            )
            .child(
                column()
                    .gap(px(5.))
                    .child(theme.heading(share.name.clone(), 18.).truncate())
                    .child(theme.caption(share.description(locale))),
            )
            .child(
                row()
                    .justify_between()
                    .pt(px(6.))
                    .border_t_1()
                    .border_color(theme.border())
                    .child(
                        row()
                            .gap(px(6.))
                            .child(theme.dot(if share.live() {
                                Tone::Mint
                            } else {
                                Tone::Neutral
                            }))
                            .child(theme.caption(status)),
                    )
                    .child(
                        Self::button(
                            ("share-copy", id),
                            "",
                            Icon::Link,
                            ButtonStyle::Ghost,
                            Action::CopyLink(id),
                            cx,
                        )
                        .compact()
                        .tooltip(locale.text(Text::CopyShareLink))
                        .disabled(share.usable_link().is_none()),
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
                        Filter::Finished => share.finished(),
                    }
            })
            .collect()
    }
    pub fn shares_page(&self, theme: Theme, compact: bool, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let shares = self.visible_shares(cx);
        let count = shares.len();
        let columns = if compact { 2 } else { 3 };
        column()
            .gap(px(24.))
            .child(
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap_4()
                    .child(self.share_filters(theme, cx))
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
                                    Action::ToggleGrid,
                                    cx,
                                )
                                .tooltip(locale.text(Text::ChangeLayout)),
                            ),
                    ),
            )
            .child(
                row()
                    .justify_between()
                    .child(theme.caption(locale.shares(count)))
                    .child(theme.caption(locale.text(Text::NewestFirst))),
            )
            .when(count == 0, |el| el.child(Self::empty_shares(theme, cx)))
            .when(count > 0 && self.grid, |el| {
                el.children(shares.chunks(columns).map(|chunk| {
                    let mut cards = row().gap(px(18.)).items_stretch();
                    for share in chunk {
                        cards = cards.child(Self::share_card(theme, share, cx));
                    }
                    for _ in chunk.len()..columns {
                        cards = cards.child(div().flex_1());
                    }
                    cards
                }))
            })
            .when(count > 0 && !self.grid, |el| {
                el.child(
                    theme.card().overflow_hidden().children(
                        shares
                            .iter()
                            .map(|share| Self::share_list_row(theme, share, cx)),
                    ),
                )
            })
    }
    fn share_filters(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        row()
            .gap(px(4.))
            .p(px(4.))
            .rounded(px(11.))
            .bg(theme.inset())
            .children(
                [
                    (Filter::All, locale.text(Text::All)),
                    (Filter::Live, locale.text(Text::Sharing)),
                    (
                        Filter::Finished,
                        locale.text(if self.runtime.is_preview() {
                            Text::Paused
                        } else {
                            Text::FinishedShares
                        }),
                    ),
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
                            Icon::Close
                        },
                        if filter == self.filter {
                            ButtonStyle::Secondary
                        } else {
                            ButtonStyle::Ghost
                        },
                        Action::Filter(filter),
                        cx,
                    )
                    .compact()
                }),
            )
    }
    fn share_list_row(theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        row()
            .gap(px(12.))
            .p(px(20.))
            .border_b_1()
            .border_color(theme.border())
            .child(theme.icon_tile(share.symbol, share.tone, 40.))
            .child(
                column()
                    .flex_1()
                    .gap(px(5.))
                    .child(theme.heading(share.name.clone(), 17.))
                    .child(theme.caption(share.description(locale))),
            )
            .child(theme.badge(locale.text(share.status().0), share.status().1))
            .child(
                Self::button(
                    ("list-link", share.id),
                    locale.text(Text::CopyLink),
                    Icon::Link,
                    ButtonStyle::Secondary,
                    Action::CopyLink(share.id),
                    cx,
                )
                .compact()
                .disabled(share.usable_link().is_none()),
            )
            .child(
                Self::button(
                    ("list-detail", share.id),
                    "",
                    Icon::Chevron,
                    ButtonStyle::Ghost,
                    Action::Detail(share.id),
                    cx,
                )
                .compact()
                .tooltip(locale.text(Text::ShareDetails)),
            )
    }
    fn empty_shares(theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        theme
            .card()
            .h(px(330.))
            .items_center()
            .justify_center()
            .gap(px(12.))
            .child(theme.icon_tile(Icon::Search, Tone::Lavender, 58.))
            .child(theme.heading(locale.text(Text::NoSharesFound), 22.))
            .child(
                theme
                    .label(locale.text(Text::NoSharesFoundDetail), 16.)
                    .text_color(theme.muted_text()),
            )
            .child(Self::button(
                "empty-new",
                locale.text(Text::NewShare),
                Icon::Plus,
                ButtonStyle::Primary,
                Action::NewShare,
                cx,
            ))
    }
}
