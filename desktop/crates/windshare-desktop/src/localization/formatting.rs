use super::Language;

impl Language {
    pub fn files(self, count: usize) -> String {
        match self {
            Self::English => format!("{count} {}", if count == 1 { "file" } else { "files" }),
            Self::SimplifiedChinese => format!("{count} 个文件"),
        }
    }

    pub fn folders_and_files(self, folders: usize, files: usize) -> String {
        let files = self.files(files);
        match self {
            Self::English => format!(
                "{folders} {} · {files}",
                if folders == 1 { "folder" } else { "folders" }
            ),
            Self::SimplifiedChinese => format!("{folders} 个文件夹 · {files}"),
        }
    }

    pub fn photos(self, count: usize) -> String {
        match self {
            Self::English => format!("{count} {}", if count == 1 { "photo" } else { "photos" }),
            Self::SimplifiedChinese => format!("{count} 张照片"),
        }
    }

    pub fn tracks(self, count: usize) -> String {
        match self {
            Self::English => format!("{count} {}", if count == 1 { "track" } else { "tracks" }),
            Self::SimplifiedChinese => format!("{count} 首音乐"),
        }
    }

    pub fn viewers(self, count: usize) -> String {
        match self {
            Self::English => format!("{count} receiving"),
            Self::SimplifiedChinese => format!("{count} 人正在接收"),
        }
    }

    pub fn shares(self, count: usize) -> String {
        match self {
            Self::English => format!("{count} {}", if count == 1 { "share" } else { "shares" }),
            Self::SimplifiedChinese => format!("共 {count} 个分享"),
        }
    }

    pub fn active_transfers(self, count: usize) -> String {
        match self {
            Self::English => format!("{count} in progress"),
            Self::SimplifiedChinese => format!("{count} 项进行中"),
        }
    }

    pub fn minutes_ago(self, minutes: u32) -> String {
        match self {
            Self::English => format!(
                "{minutes} {} ago",
                if minutes == 1 { "minute" } else { "minutes" }
            ),
            Self::SimplifiedChinese => format!("{minutes} 分钟前"),
        }
    }

    pub fn created(self, age: &str) -> String {
        match self {
            Self::English => format!("Created {age}"),
            Self::SimplifiedChinese => format!("创建于 {age}"),
        }
    }

    pub fn received_from(self, name: &str, detail: &str) -> String {
        match self {
            Self::English => format!("From {name} · {detail}"),
            Self::SimplifiedChinese => format!("来自 {name} · {detail}"),
        }
    }

    pub fn sent_to(self, name: &str, detail: &str) -> String {
        match self {
            Self::English => format!("To {name} · {detail}"),
            Self::SimplifiedChinese => format!("发送给 {name} · {detail}"),
        }
    }

    pub fn save_to_downloads(self, name: &str) -> String {
        match self {
            Self::English => format!("Save to Downloads / {name}"),
            Self::SimplifiedChinese => format!("保存到 下载 / {name}"),
        }
    }

    pub fn receive_files(self, count: usize) -> String {
        let files = self.files(count);
        match self {
            Self::English => format!("Receive {files}"),
            Self::SimplifiedChinese => format!("接收这 {files}"),
        }
    }

    pub fn transfer_rate(self, mbps: f32, remaining_seconds: u32) -> String {
        match self {
            Self::English => format!("{mbps:.1} MB/s · {remaining_seconds} s left"),
            Self::SimplifiedChinese => format!("{mbps:.1} MB/s · 剩余 {remaining_seconds} 秒"),
        }
    }
}
