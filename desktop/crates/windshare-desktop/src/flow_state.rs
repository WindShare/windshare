use windshare_ui::{Icon, Tone};

pub const SHARE_PREPARE_MS: u64 = 1350;
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
    pub fn name(self) -> &'static str {
        match self {
            Self::Design => "品牌设计素材",
            Self::Photos => "京都的周末",
        }
    }
    pub fn detail(self) -> &'static str {
        match self {
            Self::Design => "3 个文件",
            Self::Photos => "1 个文件夹 · 3 个文件",
        }
    }
    pub fn size(self) -> &'static str {
        match self {
            Self::Design => "32 MB",
            Self::Photos => "24 MB",
        }
    }
    pub fn icon(self) -> Icon {
        match self {
            Self::Design => Icon::File,
            Self::Photos => Icon::Folder,
        }
    }
    pub fn tone(self) -> Tone {
        match self {
            Self::Design => Tone::Lavender,
            Self::Photos => Tone::Peach,
        }
    }
}
#[derive(Clone, Copy, Debug)]
pub enum ShareFlow {
    Choose,
    Preparing {
        operation: usize,
        bundle: SampleBundle,
    },
    Ready {
        share_id: usize,
    },
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
