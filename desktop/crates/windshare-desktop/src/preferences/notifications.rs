use super::{Preference, PreferenceChoice};
use gpui::{App, BorrowAppContext, Global};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum NotificationsPreference {
    #[default]
    Enabled,
    Disabled,
}

impl NotificationsPreference {
    pub const fn enabled(self) -> bool {
        matches!(self, Self::Enabled)
    }

    pub const fn from_enabled(enabled: bool) -> Self {
        if enabled {
            Self::Enabled
        } else {
            Self::Disabled
        }
    }
}

pub type NotificationsSettings = Preference<NotificationsPreference>;

impl Global for NotificationsSettings {}

impl PreferenceChoice for NotificationsPreference {
    const KEY: &'static str = "notifications";

    fn code(self) -> &'static str {
        match self {
            Self::Enabled => "enabled",
            Self::Disabled => "disabled",
        }
    }

    fn parse(value: &str) -> Option<Self> {
        match value {
            "enabled" => Some(Self::Enabled),
            "disabled" => Some(Self::Disabled),
            _ => None,
        }
    }
}

pub fn select_notifications(choice: NotificationsPreference, cx: &mut App) {
    cx.update_global::<NotificationsSettings, _>(|settings, _| settings.select(choice));
}
