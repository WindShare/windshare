use crate::{Icon, RADIUS, Tone, border, icon, ink, muted, paper, primary};
use gpui::{prelude::*, *};

pub fn row() -> Div {
    div().flex().items_center()
}
pub fn column() -> Div {
    div().flex().flex_col()
}

pub fn label(text: impl Into<SharedString>, size: f32) -> Div {
    div()
        .text_size(px(size))
        .text_color(ink())
        .child(text.into())
}
pub fn caption(text: impl Into<SharedString>) -> Div {
    label(text, 14.).text_color(muted())
}
pub fn heading(text: impl Into<SharedString>, size: f32) -> Div {
    label(text, size).font_weight(FontWeight::BOLD)
}
pub fn card() -> Div {
    column()
        .rounded(px(RADIUS))
        .bg(paper())
        .border_1()
        .border_color(border())
}
pub fn badge(text: impl Into<SharedString>, tone: Tone) -> Div {
    row()
        .gap(px(5.))
        .px(px(8.))
        .h(px(28.))
        .rounded(px(6.))
        .bg(tone.surface())
        .text_color(tone.text())
        .text_size(px(12.))
        .font_weight(FontWeight::SEMIBOLD)
        .child(text.into())
}
pub fn dot(tone: Tone) -> Div {
    div()
        .size(px(6.))
        .rounded_full()
        .bg(tone.text())
        .flex_shrink_0()
}
pub fn icon_tile(symbol: Icon, tone: Tone, size: f32) -> Div {
    row()
        .justify_center()
        .size(px(size))
        .flex_shrink_0()
        .rounded(px(size * 0.28))
        .bg(tone.surface())
        .text_color(tone.text())
        .child(icon(symbol, size * 0.44).text_color(tone.text()))
}
pub fn progress(value: f32, tone: Tone) -> Div {
    div()
        .h(px(5.))
        .w_full()
        .rounded_full()
        .bg(tone.surface())
        .overflow_hidden()
        .child(
            div()
                .h_full()
                .w(relative(value.clamp(0., 1.)))
                .rounded_full()
                .bg(tone.accent()),
        )
}
pub fn separator() -> Div {
    div().w_full().h(px(1.)).bg(border()).flex_shrink_0()
}

pub fn toggle(id: impl Into<ElementId>, checked: bool) -> Stateful<Div> {
    row()
        .id(id)
        .role(Role::Switch)
        .aria_toggled(if checked {
            Toggled::True
        } else {
            Toggled::False
        })
        .w(px(36.))
        .h(px(21.))
        .px(px(3.))
        .rounded_full()
        .cursor_pointer()
        .bg(if checked {
            primary()
        } else {
            Tone::Neutral.accent()
        })
        .when(checked, |el| el.justify_end())
        .focusable()
        .tab_index(0)
        .focus_visible(|s| s.border_2().border_color(Tone::Mint.accent()))
        .hover(|s| s.opacity(0.85))
        .child(div().size(px(15.)).rounded_full().bg(paper()))
}
pub fn section_header(title: &str, meta: &str) -> Div {
    row()
        .justify_between()
        .child(heading(title.to_owned(), 17.))
        .child(caption(meta.to_owned()))
}
pub fn avatar(initials: &str, tone: Tone, size: f32) -> Div {
    row()
        .size(px(size))
        .justify_center()
        .rounded_full()
        .bg(tone.surface())
        .text_color(tone.text())
        .text_size(px(size * 0.32))
        .font_weight(FontWeight::BOLD)
        .child(initials.to_owned())
}
