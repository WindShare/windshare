use crate::{
    localization::{Text, language},
    motion::{SpiritMood, frame_ticker, scaled_spirit},
};
use gpui::{App, Context, Div, Role, Window, prelude::*, px};
use std::time::{Duration, Instant};
use windshare_host::{AccountState, AccountView};
use windshare_ui::{Button, Icon, Theme, Tone, column, row};

pub(super) fn registration_panel_view(
    theme: Theme,
    view: &AccountView,
    busy: bool,
    visible: bool,
    create_button: Button,
    login_button: Button,
    cx: &App,
) -> Div {
    let locale = language(cx);
    let progress = match view.status.state {
        AccountState::Ready if !view.status.account.is_empty() => Some(1000),
        AccountState::Registering {
            registration_permille,
        } => Some(registration_permille),
        _ if busy => Some(0),
        _ => None,
    };
    if let Some(permille) = progress {
        return column()
            .gap(px(18.))
            .child(
                column()
                    .items_center()
                    .gap(px(8.))
                    .child(scaled_spirit(
                        "registering-spirit",
                        SpiritMood::Idle,
                        Tone::Mint,
                        0.45,
                        cx.reduce_motion() || !visible,
                    ))
                    .child(theme.heading(locale.text(Text::AccountSetup), 20.)),
            )
            .child(RegistrationProgress { permille, visible })
            .child(theme.caption(locale.text(Text::AccountRegistrationWait)));
    }
    column()
        .gap(px(24.))
        .child(
            row()
                .gap(px(14.))
                .child(theme.icon_tile(Icon::Shield, Tone::Mint, 48.))
                .child(
                    column()
                        .flex_1()
                        .min_w_0()
                        .gap(px(5.))
                        .child(theme.heading(locale.text(Text::AccountRegistrationBenefit), 18.))
                        .child(theme.caption(locale.text(Text::AccountRegistrationDetail))),
                ),
        )
        .child(
            row()
                .flex_wrap()
                .gap(px(8.))
                .child(create_button)
                .child(login_button),
        )
}

#[derive(IntoElement)]
struct RegistrationProgress {
    permille: u16,
    visible: bool,
}

impl RenderOnce for RegistrationProgress {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let percent = f32::from(self.permille) / 10.;
        let meter = window.use_keyed_state("registration-progress", cx, |_, _| ProgressMeter {
            from: percent,
            target: percent,
            since: Instant::now(),
            visible: self.visible,
        });
        meter.update(cx, |meter, cx| {
            if percent > meter.target {
                let now = Instant::now();
                meter.from = meter.sample(now);
                meter.target = percent;
                meter.since = now;
                cx.notify();
            }
            meter.visible = self.visible;
        });
        meter
    }
}

struct ProgressMeter {
    from: f32,
    target: f32,
    since: Instant,
    visible: bool,
}

impl ProgressMeter {
    const SETTLE: Duration = Duration::from_millis(320);

    // Retarget from the visible position when a host update arrives mid-transition.
    fn sample(&self, now: Instant) -> f32 {
        let t = (now.duration_since(self.since).as_secs_f32() / Self::SETTLE.as_secs_f32()).min(1.);
        (self.from - self.target).mul_add((1. - t).powi(2), self.target)
    }
}

impl Render for ProgressMeter {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let locale = language(cx);
        let now = Instant::now();
        let animate =
            self.visible && window.is_visible() && window.is_window_active() && !cx.reduce_motion();
        let target = self.target;
        let value = if animate { self.sample(now) } else { target };
        let label = locale.text(if target <= 0. {
            Text::AccountConnecting
        } else {
            Text::EstimatedProgress
        });
        column()
            .id("registration-estimate")
            .role(Role::ProgressIndicator)
            .aria_label(label)
            .when(target > 0., |bar| {
                bar.aria_numeric_value(f64::from(target.round()))
                    .aria_min_numeric_value(0.)
                    .aria_max_numeric_value(100.)
            })
            .gap(px(8.))
            .child(
                row()
                    .justify_between()
                    .child(theme.caption(label))
                    .when(target > 0., |line| {
                        line.child(theme.caption(format!("{value:.0}%")))
                    }),
            )
            .child(theme.progress(value / 100., Tone::Mint))
            .when(
                animate && self.from < target && now.duration_since(self.since) < Self::SETTLE,
                |meter| meter.child(frame_ticker()),
            )
    }
}
