use crate::{Icon, Theme, Tone, icon};
use gpui::{
    App, AppContext, ElementId, FocusHandle, FontWeight, Pixels, Role, SharedString, Window, div,
    prelude::*, px,
};
use std::rc::Rc;

pub const LARGE_BUTTON_HEIGHT: f32 = 54.;
const QUIET_HOVER_OPACITY: f32 = 0.08;

#[derive(Clone, Copy, Default)]
enum ButtonAlignment {
    #[default]
    Center,
    Start,
}

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
    Contrast,
    #[default]
    Secondary,
    Ghost,
    Quiet,
    Navigation,
    ActiveNavigation(Tone),
    Tinted(Tone),
}

struct ButtonColors {
    background: gpui::Hsla,
    foreground: gpui::Hsla,
    hover: gpui::Hsla,
    hover_foreground: gpui::Hsla,
    border: gpui::Hsla,
    focus: gpui::Hsla,
}

impl ButtonStyle {
    fn colors(self, theme: Theme) -> ButtonColors {
        match self {
            Self::Primary => ButtonColors {
                background: theme.action_surface(),
                foreground: theme.action_text(),
                hover: theme.action_hover(),
                hover_foreground: theme.action_text(),
                border: theme.action_surface(),
                focus: theme.action_text(),
            },
            Self::Contrast => ButtonColors {
                background: theme.text(),
                foreground: theme.canvas(),
                hover: theme.accent_text(),
                hover_foreground: theme.canvas(),
                border: theme.text(),
                focus: theme.accent_text(),
            },
            Self::Secondary => ButtonColors {
                background: theme.surface(),
                foreground: theme.text(),
                hover: Tone::Neutral.surface(theme),
                hover_foreground: theme.text(),
                border: theme.border(),
                focus: theme.focus_ring(),
            },
            Self::Ghost => ButtonColors {
                background: gpui::transparent_black(),
                foreground: theme.muted_text(),
                hover: Tone::Neutral.surface(theme),
                hover_foreground: theme.muted_text(),
                border: gpui::transparent_black(),
                focus: theme.focus_ring(),
            },
            // Text actions on decorative surfaces should preserve the canvas beneath them.
            Self::Quiet => ButtonColors {
                background: gpui::transparent_black(),
                foreground: theme.muted_text(),
                hover: theme.accent_text().opacity(QUIET_HOVER_OPACITY),
                hover_foreground: theme.accent_text(),
                border: gpui::transparent_black(),
                focus: theme.focus_ring(),
            },
            Self::Navigation => ButtonColors {
                background: gpui::transparent_black(),
                foreground: theme.muted_text(),
                hover: theme.navigation_hover(),
                hover_foreground: theme.muted_text(),
                border: gpui::transparent_black(),
                focus: theme.focus_ring(),
            },
            Self::ActiveNavigation(tone) => ButtonColors {
                background: tone.navigation_surface(theme),
                foreground: tone.text(theme),
                hover: tone.accent(theme).opacity(0.35),
                hover_foreground: tone.text(theme),
                border: gpui::transparent_black(),
                focus: theme.focus_ring(),
            },
            Self::Tinted(tone) => ButtonColors {
                background: tone.surface(theme),
                foreground: tone.text(theme),
                hover: tone.accent(theme).opacity(0.35),
                hover_foreground: tone.text(theme),
                border: gpui::transparent_black(),
                focus: tone.text(theme),
            },
        }
    }
}

#[derive(IntoElement)]
#[must_use]
pub struct Button {
    id: ElementId,
    label: SharedString,
    symbol: Option<Icon>,
    style: ButtonStyle,
    handler: Option<PressHandler>,
    size: ButtonSize,
    full_width: bool,
    alignment: ButtonAlignment,
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
            alignment: ButtonAlignment::default(),
            disabled: false,
            tooltip: None,
            focus: None,
            min_width: None,
        }
    }
    pub const fn icon(mut self, icon: Icon) -> Self {
        self.symbol = Some(icon);
        self
    }
    pub const fn style(mut self, style: ButtonStyle) -> Self {
        self.style = style;
        self
    }
    pub const fn full_width(mut self) -> Self {
        self.full_width = true;
        self
    }
    pub const fn align_start(mut self) -> Self {
        self.alignment = ButtonAlignment::Start;
        self
    }
    pub const fn min_width(mut self, width: Pixels) -> Self {
        self.min_width = Some(width);
        self
    }
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }
    pub const fn large(mut self) -> Self {
        self.size = ButtonSize::Large;
        self
    }
    pub const fn compact(mut self) -> Self {
        self.size = ButtonSize::Small;
        self
    }
    pub const fn disabled(mut self, disabled: bool) -> Self {
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
    fn render(self, window: &mut Window, _: &mut App) -> impl IntoElement {
        let theme = Theme::from(window.appearance());
        let colors = self.style.colors(theme);
        let handler = self.handler;
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
                ButtonSize::Large => LARGE_BUTTON_HEIGHT,
            }))
            .when_some(self.min_width, gpui::Styled::min_w)
            .when(self.full_width, gpui::Styled::w_full)
            .when(matches!(self.alignment, ButtonAlignment::Start), |el| {
                el.justify_start()
            })
            .px(px(if empty { 9. } else { 16. }))
            .flex_shrink_0()
            .rounded(px(10.))
            .border_1()
            .border_color(colors.border)
            .bg(colors.background)
            .text_color(colors.foreground)
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
                    .hover(move |s| s.bg(colors.hover).text_color(colors.hover_foreground))
                    .active(|s| s.opacity(0.75))
                    .focus_visible(|s| s.border_color(colors.focus).shadow_sm())
                    // GPUI maps Enter/Space release to a click for focused elements.
                    // A separate key-down handler would activate toggles twice.
                    .on_click(move |_, window, cx| {
                        cx.stop_propagation();
                        if let Some(f) = &handler {
                            f(&(), window, cx);
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
                    .text_color(colors.foreground),
                )
            })
            .when(!empty, |el| el.child(self.label))
    }
}

struct Tooltip(SharedString);
impl Render for Tooltip {
    fn render(&mut self, window: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::from(window.appearance());
        div()
            .px_3()
            .py_2()
            .rounded_lg()
            .bg(Tone::Mint.surface(theme))
            .border_1()
            .border_color(theme.border())
            .text_color(theme.text())
            .text_size(px(14.))
            .child(self.0.clone())
    }
}
