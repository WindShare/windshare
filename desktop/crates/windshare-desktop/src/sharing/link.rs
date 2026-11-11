use gpui::SharedString;
use windshare_ui::{QrCodeError, QrCodeImage};

const PREVIEW_ORIGIN: &str = "https://windshare.example";
const PREVIEW_SHARE_ID: &str = "tVW-68OSeLBZTpU-";
const PREVIEW_KEY: &str = "AgABAgMEBQYHCAkKCwwNDg8kQqCgOKPbpFQm33t9wk_E";
const PREVIEW_RELAY_QUERY: &str = "r=https%3A%2F%2Frelay.example.com%3A8443";

/// Display elision is a view concern: copying and encoding always use this entire value.
#[derive(Clone)]
pub struct ShareLink {
    url: SharedString,
    qr: Result<QrCodeImage, QrCodeError>,
}

impl ShareLink {
    pub fn new(url: impl Into<SharedString>) -> Self {
        let url = url.into();
        let qr = QrCodeImage::encode(&url);
        Self { url, qr }
    }

    pub fn preview(id: usize) -> Self {
        // The public identity from core/testvectors/v2-identity.json is a fixture, not a
        // new cryptographic implementation. A preview-only frontend path distinguishes samples.
        let link = Self::new(format!(
            "{PREVIEW_ORIGIN}/s/preview-{id}/{PREVIEW_SHARE_ID}?{PREVIEW_RELAY_QUERY}#{PREVIEW_KEY}"
        ));
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
        link
    }

    pub fn url(&self) -> SharedString {
        self.url.clone()
    }

    pub fn qr(&self) -> Result<&QrCodeImage, QrCodeError> {
        self.qr.as_ref().map_err(|error| *error)
    }
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
