use windshare_ui::{Icon, Tone};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Page {
    Share,
    Receive,
    Shares,
    Transfers,
    Settings,
    Components,
}
impl Page {
    pub const fn title(self) -> &'static str {
        match self {
            Self::Share => "分享文件",
            Self::Receive => "接收文件",
            Self::Shares => "我的分享",
            Self::Transfers => "传输",
            Self::Settings => "偏好设置",
            Self::Components => "设计组件",
        }
    }
    pub const fn subtitle(self) -> &'static str {
        match self {
            Self::Share => "选好文件，复制链接。分享就这么简单。",
            Self::Receive => "打开链接，把喜欢的文件带到你的设备。",
            Self::Shares => "已经分享的内容，都在这里。",
            Self::Transfers => "查看正在传输和已经完成的文件。",
            Self::Settings => "让 WindShare 更合你的习惯。",
            Self::Components => "WindShare 自有组件，直接基于 GPUI 构建。",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Filter {
    All,
    Live,
    Paused,
}
#[derive(Clone, Debug)]
pub enum ShareState {
    Live { viewers: usize },
    Paused,
}
#[derive(Clone)]
pub struct Share {
    pub id: usize,
    pub name: String,
    pub detail: &'static str,
    pub size: &'static str,
    pub date: &'static str,
    pub tone: Tone,
    pub symbol: Icon,
    pub state: ShareState,
}
impl Share {
    pub const fn live(&self) -> bool {
        matches!(self.state, ShareState::Live { .. })
    }
    pub fn link(&self) -> String {
        format!("https://windshare.example/s/preview-{}", self.id)
    }
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
    pub detail: &'static str,
    pub progress: f32,
    pub tone: Tone,
    pub state: TransferState,
}
#[derive(Clone, Debug)]
pub enum Overlay {
    Detail(usize),
    Help,
}
#[derive(Clone, Copy, Debug)]
pub enum PreviewAction {
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
    ToggleShare(usize),
    ToggleTransfer(usize),
    ToggleGrid,
    Close,
    Help,
    ToggleSetting(usize),
    Toast(&'static str),
}
pub fn demo_shares() -> Vec<Share> {
    vec![
        Share {
            id: 1,
            name: "品牌设计素材".into(),
            detail: "24 个文件",
            size: "128 MB",
            date: "刚刚",
            tone: Tone::Lavender,
            symbol: Icon::Folder,
            state: ShareState::Live { viewers: 2 },
        },
        Share {
            id: 2,
            name: "京都的周末".into(),
            detail: "86 张照片",
            size: "1.4 GB",
            date: "12 分钟前",
            tone: Tone::Peach,
            symbol: Icon::Image,
            state: ShareState::Live { viewers: 1 },
        },
        Share {
            id: 3,
            name: "周日的慢节奏".into(),
            detail: "12 首音乐",
            size: "84 MB",
            date: "昨天",
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
            detail: "来自 Alex · 842 MB / 1.2 GB",
            progress: 0.68,
            tone: Tone::Mint,
            state: TransferState::Running,
        },
        Transfer {
            name: "Moodboard collection",
            symbol: Icon::Folder,
            rate_mbps: 12.4,
            remaining_seconds: 13,
            detail: "发送给 Jamie · 86 MB / 248 MB",
            progress: 0.35,
            tone: Tone::Lavender,
            state: TransferState::Running,
        },
        Transfer {
            name: "Summer sketches.pdf",
            symbol: Icon::File,
            rate_mbps: 0.0,
            remaining_seconds: 0,
            detail: "来自 Sam · 18 MB",
            progress: 1.,
            tone: Tone::Peach,
            state: TransferState::Complete,
        },
    ]
}
