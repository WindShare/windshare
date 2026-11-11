use gpui::{AssetSource, SharedString};
use std::borrow::Cow;

pub struct Assets;

impl AssetSource for Assets {
    fn load(&self, path: &str) -> anyhow::Result<Option<Cow<'static, [u8]>>> {
        let Some(name) = path.strip_prefix("icons/") else {
            return Ok(None);
        };
        let body = match name {
            "wind" => {
                r#"<path d="M3 8h10c5 0 5-6 1-6-2 0-3 1-3 2M2 12h16c5 0 5-6 1-6M4 16h9c5 0 5 6 1 6-2 0-3-1-3-2"/>"#
            }
            "home" => r#"<path d="m3 10 9-7 9 7v10H3Z"/><path d="M9 20v-7h6v7"/>"#,
            "folder" => r#"<path d="M3 7V5h7l2 3h9v12H3V7Z"/>"#,
            "download" => r#"<path d="M12 3v12m-5-5 5 5 5-5M4 15v6h16v-6"/>"#,
            "upload" => r#"<path d="M12 16V4m-5 5 5-5 5 5M4 15v6h16v-6"/>"#,
            "activity" => r#"<path d="M2 12h5l3-8 4 16 3-8h5"/>"#,
            "settings" => {
                r#"<path d="m9 3-1 3-3 1-2 4 2 2 1 4 4 2 3-1 4 1 3-4-1-3 1-4-4-3-3 1Z"/><circle cx="12" cy="12" r="3"/>"#
            }
            "help" => {
                r#"<circle cx="12" cy="12" r="9"/><path d="M9 9a3 3 0 1 1 5 2c-2 1-2 1-2 3m0 3h.01"/>"#
            }
            "plus" => r#"<path d="M12 5v14M5 12h14"/>"#,
            "arrow" => r#"<path d="M4 12h15m-6-6 6 6-6 6"/>"#,
            "arrow-up-right" => r#"<path d="M6 18 18 6M6 6h12v12"/>"#,
            "chevron" => r#"<path d="m8 5 7 7-7 7"/>"#,
            "chevron-down" => r#"<path d="m5 9 7 7 7-7"/>"#,
            "chevron-up" => r#"<path d="m5 15 7-7 7 7"/>"#,
            "expand" => r#"<path d="M8 3H3v5m13-5h5v5M3 16v5h5m8 0h5v-5"/>"#,
            "link" => {
                r#"<path d="m10 13 4-4m-7 6-1 1a4 4 0 0 0 6 6l4-4a4 4 0 0 0 0-6M17 9l1-1a4 4 0 0 0-6-6L8 6a4 4 0 0 0 0 6" transform="translate(0 -1)"/>"#
            }
            "copy" => {
                r#"<rect x="8" y="8" width="12" height="13" rx="2"/><path d="M16 8V3H3v13h5"/>"#
            }
            "check" => r#"<path d="m5 12 4 4L19 6"/>"#,
            "close" => r#"<path d="m6 6 12 12M6 18 18 6"/>"#,
            "more" => {
                r#"<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>"#
            }
            "shield" => {
                r#"<path d="m12 2 8 3v7c0 5-8 10-8 10S4 17 4 12V5Z"/><path d="m8 11 3 3 5-5"/>"#
            }
            "globe" => {
                r#"<circle cx="12" cy="12" r="9"/><ellipse cx="12" cy="12" rx="4" ry="9"/><path d="M3 12h18"/>"#
            }
            "search" => r#"<circle cx="10" cy="10" r="6"/><path d="m15 15 6 6"/>"#,
            "pause" => r#"<path d="M8 5v14M16 5v14"/>"#,
            "play" => r#"<path d="m8 4 12 8-12 8Z"/>"#,
            "file" => r#"<path d="M5 2h9l5 5v15H5Zm9 0v6h5M8 13h8M8 17h6"/>"#,
            "image" => {
                r#"<rect x="3" y="3" width="18" height="18" rx="3"/><circle cx="8" cy="8" r="1.5"/><path d="m3 17 6-6 4 4 3-3 5 5"/>"#
            }
            "music" => {
                r#"<path d="M9 18V5l12-2v13M9 9l12-2"/><ellipse cx="6" cy="18" rx="3" ry="3"/><ellipse cx="18" cy="16" rx="3" ry="3"/>"#
            }
            "monitor" => {
                r#"<rect x="2" y="3" width="20" height="14" rx="2"/><path d="M12 17v5M7 22h10"/>"#
            }
            "sun" => {
                r#"<circle cx="12" cy="12" r="4"/><path d="M12 1v2m0 18v2M1 12h2m18 0h2M4 4l2 2m12 12 2 2M4 20l2-2M18 6l2-2"/>"#
            }
            "moon" => r#"<path d="M20 16A9 9 0 0 1 8 3a9 9 0 1 0 12 13Z"/>"#,
            "bell" => r#"<path d="M6 10a6 6 0 0 1 12 0v6l2 2H4l2-2Zm4 11h4"/>"#,
            "grid" => {
                r#"<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/>"#
            }
            "list" => r#"<path d="M8 5h13M8 12h13M8 19h13M3 5h.01M3 12h.01M3 19h.01"/>"#,
            "lock" => {
                r#"<rect x="5" y="10" width="14" height="11" rx="2"/><path d="M8 10V6a4 4 0 0 1 8 0v4m-4 5v2"/>"#
            }
            "minimize" => r#"<path d="M5 12h14"/>"#,
            "maximize" => r#"<rect x="5" y="5" width="14" height="14" rx="1"/>"#,
            "restore" => {
                r#"<path d="M9 5V3h12v12h-2"/><rect x="3" y="9" width="12" height="12" rx="1"/>"#
            }
            "refresh" => r#"<path d="M20 8a9 9 0 1 0 0 8M20 2v6h-6"/>"#,
            "external" => r#"<path d="M14 3h7v7M21 3 10 14M10 3H3v18h18v-7"/>"#,
            "sliders" => {
                r#"<path d="M3 6h5m4 0h9M3 18h9m4 0h5"/><circle cx="10" cy="6" r="2"/><circle cx="14" cy="18" r="2"/>"#
            }
            "spark" => r#"<path d="m12 2 3 7 7 3-7 3-3 7-3-7-7-3 7-3Z"/>"#,
            "heart" => r#"<path d="M12 21 3 12C-2 5 7-1 12 6c5-7 14-1 9 6Z"/>"#,
            _ => return Ok(None),
        };
        Ok(Some(Cow::Owned(format!(
            r#"<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="white" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round">{body}</svg>"#
        ).into_bytes())))
    }
    fn list(&self, _: &str) -> anyhow::Result<Vec<SharedString>> {
        Ok(vec![])
    }
}
