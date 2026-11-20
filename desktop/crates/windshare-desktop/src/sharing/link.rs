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
    Standard,
    Backup,
    Diagnostic,
}

#[derive(Clone)]
pub struct SharePublication {
    standard: ShareLink,
    backup: Option<SharedString>,
    diagnostic: OnceCell<Option<ShareLink>>,
}

impl SharePublication {
    pub fn new(url: impl Into<SharedString>, backup: Option<SharedString>) -> Self {
        let standard_url: SharedString = url.into();
        let backup = backup.filter(|candidate| !candidate.is_empty() && candidate != &standard_url);
        Self {
            standard: ShareLink::new(standard_url),
            backup,
            diagnostic: OnceCell::new(),
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
        let link = &publication.standard;
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
        self.standard.url.as_ref() == url && self.backup.as_deref() == normalized
    }

    pub const fn standard(&self) -> &ShareLink {
        &self.standard
    }

    pub const fn has_backup(&self) -> bool {
        self.backup.is_some()
    }

    pub fn diagnostic(&self) -> Option<&ShareLink> {
        self.diagnostic
            .get_or_init(|| {
                browser_trace_url(&self.standard.url).map(|url| ShareLink::new(url.into()))
            })
            .as_ref()
    }

    pub fn url(&self, kind: LinkKind) -> Option<SharedString> {
        match kind {
            LinkKind::Standard => Some(self.standard.url()),
            LinkKind::Backup => self.backup.clone(),
            LinkKind::Diagnostic => self.diagnostic().map(ShareLink::url),
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
    // so generating a diagnostic link cannot change its sharing capability.
    let (page, key) = url.split_once('#')?;
    let separator = match page.split_once('?') {
        Some((_, "")) => "",
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
