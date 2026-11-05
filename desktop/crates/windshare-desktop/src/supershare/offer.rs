use super::{
    SuperShare,
    emblem::emblem,
    layout::{COLUMN_GAP, OfferLayout, PURCHASE_MAX_WIDTH, STACKED_HERO_GAP, Stage},
    state::{Action, Plan},
    visuals::{self, Finish},
};
use crate::localization::{Text, language};
use gpui::{
    Animation, AnimationExt, Context, Div, Role, Toggled, Window, ease_out_quint, prelude::*, px,
};
use std::time::Duration;
use windshare_ui::{ButtonStyle, Icon, Theme, column, icon, row};

const ENABLE_ENTRANCE: Duration = Duration::from_millis(640);
const PURCHASE_BUTTON_WIDTH: f32 = 240.;
const DETAILS_GAP: f32 = 48.;
const STACKED_PURCHASE_GAP: f32 = 28.;

impl SuperShare {
    pub(super) fn offer_page(
        &self,
        theme: Theme,
        stage: Stage,
        plan: Plan,
        window: &Window,
        cx: &Context<Self>,
    ) -> Div {
        let heading = visuals::heading(theme, stage.title_size);
        let emblem = emblem(
            theme,
            stage.emblem_size,
            self.reduced_motion || !window.is_window_active(),
        );
        let purchase = self.purchase_controls(theme, plan, cx);
        let composition = match stage.layout {
            OfferLayout::Columns { details_width } => row()
                .justify_between()
                .gap(px(COLUMN_GAP))
                .child(
                    column()
                        .w(px(details_width))
                        .flex_shrink_0()
                        .gap(px(DETAILS_GAP))
                        .child(heading)
                        .child(purchase),
                )
                .child(emblem),
            OfferLayout::Stacked => column()
                .gap(px(STACKED_PURCHASE_GAP))
                .child(
                    row()
                        .justify_between()
                        .gap(px(STACKED_HERO_GAP))
                        .child(heading)
                        .child(emblem),
                )
                .child(purchase.mx_auto()),
        };
        composition
            .w_full()
            .max_w(px(stage.content_width))
            .mx_auto()
    }

    fn purchase_controls(&self, theme: Theme, plan: Plan, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .w_full()
            .max_w(px(PURCHASE_MAX_WIDTH))
            .gap(px(32.))
            .child(
                row()
                    .gap(px(40.))
                    .child(Self::plan_option(theme, Plan::Month, plan, cx))
                    .child(Self::plan_option(theme, Plan::LongTerm, plan, cx)),
            )
            .child(
                row()
                    .gap(px(20.))
                    .child(
                        Self::button(
                            "super-buy",
                            locale.text(Text::SuperContinue),
                            Action::Checkout,
                            cx,
                        )
                        .icon(Icon::Arrow)
                        .style(ButtonStyle::Contrast)
                        .large()
                        .min_width(px(PURCHASE_BUTTON_WIDTH))
                        .focus_handle(&self.primary_focus),
                    )
                    .child(
                        Self::button(
                            "super-redeem-link",
                            locale.text(Text::SuperRedeemLink),
                            Action::Redeem,
                            cx,
                        )
                        .style(ButtonStyle::Quiet),
                    ),
            )
    }

    fn plan_option(
        theme: Theme,
        plan: Plan,
        selected: Plan,
        cx: &Context<Self>,
    ) -> impl IntoElement {
        let locale = language(cx);
        let finish = Finish::new(theme);
        let active = plan == selected;
        column()
            .id(("super-plan", plan as usize))
            .role(Role::RadioButton)
            .aria_label(format!(
                "{} · ${} USD",
                locale.text(plan.label()),
                plan.price()
            ))
            .aria_toggled(if active {
                Toggled::True
            } else {
                Toggled::False
            })
            .flex_1()
            .min_w_0()
            .gap(px(12.))
            .px(px(4.))
            .pt(px(8.))
            .pb(px(18.))
            .border_b_2()
            .border_color(if active {
                finish.foil
            } else {
                finish.edge.opacity(0.28)
            })
            .opacity(if active { 1. } else { 0.72 })
            .cursor_pointer()
            .focusable()
            .tab_index(0)
            .hover(|style| style.opacity(1.).border_color(finish.foil))
            .focus_visible(|style| style.opacity(1.).border_color(theme.focus_ring()))
            .on_click(cx.listener(move |this, _, window, cx| {
                this.dispatch(Action::SelectPlan(plan), window, cx);
            }))
            .child(
                row()
                    .justify_between()
                    .child(theme.heading(locale.text(plan.label()), 17.))
                    .child(
                        row()
                            .size(px(19.))
                            .justify_center()
                            .rounded_full()
                            .border_1()
                            .border_color(if active { finish.foil } else { finish.edge })
                            .when(active, |el| {
                                el.bg(finish.foil)
                                    .child(icon(Icon::Check, 12.).text_color(finish.pearl))
                            }),
                    ),
            )
            .child(visuals::price(theme, plan.price(), true))
    }

    pub(super) fn enabled_page(
        &self,
        theme: Theme,
        stage: Stage,
        window: &Window,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        column()
            .w_full()
            .items_center()
            .justify_center()
            .gap(px(32.))
            .child(
                emblem(
                    theme,
                    stage.emblem_size,
                    self.reduced_motion || !window.is_window_active(),
                )
                .with_animation(
                    ("super-enabled", self.sequence),
                    Animation::new(if self.reduced_motion {
                        Duration::from_millis(1)
                    } else {
                        ENABLE_ENTRANCE
                    })
                    .with_easing(ease_out_quint()),
                    |el, delta| el.opacity(delta).top(px(delta.mul_add(-12., 12.))),
                ),
            )
            .child(
                row()
                    .gap(px(12.))
                    .child(icon(Icon::Check, 23.).text_color(theme.accent_text()))
                    .child(theme.heading(locale.text(Text::SuperEnabled), 30.)),
            )
            .child(
                Self::button(
                    "super-return-to-checkout",
                    locale.text(Text::SuperReturnToPayment),
                    Action::ReturnToCheckout,
                    cx,
                )
                .icon(Icon::Arrow)
                .style(ButtonStyle::Contrast)
                .large()
                .min_width(px(PURCHASE_BUTTON_WIDTH))
                .focus_handle(&self.primary_focus),
            )
    }
}
