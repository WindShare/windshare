use super::Language;
use crate::model::ShareError;
use std::{borrow::Cow, fmt::Write as _};

#[cfg(test)]
#[path = "formatting_tests.rs"]
mod tests;

const MAX_OMISSION_PREVIEW_ITEMS: usize = 2;

const fn is_unsafe_display_char(ch: char) -> bool {
    // Control chars, Unicode line/paragraph separators (U+2028..=U+2029), and
    // BiDi formatting controls can break card wrapping or reorder trailing labels.
    ch.is_control()
        || matches!(
            ch,
            '\u{061C}' | '\u{200E}'..='\u{200F}' | '\u{2028}'..='\u{202E}' | '\u{2066}'..='\u{2069}'
        )
}

fn sanitize_display_text(text: &str) -> Cow<'_, str> {
    if !text.chars().any(is_unsafe_display_char) {
        return Cow::Borrowed(text);
    }
    let mut sanitized = String::with_capacity(text.len());
    for ch in text.chars() {
        match ch {
            '\n' => sanitized.push_str(r"\n"),
            '\r' => sanitized.push_str(r"\r"),
            '\t' => sanitized.push_str(r"\t"),
            _ if is_unsafe_display_char(ch) => {
                let _ = write!(sanitized, "\\u{{{:04X}}}", u32::from(ch));
            }
            _ => sanitized.push(ch),
        }
    }
    Cow::Owned(sanitized)
}

fn item_display_name(path: &str) -> Option<Cow<'_, str>> {
    path.trim_end_matches(['/', '\\'])
        .rsplit(['/', '\\'])
        .next()
        .filter(|name| !name.is_empty())
        .map(sanitize_display_text)
}

fn path_segments(path: &str) -> Vec<&str> {
    let trimmed = path
        .strip_prefix(r"\\?\UNC\")
        .or_else(|| path.strip_prefix(r"\\?\"))
        .unwrap_or(path);
    trimmed
        .split(['/', '\\'])
        .filter(|segment| !segment.is_empty() && *segment != ".")
        .collect()
}

fn path_separator(path: &str) -> &'static str {
    if path.contains('\\') { "\\" } else { "/" }
}

fn distinguishing_display_names(first_path: &str, second_path: &str) -> Option<(String, String)> {
    let first = path_segments(first_path);
    let second = path_segments(second_path);
    if first.is_empty() || second.is_empty() {
        return None;
    }
    // Upstream osfs already proved the leaf names collide under Unicode 15
    // folding; expand from the right only when the parent paths actually differ.
    let first_parents = &first[..first.len() - 1];
    let second_parents = &second[..second.len() - 1];
    let extra_parents = if first_parents == second_parents {
        0
    } else {
        let common_suffix = first_parents
            .iter()
            .rev()
            .zip(second_parents.iter().rev())
            .take_while(|(left, right)| left == right)
            .count();
        common_suffix + 1
    };
    let format_tail = |segments: &[&str], sep: &str| {
        let keep = (extra_parents + 1).min(segments.len());
        let joined = segments[segments.len() - keep..].join(sep);
        sanitize_display_text(&joined).into_owned()
    };
    Some((
        format_tail(&first, path_separator(first_path)),
        format_tail(&second, path_separator(second_path)),
    ))
}

impl Language {
    pub fn share_error(self, error: &ShareError) -> String {
        match error {
            ShareError::CloudPlaceholder { path } => self.cloud_placeholder_error(path.as_deref()),
            ShareError::SourceUnsupported { path } => {
                self.source_unsupported_error(path.as_deref())
            }
            ShareError::SourceMissing { path } => self.source_missing_error(path.as_deref()),
            ShareError::SourceAccessDenied { path } => {
                self.source_access_denied_error(path.as_deref())
            }
            ShareError::SourceChanged { path } => self.source_changed_error(path.as_deref()),
            ShareError::SourceUnavailable { path } => {
                self.source_unavailable_error(path.as_deref())
            }
            ShareError::SelectionCollision {
                first_path,
                second_path,
            } => self.selection_collision_error(first_path, second_path),
            ShareError::Message(message) => message.clone(),
        }
    }

    fn selection_collision_error(self, first_path: &str, second_path: &str) -> String {
        match (self, distinguishing_display_names(first_path, second_path)) {
            (Self::English, Some((first, second))) => format!(
                "“{first}” and “{second}” have conflicting names. Rename one item or remove a duplicate before sharing."
            ),
            (Self::English, None) => {
                "Selected items have conflicting names. Rename one item or remove a duplicate before sharing."
                    .into()
            }
            (Self::SimplifiedChinese, Some((first, second))) => format!(
                "“{first}”与“{second}”名称冲突。请重命名其中一项或移除重复项后再分享。"
            ),
            (Self::SimplifiedChinese, None) => {
                "所选项目存在名称冲突。请重命名其中一项或移除重复项后再分享。".into()
            }
        }
    }

    fn cloud_placeholder_error(self, path: Option<&str>) -> String {
        match (self, path.and_then(item_display_name)) {
            (Self::English, Some(name)) => format!(
                "“{name}” isn’t fully downloaded on this device. Make it available on this device, then share it again."
            ),
            (Self::English, None) => {
                "This item isn’t fully downloaded on this device. Make it available on this device, then share it again.".into()
            }
            (Self::SimplifiedChinese, Some(name)) => format!(
                "“{name}”尚未完全下载到此设备。请在右键菜单选择“始终保留在此设备上”，待下载完成后再重新分享。"
            ),
            (Self::SimplifiedChinese, None) => {
                "所选项目尚未完全下载到此设备。请先选择“始终保留在此设备上”，待下载完成后再重新分享。".into()
            }
        }
    }

    fn source_unsupported_error(self, path: Option<&str>) -> String {
        match (self, path.and_then(item_display_name)) {
            (Self::English, Some(name)) => format!(
                "“{name}” can’t be shared safely. Select a regular file or folder instead of a symbolic link or junction."
            ),
            (Self::English, None) => {
                "This item can’t be shared safely. Select a regular file or folder instead of a symbolic link or junction.".into()
            }
            (Self::SimplifiedChinese, Some(name)) => format!(
                "无法安全分享“{name}”。请选择常规文件或文件夹，暂不支持符号链接或目录联接。"
            ),
            (Self::SimplifiedChinese, None) => {
                "无法安全分享所选项目。请选择常规文件或文件夹，暂不支持符号链接或目录联接。".into()
            }
        }
    }

    fn source_missing_error(self, path: Option<&str>) -> String {
        match (self, path.and_then(item_display_name)) {
            (Self::English, Some(name)) => {
                format!("“{name}” could not be found. Check that it still exists and try again.")
            }
            (Self::English, None) => {
                "The selected item could not be found. Check that it still exists and try again."
                    .into()
            }
            (Self::SimplifiedChinese, Some(name)) => {
                format!("找不到“{name}”。请确认文件或文件夹未被移动或删除后重试。")
            }
            (Self::SimplifiedChinese, None) => {
                "找不到所选项目。请确认文件或文件夹未被移动或删除后重试。".into()
            }
        }
    }

    fn source_access_denied_error(self, path: Option<&str>) -> String {
        match (self, path.and_then(item_display_name)) {
            (Self::English, Some(name)) => {
                format!("Permission denied for “{name}”. Check read permissions and try again.")
            }
            (Self::English, None) => {
                "Permission denied while opening the source. Check read permissions and try again."
                    .into()
            }
            (Self::SimplifiedChinese, Some(name)) => {
                format!("没有权限读取“{name}”。请检查读取权限后重试。")
            }
            (Self::SimplifiedChinese, None) => {
                "没有权限读取所选项目。请检查读取权限后重试。".into()
            }
        }
    }

    fn source_changed_error(self, path: Option<&str>) -> String {
        match (self, path.and_then(item_display_name)) {
            (Self::English, Some(name)) => {
                format!("“{name}” changed while preparing the share. Try sharing it again.")
            }
            (Self::English, None) => "The shared source changed during transfer.".into(),
            (Self::SimplifiedChinese, Some(name)) => {
                format!("准备分享时“{name}”发生了变化，请重新分享。")
            }
            (Self::SimplifiedChinese, None) => "传输过程中源文件发生了变化。".into(),
        }
    }

    fn source_unavailable_error(self, path: Option<&str>) -> String {
        match (self, path.and_then(item_display_name)) {
            (Self::English, Some(name)) => format!(
                "“{name}” is currently unavailable. Check that it isn’t locked or offline, then try again."
            ),
            (Self::English, None) => {
                "The source is currently unavailable. Check that it is accessible, then try again."
                    .into()
            }
            (Self::SimplifiedChinese, Some(name)) => {
                format!("暂时无法读取“{name}”。请确认文件未被占用且可访问后重试。")
            }
            (Self::SimplifiedChinese, None) => {
                "暂时无法读取所选项目。请确认文件未被占用且可访问后重试。".into()
            }
        }
    }

    pub fn source_omissions_notice(self, omissions: &windshare_host::SourceOmissions) -> String {
        let count = omissions.count;
        let header = match self {
            Self::English => format!(
                "{count} {} skipped so far",
                if count == 1 { "item" } else { "items" }
            ),
            Self::SimplifiedChinese => format!("目前已跳过 {count} 项"),
        };
        if omissions.samples.is_empty() {
            return header;
        }
        let separator = match self {
            Self::English => ", ",
            Self::SimplifiedChinese => "、",
        };
        let examples: Vec<String> = omissions
            .samples
            .iter()
            .take(MAX_OMISSION_PREVIEW_ITEMS)
            .map(|sample| {
                let name = sanitize_display_text(&sample.name);
                let reason = self.omission_reason_label(&sample.reason);
                match self {
                    Self::English => format!("{name} ({reason})"),
                    Self::SimplifiedChinese => format!("{name}（{reason}）"),
                }
            })
            .collect();
        let shown = u64::try_from(examples.len()).unwrap_or(u64::MAX);
        let remaining = count.saturating_sub(shown);
        let mut notice = format!("{header} · {}", examples.join(separator));
        if remaining > 0 {
            match self {
                Self::English => {
                    let _ = write!(notice, ", +{remaining} more");
                }
                Self::SimplifiedChinese => notice.push('等'),
            }
        }
        notice
    }

    fn omission_reason_label(self, reason: &str) -> &'static str {
        match (self, reason) {
            (Self::English, "cloud_placeholder") => "cloud-only",
            (Self::SimplifiedChinese, "cloud_placeholder") => "仅存云端",
            (Self::English, "symbolic_link") => "symbolic link",
            (Self::SimplifiedChinese, "symbolic_link") => "符号链接",
            (Self::English, "reparse_point") => "reparse point",
            (Self::SimplifiedChinese, "reparse_point") => "重解析点",
            (Self::English, "special_file") => "special file",
            (Self::SimplifiedChinese, "special_file") => "特殊文件",
            (Self::English, "unrepresentable_name") => "unsupported name",
            (Self::SimplifiedChinese, "unrepresentable_name") => "名称不受支持",
            (Self::English, "name_collision") => "name conflict",
            (Self::SimplifiedChinese, "name_collision") => "名称冲突",
            (Self::English, "access_denied") => "access denied",
            (Self::SimplifiedChinese, "access_denied") => "无访问权限",
            (Self::English, _) => "unavailable",
            (Self::SimplifiedChinese, _) => "暂不可用",
        }
    }

    pub fn relay_slot_usage(self, enabled: usize, limit: usize) -> String {
        match self {
            Self::English => format!("{enabled} / {limit} enabled"),
            Self::SimplifiedChinese => format!("已启用 {enabled} / {limit}"),
        }
    }

    pub fn nickname_next_change(self, at: &str) -> String {
        match self {
            Self::English => format!("Next nickname change · {at}"),
            Self::SimplifiedChinese => format!("下次可改昵称 · {at}"),
        }
    }

    pub fn code_sent_to(self, email: &str) -> String {
        match self {
            Self::English => format!("Enter the code sent to {email}."),
            Self::SimplifiedChinese => format!("请输入发送到 {email} 的验证码。"),
        }
    }

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
