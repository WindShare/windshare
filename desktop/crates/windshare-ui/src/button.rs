use crate::{Icon, Tone, action_hover, action_surface, border, icon, ink, muted, paper, primary};
use gpui::{prelude::*, *};
use std::rc::Rc;

#[derive(Clone, Copy)]
enum ButtonSize {
    Small,
    Regular,
    Large,
}

type PressHandler = Rc<dyn Fn(&(), &mut Window, &mut App)>;

#[derive(Clone, Copy, Default)]
pub enum ButtonStyle {
    Primary,
    #[default]
    Secondary,
    Ghost,
    Tinted(Tone),
}

#[derive(IntoElement)]
pub struct Button {
    id: ElementId,
    label: SharedString,
    symbol: Option<Icon>,
    style: ButtonStyle,
    handler: Option<PressHandler>,
    size: ButtonSize,
    full_width: bool,
    disabled: bool,
    tooltip: Option<SharedString>,
    focus: Option<FocusHandle>,
    min_width: Option<Pixels>,
}

impl Button {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            symbol: None,
            style: ButtonStyle::default(),
            handler: None,
            size: ButtonSize::Regular,
            full_width: false,
            disabled: false,
            tooltip: None,
            focus: None,
            min_width: None,
        }
    }
    pub fn icon(mut self, icon: Icon) -> Self {
        self.symbol = Some(icon);
        self
    }
    pub fn style(mut self, style: ButtonStyle) -> Self {
        self.style = style;
        self
    }
    pub fn full_width(mut self) -> Self {
        self.full_width = true;
        self
    }
    pub fn min_width(mut self, width: Pixels) -> Self {
        self.min_width = Some(width);
        self
    }
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }
    pub fn large(mut self) -> Self {
        self.size = ButtonSize::Large;
        self
    }
    pub fn compact(mut self) -> Self {
        self.size = ButtonSize::Small;
        self
    }
    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }
    pub fn tooltip(mut self, text: impl Into<SharedString>) -> Self {
        self.tooltip = Some(text.into());
        self
    }
    pub fn on_press(mut self, f: impl Fn(&(), &mut Window, &mut App) + 'static) -> Self {
        self.handler = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for Button {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let (bg, fg, hover, stroke) = match self.style {
            ButtonStyle::Primary => (
                action_surface(),
                primary(),
                action_hover(),
                action_surface(),
            ),
            ButtonStyle::Secondary => (paper(), ink(), Tone::Neutral.surface(), border()),
            ButtonStyle::Ghost => (
                gpui::transparent_black(),
                muted(),
                Tone::Neutral.surface(),
                gpui::transparent_black(),
            ),
            ButtonStyle::Tinted(tone) => (
                tone.surface(),
                tone.text(),
                tone.accent().opacity(0.35),
                gpui::transparent_black(),
            ),
        };
        let mouse_handler = self.handler.clone();
        let key_handler = self.handler;
        let empty = self.label.is_empty();
        let accessible_label = if empty {
            self.tooltip
                .clone()
                .unwrap_or_else(|| self.symbol.map_or("Button", Icon::name).into())
        } else {
            self.label.clone()
        };
        div()
            .id(self.id)
            .role(Role::Button)
            .aria_label(accessible_label)
            .flex()
            .items_center()
            .justify_center()
            .gap(px(8.))
            .h(px(match self.size {
                ButtonSize::Small => 36.,
                ButtonSize::Regular => 46.,
                ButtonSize::Large => 54.,
            }))
            .when_some(self.min_width, |el, width| el.min_w(width))
            .when(self.full_width, |el| el.w_full().justify_start())
            .px(px(if empty { 9. } else { 16. }))
            .flex_shrink_0()
            .rounded(px(10.))
            .border_1()
            .border_color(stroke)
            .bg(bg)
            .text_color(fg)
            .text_size(px(match self.size {
                ButtonSize::Small => 13.,
                ButtonSize::Regular => 15.,
                ButtonSize::Large => 16.,
            }))
            .font_weight(FontWeight::SEMIBOLD)
            .when(self.disabled, |el| el.opacity(0.4).cursor_not_allowed())
            .when(!self.disabled, |el| {
                el.cursor_pointer()
                    .focusable()
                    .when_some(self.focus, |el, handle| el.track_focus(&handle))
                    .tab_index(0)
                    .hover(move |s| s.bg(hover))
                    .active(|s| s.opacity(0.75))
                    .focus_visible(|s| s.border_color(primary()).shadow_sm())
                    .on_click(move |_, window, cx| {
                        cx.stop_propagation();
                        if let Some(f) = &mouse_handler {
                            f(&(), window, cx);
                        }
                    })
                    .on_key_down(move |event, window, cx| {
                        if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                            cx.stop_propagation();
                            if let Some(f) = &key_handler {
                                f(&(), window, cx);
                            }
                        }
                    })
            })
            .when_some(self.tooltip, |el, text| {
                el.tooltip(move |_, cx| cx.new(|_| Tooltip(text.clone())).into())
            })
            .when_some(self.symbol, |el, symbol| {
                el.child(
                    icon(
                        symbol,
                        if matches!(self.size, ButtonSize::Large) {
                            20.
                        } else {
                            18.
                        },
                    )
                    .text_color(fg),
                )
            })
            .when(!empty, |el| el.child(self.label))
    }
}

struct Tooltip(SharedString);
impl Render for Tooltip {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div()
            .px_3()
            .py_2()
            .rounded_lg()
            .bg(Tone::Mint.surface())
            .border_1()
            .border_color(border())
            .text_color(ink())
            .text_size(px(14.))
            .child(self.0.clone())
    }
}
