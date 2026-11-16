use super::{Preference, PreferenceChoice};
use crate::motion::ShareAnimation;
use gpui::{App, BorrowAppContext, Global};

pub type ShareAnimationSettings = Preference<ShareAnimation>;

impl Global for ShareAnimationSettings {}

impl PreferenceChoice for ShareAnimation {
    const KEY: &'static str = "share-animation";

    fn code(self) -> &'static str {
        match self {
            Self::Whirl => "whirl",
            Self::Paper => "paper",
        }
    }

    fn parse(value: &str) -> Option<Self> {
        match value {
            "whirl" => Some(Self::Whirl),
            "paper" => Some(Self::Paper),
            _ => None,
        }
    }
}

pub fn select_share_animation(choice: ShareAnimation, cx: &mut App) {
    cx.update_global::<ShareAnimationSettings, _>(|settings, _| settings.select(choice));
}
