use gpui::SharedString;
use std::cell::OnceCell;
use windshare_ui::{QrCodeError, QrCodeImage};

const BROWSER_TRACE_QUERY: &str = "trace=1";

const PREVIEW_ORIGIN: &str = "https://windshare.example";
const PREVIEW_BACKUP_ORIGIN: &str = "https://backup.windshare.example";
const PREVIEW_SHARE_ID: &str = "tVW-68OSeLBZTpU-";
const PREVIEW_KEY: &str = "AgABAgMEBQYHCAkKCwwNDg8kQqCgOKPbpFQm33t9wk_E";
const PREVIEW_RELAY_QUERY: &str = "r=https%3A%2F%2Frelay.example.com%3A8443";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LinkKind {
    Primary,
    Backup,
}

#[derive(Clone)]
pub struct SharePublication {
    primary: ShareLink,
    backup: Option<SharedString>,
    traced_primary: OnceCell<Option<ShareLink>>,
}

impl SharePublication {
    pub fn new(url: impl Into<SharedString>, backup: Option<SharedString>) -> Self {
        let primary_url: SharedString = url.into();
        let backup = backup.filter(|candidate| !candidate.is_empty() && candidate != &primary_url);
        Self {
            primary: ShareLink::new(primary_url),
            backup,
            traced_primary: OnceCell::new(),
        }
    }

    pub fn preview(id: usize) -> Self {
        // The public identity from core/testvectors/v2-identity.json is a fixture, not a
        // new cryptographic implementation. A preview-only frontend path distinguishes samples.
        let path =
            format!("/s/preview-{id}/{PREVIEW_SHARE_ID}?{PREVIEW_RELAY_QUERY}#{PREVIEW_KEY}");
        let publication = Self::new(
            format!("{PREVIEW_ORIGIN}{path}"),
            Some(format!("{PREVIEW_BACKUP_ORIGIN}{path}").into()),
        );
        let link = &publication.primary;
        match &link.qr {
            Ok(code) => tracing::info!(
                event = "share.qr_prepared",
                session = std::process::id(),
                share_id = id,
                link_bytes = link.url.len(),
                modules = code.modules_with_margin(),
                preview = true,
            ),
            Err(error) => tracing::warn!(
                event = "share.qr_unavailable",
                session = std::process::id(),
                share_id = id,
                link_bytes = link.url.len(),
                %error,
            ),
        }
        publication
    }

    pub fn matches(&self, url: &str, backup: Option<&str>) -> bool {
        let normalized = backup.filter(|candidate| !candidate.is_empty() && *candidate != url);
        self.primary.url.as_ref() == url && self.backup.as_deref() == normalized
    }

    pub fn primary(&self, browser_trace: bool) -> &ShareLink {
        if browser_trace && let Some(traced) = self.traced_primary() {
            traced
        } else {
            &self.primary
        }
    }

    pub fn is_traced(&self, browser_trace: bool) -> bool {
        browser_trace && self.traced_primary().is_some()
    }

    pub const fn has_backup(&self) -> bool {
        self.backup.is_some()
    }

    pub fn is_backup_traced(&self, browser_trace: bool) -> bool {
        browser_trace && self.backup.as_deref().and_then(browser_trace_url).is_some()
    }

    fn traced_primary(&self) -> Option<&ShareLink> {
        self.traced_primary
            .get_or_init(|| {
                browser_trace_url(&self.primary.url).map(|url| ShareLink::new(url.into()))
            })
            .as_ref()
    }

    fn backup_url(&self, browser_trace: bool) -> Option<SharedString> {
        let url = self.backup.as_ref()?;
        if browser_trace && let Some(traced) = browser_trace_url(url) {
            Some(traced.into())
        } else {
            Some(url.clone())
        }
    }

    pub fn url(&self, kind: LinkKind, browser_trace: bool) -> Option<SharedString> {
        match kind {
            LinkKind::Primary => Some(self.primary(browser_trace).url()),
            LinkKind::Backup => self.backup_url(browser_trace),
        }
    }
}

/// Display elision is a view concern: copying and encoding always use this entire value.
#[derive(Clone)]
pub struct ShareLink {
    url: SharedString,
    qr: Result<QrCodeImage, QrCodeError>,
}

impl ShareLink {
    fn new(url: SharedString) -> Self {
        let qr = QrCodeImage::encode(&url);
        Self { url, qr }
    }

    pub fn url(&self) -> SharedString {
        self.url.clone()
    }

    pub fn qr(&self) -> Result<&QrCodeImage, QrCodeError> {
        self.qr.as_ref().map_err(|error| *error)
    }
}

fn browser_trace_url(url: &str) -> Option<String> {
    // Preserve the relay spelling and keep the decryption key in the fragment
    // so enabling browser diagnostics cannot change a share's capability.
    let (page, key) = url.split_once('#')?;
    let separator = match page.split_once('?') {
        Some((_, "")) => "",
        Some((_, query)) if query.split('&').any(|part| part == BROWSER_TRACE_QUERY) => {
            return Some(url.to_owned());
        }
        Some(_) => "&",
        None => "?",
    };
    Some(format!("{page}{separator}{BROWSER_TRACE_QUERY}#{key}"))
}

#[derive(Clone, Copy, Default)]
pub enum LinkDisclosure {
    #[default]
    Summary,
    Expanded(usize),
}

impl LinkDisclosure {
    pub const fn expanded(self, share_id: usize) -> bool {
        matches!(self, Self::Expanded(id) if id == share_id)
    }

    pub const fn toggle(&mut self, share_id: usize) {
        *self = if self.expanded(share_id) {
            Self::Summary
        } else {
            Self::Expanded(share_id)
        };
    }
}

#[cfg(test)]
#[path = "link_tests.rs"]
mod tests;
