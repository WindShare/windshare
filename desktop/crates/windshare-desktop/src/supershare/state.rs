use crate::localization::Text;
use std::time::Duration;

pub const CONFIRM_DELAY: Duration = Duration::from_millis(2400);
pub const ENABLE_DELAY: Duration = Duration::from_millis(1600);
pub const REDEEM_DELAY: Duration = Duration::from_millis(1800);
pub const PREVIEW_CODE: &str = "SUPER-PREVIEW";
pub const OFFLINE_CODE: &str = "OFFLINE-PREVIEW";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Plan {
    Month,
    #[default]
    LongTerm,
}

impl Plan {
    pub const fn label(self) -> Text {
        match self {
            Self::Month => Text::SuperMonth,
            Self::LongTerm => Text::SuperLongTerm,
        }
    }

    pub const fn price(self) -> &'static str {
        match self {
            Self::Month => "4.9",
            Self::LongTerm => "9.9",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Coin {
    Btc,
    Eth,
    UsdtTrc20,
    Sol,
    Sui,
    Bch,
    Bnb,
}

impl Coin {
    pub const ALL: [Self; 7] = [
        Self::Btc,
        Self::Eth,
        Self::UsdtTrc20,
        Self::Sol,
        Self::Sui,
        Self::Bch,
        Self::Bnb,
    ];

    pub const fn label(self) -> &'static str {
        match self {
            Self::Btc => "BTC",
            Self::Eth => "ETH",
            Self::UsdtTrc20 => "USDT · TRC20",
            Self::Sol => "SOL",
            Self::Sui => "SUI",
            Self::Bch => "BCH",
            Self::Bnb => "BNB",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Payment {
    Alipay,
    PayPal,
    Crypto(Coin),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Attempt {
    Initial,
    Retry,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Confirmation {
    Ready,
    Checking { operation: usize, attempt: Attempt },
    Unconfirmed,
}

impl Confirmation {
    pub const fn busy(self) -> bool {
        matches!(self, Self::Checking { .. })
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Redemption {
    Ready,
    Checking { operation: usize },
    Invalid,
    Unavailable,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct State {
    pub page: Page,
    // Returning to checkout must not revoke the locally enabled plan.
    enabled_plan: Option<Plan>,
}

impl State {
    pub const fn enabled(self) -> bool {
        self.enabled_plan.is_some()
    }

    pub const fn enable(&mut self, plan: Plan, payment: Payment) {
        self.enabled_plan = Some(plan);
        self.page = Page::Enabled { plan, payment };
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Page {
    Offer(Plan),
    Checkout {
        plan: Plan,
        payment: Payment,
        confirmation: Confirmation,
    },
    Redeem {
        plan: Plan,
        status: Redemption,
    },
    Enabled {
        plan: Plan,
        payment: Payment,
    },
}

impl Default for Page {
    fn default() -> Self {
        Self::Offer(Plan::default())
    }
}

impl Page {
    pub const fn plan(self) -> Plan {
        match self {
            Self::Offer(plan)
            | Self::Enabled { plan, .. }
            | Self::Checkout { plan, .. }
            | Self::Redeem { plan, .. } => plan,
        }
    }

    pub const fn operation(self) -> Option<usize> {
        match self {
            Self::Checkout {
                confirmation: Confirmation::Checking { operation, .. },
                ..
            }
            | Self::Redeem {
                status: Redemption::Checking { operation },
                ..
            } => Some(operation),
            _ => None,
        }
    }

    pub const fn cancel_pending(&mut self) {
        match self {
            Self::Checkout { confirmation, .. } => {
                if let Confirmation::Checking { attempt, .. } = *confirmation {
                    *confirmation = match attempt {
                        Attempt::Initial => Confirmation::Ready,
                        Attempt::Retry => Confirmation::Unconfirmed,
                    };
                }
            }
            Self::Redeem { status, .. } if matches!(status, Redemption::Checking { .. }) => {
                *status = Redemption::Ready;
            }
            _ => {}
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub enum Action {
    SelectPlan(Plan),
    Checkout,
    SelectPayment(Payment),
    Confirm,
    Back,
    Redeem,
    SubmitCode,
    ReturnToCheckout,
}
