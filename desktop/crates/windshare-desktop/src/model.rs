mod share;
pub use share::{Share, ShareSource, ShareState};

use crate::localization::{Language, LanguageChoice, Text};
use crate::sharing::ShareLink;
use windshare_ui::{Icon, Tone};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Page {
    Share,
    Receive,
    Shares,
    Transfers,
    Settings,
    Components,
    SuperShare,
}
impl Page {
    pub const fn title(self, language: Language) -> &'static str {
        match self {
            Self::Share => language.text(Text::ShareTitle),
            Self::Receive => language.text(Text::ReceiveTitle),
            Self::Shares => language.text(Text::SharesTitle),
            Self::Transfers => language.text(Text::TransfersTitle),
            Self::Settings => language.text(Text::SettingsTitle),
            Self::Components => language.text(Text::ComponentsTitle),
            Self::SuperShare => "SuperShare",
        }
    }
    pub const fn subtitle(self, language: Language) -> &'static str {
        match self {
            Self::Share => language.text(Text::ShareSubtitle),
            Self::Receive => language.text(Text::ReceiveSubtitle),
            Self::Shares => language.text(Text::SharesSubtitle),
            Self::Transfers => language.text(Text::TransfersSubtitle),
            Self::Settings => language.text(Text::SettingsSubtitle),
            Self::Components => language.text(Text::ComponentsSubtitle),
            Self::SuperShare => "",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Filter {
    All,
    Live,
    Finished,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TransferState {
    Running,
    Paused,
    Complete,
}
#[derive(Clone)]
pub struct Transfer {
    pub symbol: Icon,
    pub rate_mbps: f32,
    pub remaining_seconds: u32,
    pub name: &'static str,
    pub peer: TransferPeer,
    pub size_detail: &'static str,
    pub progress: f32,
    pub tone: Tone,
    pub state: TransferState,
}
#[derive(Clone, Debug)]
pub enum Overlay {
    Detail(usize),
    ShareQr(usize),
    Help,
}
#[derive(Clone, Copy, Debug)]
pub enum Action {
    Navigate(Page),
    Filter(Filter),
    NewShare,
    SelectFiles,
    SelectFolder,
    ResetShare,
    Receive,
    DemoLink,
    OpenPreview,
    PasteLink,
    SavePreview,
    ResetReceive,
    Detail(usize),
    CopyLink(usize),
    ToggleLink(usize),
    EnlargeQr(usize),
    ToggleShare(usize),
    StopShare(usize),
    ShareAgain(usize),
    ConfigureShare(usize),
    SaveSharingSetup(usize),
    ResumeShare(usize),
    ApproveRelay,
    OpenLink(usize),
    ToggleTransfer(usize),
    ToggleGrid,
    Close,
    Help,
    ToggleSetting(usize),
    SetLanguage(LanguageChoice),
    Toast(Text),
}
pub fn demo_shares() -> Vec<Share> {
    vec![
        Share {
            id: 1,
            source: ShareSource::Preview,
            link: Some(ShareLink::preview(1)),
            error: None,
            relay_reconnecting: false,
            name: "品牌设计素材".into(),
            contents: ShareContents::Files(24),
            size: Some("128 MB".into()),
            age: ShareAge::JustNow,
            tone: Tone::Lavender,
            symbol: Icon::Folder,
            state: ShareState::Live { viewers: Some(2) },
        },
        Share {
            id: 2,
            source: ShareSource::Preview,
            link: Some(ShareLink::preview(2)),
            error: None,
            relay_reconnecting: false,
            name: "京都的周末".into(),
            contents: ShareContents::Photos(86),
            size: Some("1.4 GB".into()),
            age: ShareAge::MinutesAgo(12),
            tone: Tone::Peach,
            symbol: Icon::Image,
            state: ShareState::Live { viewers: Some(1) },
        },
        Share {
            id: 3,
            source: ShareSource::Preview,
            link: Some(ShareLink::preview(3)),
            error: None,
            relay_reconnecting: false,
            name: "周日的慢节奏".into(),
            contents: ShareContents::Tracks(12),
            size: Some("84 MB".into()),
            age: ShareAge::Yesterday,
            tone: Tone::Blue,
            symbol: Icon::Music,
            state: ShareState::Paused,
        },
    ]
}
pub fn demo_transfers() -> Vec<Transfer> {
    vec![
        Transfer {
            name: "Studio assets.zip",
            symbol: Icon::File,
            rate_mbps: 24.8,
            remaining_seconds: 16,
            peer: TransferPeer::From("Alex"),
            size_detail: "842 MB / 1.2 GB",
            progress: 0.68,
            tone: Tone::Mint,
            state: TransferState::Running,
        },
        Transfer {
            name: "Moodboard collection",
            symbol: Icon::Folder,
            rate_mbps: 12.4,
            remaining_seconds: 13,
            peer: TransferPeer::To("Jamie"),
            size_detail: "86 MB / 248 MB",
            progress: 0.35,
            tone: Tone::Lavender,
            state: TransferState::Running,
        },
        Transfer {
            name: "Summer sketches.pdf",
            symbol: Icon::File,
            rate_mbps: 0.0,
            remaining_seconds: 0,
            peer: TransferPeer::From("Sam"),
            size_detail: "18 MB",
            progress: 1.,
            tone: Tone::Peach,
            state: TransferState::Complete,
        },
    ]
}

#[derive(Clone, Copy)]
pub enum ShareContents {
    Selected(usize),
    Files(usize),
    FoldersAndFiles { folders: usize, files: usize },
    Photos(usize),
    Tracks(usize),
}
impl ShareContents {
    pub fn description(self, language: Language) -> String {
        match self {
            Self::Selected(count) => match language {
                Language::English => format!(
                    "{count} selected {}",
                    if count == 1 { "item" } else { "items" }
                ),
                Language::SimplifiedChinese => format!("已选择 {count} 项"),
            },
            Self::Files(count) => language.files(count),
            Self::FoldersAndFiles { folders, files } => language.folders_and_files(folders, files),
            Self::Photos(count) => language.photos(count),
            Self::Tracks(count) => language.tracks(count),
        }
    }
}
#[derive(Clone, Copy)]
pub enum ShareAge {
    CreatedAt(std::time::SystemTime),
    JustNow,
    MinutesAgo(u32),
    Yesterday,
}
impl ShareAge {
    pub fn description(self, language: Language) -> String {
        match self {
            Self::CreatedAt(at) => {
                let minutes = u32::try_from(at.elapsed().unwrap_or_default().as_secs() / 60)
                    .unwrap_or(u32::MAX);
                if minutes == 0 {
                    language.text(Text::JustNow).into()
                } else {
                    language.minutes_ago(minutes)
                }
            }
            Self::JustNow => language.text(Text::JustNow).to_owned(),
            Self::MinutesAgo(minutes) => language.minutes_ago(minutes),
            Self::Yesterday => language.text(Text::Yesterday).to_owned(),
        }
    }
}
#[derive(Clone, Copy)]
pub enum TransferPeer {
    From(&'static str),
    To(&'static str),
}
impl Transfer {
    pub fn detail(&self, language: Language) -> String {
        match self.peer {
            TransferPeer::From(name) => language.received_from(name, self.size_detail),
            TransferPeer::To(name) => language.sent_to(name, self.size_detail),
        }
    }
}
