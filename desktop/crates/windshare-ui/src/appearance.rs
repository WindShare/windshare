use crate::Theme;
use gpui::{App, Global, Subscription, Window, WindowAppearance};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ThemePreference {
    #[default]
    System,
    Explicit(Theme),
}

impl Global for ThemePreference {}

impl ThemePreference {
    pub const ALL: [Self; 3] = [
        Self::System,
        Self::Explicit(Theme::Light),
        Self::Explicit(Theme::Dark),
    ];

    pub fn resolve(self, appearance: WindowAppearance) -> Theme {
        match self {
            Self::System => Theme::from(appearance),
            Self::Explicit(theme) => theme,
        }
    }
}

impl Theme {
    pub fn current(window: &Window, cx: &App) -> Self {
        cx.global::<ThemePreference>().resolve(window.appearance())
    }
}

/// Keep both subscriptions alive for the window's lifetime.
pub fn observe_theme(window: &mut Window, cx: &mut App) -> [Subscription; 2] {
    trace_theme(window, cx, "initial");
    [
        window.observe_window_appearance(|window, cx| {
            trace_theme(window, cx, "system_changed");
            if matches!(cx.global::<ThemePreference>(), ThemePreference::System) {
                window.refresh();
            }
        }),
        window.observe_global::<ThemePreference>(cx, |window, cx| {
            trace_theme(window, cx, "preference_changed");
            // Inputs, tooltips and cached canvases must invalidate together,
            // even when their owning entities did not otherwise change.
            window.refresh();
        }),
    ]
}

fn trace_theme(window: &Window, cx: &App, reason: &str) {
    tracing::info!(
        operation = "desktop.theme_resolved",
        session = std::process::id(),
        window = ?window.window_handle(),
        preference = ?cx.global::<ThemePreference>(),
        appearance = ?window.appearance(),
        theme = ?Theme::current(window, cx),
        reason,
    );
}
