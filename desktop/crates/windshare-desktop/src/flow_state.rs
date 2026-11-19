use crate::{
    model::ShareContents,
    motion::{
        ShareAnimation,
        whirl::{Wind, seconds_between},
    },
    sharing::{LinkKind, Perch},
};
use std::time::{Duration, Instant};
use windshare_ui::{Icon, Tone};
pub const COPY_FEEDBACK_MS: u64 = 2400;

#[derive(Clone, Copy)]
pub enum CopyFeedback {
    Idle,
    Copied {
        share_id: usize,
        kind: LinkKind,
        revision: usize,
    },
}

impl CopyFeedback {
    pub fn matches(self, id: usize, link_kind: LinkKind) -> bool {
        matches!(self, Self::Copied { share_id, kind, .. } if share_id == id && kind == link_kind)
    }
}

pub const RECEIVE_OPEN_MS: u64 = 1000;
pub const RECEIVE_SAVE_MS: u64 = 2400;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SampleBundle {
    Design,
    Photos,
}
impl SampleBundle {
    pub const fn name(self) -> &'static str {
        match self {
            Self::Design => "品牌设计素材",
            Self::Photos => "京都的周末",
        }
    }
    pub const fn contents(self) -> ShareContents {
        match self {
            Self::Design => ShareContents::Files(3),
            Self::Photos => ShareContents::FoldersAndFiles {
                folders: 1,
                files: 3,
            },
        }
    }
    pub const fn size(self) -> &'static str {
        match self {
            Self::Design => "32 MB",
            Self::Photos => "24 MB",
        }
    }
    pub const fn icon(self) -> Icon {
        match self {
            Self::Design => Icon::File,
            Self::Photos => Icon::Folder,
        }
    }
    pub const fn tone(self) -> Tone {
        match self {
            Self::Design => Tone::Lavender,
            Self::Photos => Tone::Peach,
        }
    }
}
#[derive(Clone, Copy, Debug)]
pub enum ShareFlow {
    Choose,
    Running(ShareRun),
}

/// One presentation of a share on the sharing page, and the clock its entrance runs on.
#[derive(Clone, Copy, Debug)]
pub struct ShareRun {
    pub share_id: usize,
    started_at: Instant,
    /// The choose prompt to depart from. Without one, the spirit enters from
    /// a fresh scene in the result stage.
    pub perch: Option<Perch>,
    pub animation: ShareAnimation,
}

impl ShareRun {
    pub const fn new(
        share_id: usize,
        started_at: Instant,
        perch: Option<Perch>,
        animation: ShareAnimation,
    ) -> Self {
        Self {
            share_id,
            started_at,
            perch,
            animation,
        }
    }

    pub fn seconds(self, now: Instant) -> f32 {
        seconds_between(self.started_at, now)
    }

    /// Every entrance flies into the slot, whether launched from the choose
    /// prompt or staged directly by an external activation.
    pub const fn wind(self) -> Wind {
        Wind::Carried {
            since: self.started_at,
        }
    }

    /// Time left before the result may present more than "preparing".
    pub fn until_reveal(self, now: Instant) -> Duration {
        (self.started_at + Duration::from_secs_f32(self.animation.duration()))
            .saturating_duration_since(now)
    }
}
#[derive(Clone, Copy, Debug)]
pub enum LinkFeedback {
    None,
    Invalid,
}
#[derive(Clone, Copy, Debug)]
pub enum ReceiveFlow {
    Link { feedback: LinkFeedback },
    Opening { operation: usize },
    Files,
    Saving { operation: usize },
    Saved,
}
impl Default for ReceiveFlow {
    fn default() -> Self {
        Self::Link {
            feedback: LinkFeedback::None,
        }
    }
}

pub const RECEIVED_FILES: [(&str, &str, Icon); 3] = [
    ("清晨的鸭川.jpg", "8.2 MB", Icon::Image),
    ("街角与咖啡.jpg", "12.4 MB", Icon::Image),
    ("京都散步手记.pdf", "3.4 MB", Icon::File),
];
