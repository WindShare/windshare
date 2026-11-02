use gpui::{Svg, prelude::*, px, svg};

#[derive(Clone, Copy, Debug)]
pub enum Icon {
    Wind,
    Home,
    Folder,
    Download,
    Upload,
    Activity,
    Settings,
    Help,
    Plus,
    Arrow,
    ArrowUpRight,
    Chevron,
    Link,
    Copy,
    Check,
    Close,
    More,
    Shield,
    Globe,
    Search,
    Pause,
    Play,
    File,
    Image,
    Music,
    Monitor,
    Sun,
    Moon,
    Bell,
    Grid,
    List,
    Lock,
    Minimize,
    Maximize,
    Refresh,
    External,
    Sliders,
    Spark,
    Heart,
}

impl Icon {
    pub const fn name(self) -> &'static str {
        match self {
            Self::Wind => "wind",
            Self::Home => "home",
            Self::Folder => "folder",
            Self::Download => "download",
            Self::Upload => "upload",
            Self::Activity => "activity",
            Self::Settings => "settings",
            Self::Help => "help",
            Self::Plus => "plus",
            Self::Arrow => "arrow",
            Self::ArrowUpRight => "arrow-up-right",
            Self::Chevron => "chevron",
            Self::Link => "link",
            Self::Copy => "copy",
            Self::Check => "check",
            Self::Close => "close",
            Self::More => "more",
            Self::Shield => "shield",
            Self::Globe => "globe",
            Self::Search => "search",
            Self::Pause => "pause",
            Self::Play => "play",
            Self::File => "file",
            Self::Image => "image",
            Self::Music => "music",
            Self::Monitor => "monitor",
            Self::Sun => "sun",
            Self::Moon => "moon",
            Self::Bell => "bell",
            Self::Grid => "grid",
            Self::List => "list",
            Self::Lock => "lock",
            Self::Minimize => "minimize",
            Self::Maximize => "maximize",
            Self::Refresh => "refresh",
            Self::External => "external",
            Self::Sliders => "sliders",
            Self::Spark => "spark",
            Self::Heart => "heart",
        }
    }
}

pub fn icon(name: Icon, size: f32) -> Svg {
    svg()
        .path(format!("icons/{}", name.name()))
        .size(px(size))
        .flex_shrink_0()
        .text_color(crate::ink())
}
