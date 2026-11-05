mod backdrop;
mod checkout;
mod emblem;
mod layout;
mod offer;
mod redemption;
mod state;
mod visuals;

use crate::localization::{LanguageSettings, Text, language};
use gpui::{AppContext, Context, Entity, FocusHandle, Focusable, Subscription, Window, prelude::*};
use state::{
    Action, Attempt, CONFIRM_DELAY, Confirmation, ENABLE_DELAY, OFFLINE_CODE, PREVIEW_CODE, Page,
    Payment, Plan, REDEEM_DELAY, Redemption, State,
};
use windshare_ui::{Button, Input, InputEvent};

pub use backdrop::backdrop;
pub use visuals::brand_mark;

pub struct SuperShare {
    state: State,
    code: Entity<Input>,
    primary_focus: FocusHandle,
    sequence: usize,
    reduced_motion: bool,
    _subscriptions: Vec<Subscription>,
}

impl Focusable for SuperShare {
    fn focus_handle(&self, _: &gpui::App) -> FocusHandle {
        self.primary_focus.clone()
    }
}

impl SuperShare {
    pub fn new(window: &Window, cx: &mut Context<Self>) -> Self {
        let label = language(cx).text(Text::SuperCode);
        let code = cx.new(|cx| Input::new(label, cx).placeholder(label));
        let subscriptions = vec![
            cx.observe_global_in::<LanguageSettings>(window, |this, _, cx| {
                let label = language(cx).text(Text::SuperCode);
                this.code
                    .update(cx, |input, cx| input.set_labels(label, label, cx));
                cx.notify();
            }),
            cx.subscribe_in(&code, window, |this, _, event, window, cx| match event {
                InputEvent::Submit => this.dispatch(Action::SubmitCode, window, cx),
                InputEvent::Changed => {
                    if let Page::Redeem { status, .. } = &mut this.state.page {
                        *status = Redemption::Ready;
                    }
                    cx.notify();
                }
            }),
        ];
        Self {
            state: State::default(),
            code,
            primary_focus: cx.focus_handle(),
            sequence: 0,
            reduced_motion: false,
            _subscriptions: subscriptions,
        }
    }

    pub const fn enabled(&self) -> bool {
        self.state.enabled()
    }

    pub fn set_reduced_motion(&mut self, reduced: bool, cx: &mut Context<Self>) {
        self.reduced_motion = reduced;
        cx.notify();
    }

    pub fn leave(&mut self, cx: &mut Context<Self>) {
        if let Some(operation) = self.state.page.operation() {
            tracing::info!(
                event = "supershare.operation_canceled",
                session = std::process::id(),
                operation,
                reason = "navigation",
            );
            self.state.page.cancel_pending();
            cx.notify();
        }
    }

    fn button(id: &'static str, label: &'static str, action: Action, cx: &Context<Self>) -> Button {
        Button::new(id, label)
            .on_press(cx.listener(move |this, (), window, cx| this.dispatch(action, window, cx)))
    }

    fn dispatch(&mut self, action: Action, window: &mut Window, cx: &mut Context<Self>) {
        let before = self.state;
        match action {
            Action::SelectPlan(plan) if matches!(self.state.page, Page::Offer(_)) => {
                self.state.page = Page::Offer(plan);
            }
            Action::Checkout if matches!(self.state.page, Page::Offer(_)) => {
                self.state.page = Page::Checkout {
                    plan: self.state.page.plan(),
                    payment: Payment::Alipay,
                    confirmation: Confirmation::Ready,
                };
                window.focus(&self.primary_focus, cx);
            }
            Action::SelectPayment(payment) => {
                if let Page::Checkout { plan, .. } = self.state.page {
                    self.state.page = Page::Checkout {
                        plan,
                        payment,
                        confirmation: Confirmation::Ready,
                    };
                }
            }
            Action::Confirm => self.confirm(cx),
            Action::Back if !matches!(self.state.page, Page::Enabled { .. }) => {
                self.state.page = Page::Offer(self.state.page.plan());
                window.focus(&self.primary_focus, cx);
            }
            Action::Redeem if !matches!(self.state.page, Page::Enabled { .. }) => {
                self.state.page = Page::Redeem {
                    plan: self.state.page.plan(),
                    status: Redemption::Ready,
                };
                window.focus(&self.code.read(cx).focus_handle(cx), cx);
            }
            Action::SubmitCode => self.redeem(cx),
            Action::ReturnToCheckout => {
                if let Page::Enabled { plan, payment } = self.state.page {
                    self.state.page = Page::Checkout {
                        plan,
                        payment,
                        confirmation: Confirmation::Ready,
                    };
                    window.focus(&self.primary_focus, cx);
                }
            }
            _ => {}
        }
        tracing::info!(
            event = "supershare.action", session = std::process::id(),
            ?action, ?before, after = ?self.state, preview = true,
        );
        cx.notify();
    }

    fn confirm(&mut self, cx: &Context<Self>) {
        let Page::Checkout {
            plan,
            payment,
            confirmation,
        } = self.state.page
        else {
            return;
        };
        let (attempt, delay) = match confirmation {
            Confirmation::Ready => (Attempt::Initial, CONFIRM_DELAY),
            Confirmation::Unconfirmed => (Attempt::Retry, ENABLE_DELAY),
            Confirmation::Checking { .. } => return,
        };
        self.sequence += 1;
        let operation = self.sequence;
        self.state.page = Page::Checkout {
            plan,
            payment,
            confirmation: Confirmation::Checking { operation, attempt },
        };
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(delay).await;
            let _ = view.update(cx, |this, cx| {
                if !this.accept_completion(operation) {
                    return;
                }
                // This preview never claims a payment settled; confirmation only changes local appearance.
                match attempt {
                    Attempt::Initial => {
                        this.state.page = Page::Checkout {
                            plan,
                            payment,
                            confirmation: Confirmation::Unconfirmed,
                        };
                    }
                    Attempt::Retry => this.state.enable(plan, payment),
                }
                this.trace_completion(operation);
                cx.notify();
            });
        })
        .detach();
    }

    fn redeem(&mut self, cx: &Context<Self>) {
        let Page::Redeem { plan, status } = self.state.page else {
            return;
        };
        if matches!(status, Redemption::Checking { .. }) {
            return;
        }
        let code = self.code.read(cx).value().trim().to_ascii_uppercase();
        if code.is_empty() {
            return;
        }
        self.sequence += 1;
        let operation = self.sequence;
        self.state.page = Page::Redeem {
            plan,
            status: Redemption::Checking { operation },
        };
        cx.spawn(async move |view, cx| {
            cx.background_executor().timer(REDEEM_DELAY).await;
            let _ = view.update(cx, |this, cx| {
                if !this.accept_completion(operation) {
                    return;
                }
                // Fixed fixtures exercise UI outcomes without inventing an official redemption service.
                if code == PREVIEW_CODE {
                    this.state.enable(Plan::LongTerm, Payment::Alipay);
                } else {
                    this.state.page = Page::Redeem {
                        plan,
                        status: if code == OFFLINE_CODE {
                            Redemption::Unavailable
                        } else {
                            Redemption::Invalid
                        },
                    };
                }
                this.trace_completion(operation);
                cx.notify();
            });
        })
        .detach();
    }

    fn accept_completion(&self, operation: usize) -> bool {
        let current = self.state.page.operation();
        if current == Some(operation) {
            return true;
        }
        tracing::info!(
            event = "supershare.completion_discarded",
            session = std::process::id(),
            operation,
            ?current,
            reason = "canceled_or_superseded",
        );
        false
    }

    fn trace_completion(&self, operation: usize) {
        tracing::info!(
            event = "supershare.completed", session = std::process::id(),
            operation, state = ?self.state, preview = true, network = false,
        );
    }
}

impl Render for SuperShare {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = windshare_ui::Theme::from(window.appearance());
        let stage = layout::Stage::for_window(window);
        let content = match self.state.page {
            Page::Offer(plan) => self.offer_page(theme, stage, plan, window, cx),
            Page::Checkout {
                plan,
                payment,
                confirmation,
            } => self.checkout_page(theme, plan, payment, confirmation, cx),
            Page::Redeem { status, .. } => self.redemption_page(theme, status, cx),
            Page::Enabled { .. } => self.enabled_page(theme, stage, window, cx),
        };
        windshare_ui::column()
            .id("super-page-scroll")
            .size_full()
            .overflow_y_scroll()
            .child(
                windshare_ui::column()
                    .w_full()
                    .min_h(gpui::px(stage.height))
                    .justify_center()
                    .px(gpui::px(stage.gutter))
                    .py(gpui::px(32.))
                    .child(content),
            )
    }
}
