use crate::{Icon, RADIUS, Theme, Tone, icon};
use gpui::{
    Div, ElementId, FontWeight, Pixels, Point, Role, SharedString, Size, Stateful, Svg, Toggled,
    Transformation, div, prelude::*, px, relative, size, svg,
};

pub fn vector_sprite(
    path: impl Into<SharedString>,
    native_size: Size<Pixels>,
    center: Point<Pixels>,
    scale: f32,
) -> Svg {
    // GPUI's SVG atlas is keyed by raster size. Animate a GPU transform so every
    // intermediate scale reuses the same texture, including on the first playback.
    svg()
        .path(path)
        .absolute()
        .left_0()
        .top_0()
        .w(native_size.width)
        .h(native_size.height)
        .with_transformation(Transformation::scale(size(scale, scale)).with_translation(
            center - gpui::point(native_size.width / 2., native_size.height / 2.),
        ))
}

pub fn row() -> Div {
    div().flex().items_center()
}
pub fn column() -> Div {
    div().flex().flex_col()
}

impl Theme {
    pub fn label(self, text: impl Into<SharedString>, size: f32) -> Div {
        div()
            .text_size(px(size))
            .text_color(self.text())
            .child(text.into())
    }
    pub fn caption(self, text: impl Into<SharedString>) -> Div {
        self.label(text, 14.).text_color(self.muted_text())
    }
    pub fn heading(self, text: impl Into<SharedString>, size: f32) -> Div {
        self.label(text, size).font_weight(FontWeight::BOLD)
    }
    pub fn card(self) -> Div {
        column()
            .rounded(px(RADIUS))
            .bg(self.surface())
            .border_1()
            .border_color(self.border())
    }
    pub fn badge(self, text: impl Into<SharedString>, tone: Tone) -> Div {
        row()
            .gap(px(5.))
            .px(px(8.))
            .h(px(28.))
            .rounded(px(6.))
            .bg(tone.surface(self))
            .text_color(tone.text(self))
            .text_size(px(12.))
            .font_weight(FontWeight::SEMIBOLD)
            .child(text.into())
    }
    pub fn dot(self, tone: Tone) -> Div {
        div()
            .size(px(6.))
            .rounded_full()
            .bg(tone.text(self))
            .flex_shrink_0()
    }
    pub fn icon_tile(self, symbol: Icon, tone: Tone, size: f32) -> Div {
        row()
            .justify_center()
            .size(px(size))
            .flex_shrink_0()
            .rounded(px(size * 0.28))
            .bg(tone.surface(self))
            .text_color(tone.text(self))
            .child(icon(symbol, size * 0.44).text_color(tone.text(self)))
    }
    pub fn progress(self, value: f32, tone: Tone) -> Div {
        div()
            .h(px(5.))
            .w_full()
            .rounded_full()
            .bg(tone.surface(self))
            .overflow_hidden()
            .child(
                div()
                    .h_full()
                    .w(relative(value.clamp(0., 1.)))
                    .rounded_full()
                    .bg(tone.accent(self)),
            )
    }
    pub fn separator(self) -> Div {
        div().w_full().h(px(1.)).bg(self.border()).flex_shrink_0()
    }

    pub fn toggle(self, id: impl Into<ElementId>, checked: bool) -> Stateful<Div> {
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
            .bg(self.switch_track(checked))
            .when(checked, gpui::Styled::justify_end)
            .focusable()
            .tab_index(0)
            .focus_visible(|s| s.border_2().border_color(self.focus_ring()))
            .hover(|s| s.opacity(0.85))
            .child(
                div()
                    .size(px(15.))
                    .rounded_full()
                    .bg(self.switch_thumb(checked)),
            )
    }
    pub fn section_header(self, title: &str, meta: &str) -> Div {
        row()
            .justify_between()
            .child(self.heading(title.to_owned(), 17.))
            .child(self.caption(meta.to_owned()))
    }
    pub fn avatar(self, initials: &str, tone: Tone, size: f32) -> Div {
        row()
            .size(px(size))
            .justify_center()
            .rounded_full()
            .bg(tone.surface(self))
            .text_color(tone.text(self))
            .text_size(px(size * 0.32))
            .font_weight(FontWeight::BOLD)
            .child(initials.to_owned())
    }
}
