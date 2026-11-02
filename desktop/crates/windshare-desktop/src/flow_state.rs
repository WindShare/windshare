use crate::{model::ShareContents, share_timing::ShareFrame};
use std::time::Instant;
use windshare_ui::{Icon, Tone};
pub const COPY_FEEDBACK_MS: u64 = 2400;

#[derive(Clone, Copy)]
pub enum CopyFeedback {
    Idle,
    Copied { share_id: usize, revision: usize },
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

#[derive(Clone, Copy, Debug)]
pub struct ShareRun {
    pub operation: usize,
    pub bundle: SampleBundle,
    pub started_at: Instant,
    pub readiness: ShareReadiness,
}

#[derive(Clone, Copy, Debug)]
pub enum ShareReadiness {
    Pending,
    Ready { share_id: usize, at: Instant },
}

impl ShareRun {
    pub fn sample(self, now: Instant, still: bool) -> ShareFrame {
        let ready_at = match self.readiness {
            ShareReadiness::Pending => None,
            ShareReadiness::Ready { at, .. } => Some(at.saturating_duration_since(self.started_at)),
        };
        ShareFrame::sample(
            now.saturating_duration_since(self.started_at),
            ready_at,
            still,
        )
    }

    pub const fn share_id(self) -> Option<usize> {
        match self.readiness {
            ShareReadiness::Pending => None,
            ShareReadiness::Ready { share_id, .. } => Some(share_id),
        }
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
