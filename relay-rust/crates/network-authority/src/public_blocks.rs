use crate::{
    Authority, Environment, Store,
    restriction_store::BlockList,
    store::{key, load, save},
};
use anyhow::{Result, anyhow};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use windshare_relay_access::{restrictions::Restriction, usage::ShareAttribution};

const PUBLIC_BLOCKS_KEY: &str = "public-blocks";
impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn remember_shares(&mut self, shares: &[ShareAttribution]) -> Result<()> {
        let mut changed = false;
        for entry in shares {
            let record = format!(
                "{}/{}",
                key("attribution", &entry.account),
                URL_SAFE_NO_PAD.encode(entry.share)
            );
            if self.store.get(&record)?.is_none() {
                // A capability keeps its attribution after a relay disconnects.
                // Remembering the association makes a later account block effective
                // at web entrypoints without querying authority for every transfer.
                save(&mut self.store, &record, &entry.share)?;
                changed |= self.restricted(Restriction::Account(entry.account))?;
            }
        }
        if changed {
            self.update_public_blocks()?;
        }
        Ok(())
    }

    pub(crate) fn update_public_blocks(&mut self) -> Result<()> {
        let restrictions = self.blocks()?;
        let current = load::<BlockList>(&self.store, PUBLIC_BLOCKS_KEY)?.unwrap_or_default();
        let mut shares = restrictions.shares;
        for account in &restrictions.accounts {
            let prefix = format!("{}/", key("attribution", account));
            for (_, value) in self.store.scan(&prefix, None)? {
                shares.insert(serde_json::from_str(&value)?);
            }
        }
        if shares != current.shares || restrictions.accounts != current.accounts {
            let next = BlockList {
                version: current
                    .version
                    .checked_add(1)
                    .ok_or_else(|| anyhow!("public block version exhausted"))?,
                shares,
                accounts: restrictions.accounts,
            };
            save(&mut self.store, PUBLIC_BLOCKS_KEY, &next)?;
        }
        Ok(())
    }
}

#[cfg(any(test, target_arch = "wasm32"))]
mod feed {
    use super::{Authority, BlockList, Environment, PUBLIC_BLOCKS_KEY, Store};
    use anyhow::Result;
    use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
    use sha2::{Digest, Sha256};
    use windshare_relay_access::trust;

    // Re-sign with half of the browser's one-hour freshness window to spare.
    pub const RENEW_AFTER_SECONDS: u64 = 30 * 60;

    #[derive(Default)]
    pub struct PublicBlockFeed {
        snapshot: Option<PublicBlockSnapshot>,
    }

    pub struct PublicBlockSnapshot {
        source: Option<String>,
        certificate: String,
        issued_at: u64,
        pub body: String,
        pub etag: String,
    }

    impl PublicBlockSnapshot {
        pub fn not_modified(&self, validator: &str) -> bool {
            validator
                .split(',')
                .map(str::trim)
                .any(|tag| tag == "*" || tag.strip_prefix("W/").unwrap_or(tag) == self.etag)
        }
    }

    impl PublicBlockFeed {
        pub fn snapshot<R: Store, E: Environment>(
            &mut self,
            authority: &Authority<R, E>,
        ) -> Result<&PublicBlockSnapshot> {
            // Read the committed projection on each request so another route's
            // policy change invalidates the cache immediately.
            let source = authority.store.get(PUBLIC_BLOCKS_KEY)?;
            let now = authority.environment.now();
            let certificate = &authority.signing.certificate;
            if self.snapshot.as_ref().is_none_or(|cached| {
                cached.source != source
                    || cached.certificate != *certificate
                    || now < cached.issued_at
                    || now - cached.issued_at >= RENEW_AFTER_SECONDS
            }) {
                let blocks: BlockList = source
                    .as_deref()
                    .map(serde_json::from_str)
                    .transpose()?
                    .unwrap_or_default();
                let payload = serde_json::to_vec(&serde_json::json!({
                    "version": blocks.version, "shares": blocks.shares,
                    "accounts": blocks.accounts, "issued_at": now,
                }))?;
                let signed = trust::sign(
                    b"windshare/public-blocks/v1\0",
                    &payload,
                    &authority.signing.key,
                );
                let body = serde_json::json!({
                    "certificate": certificate, "body": URL_SAFE_NO_PAD.encode(signed),
                })
                .to_string();
                let etag = format!(
                    "\"{}\"",
                    URL_SAFE_NO_PAD.encode(Sha256::digest(body.as_bytes()))
                );
                tracing::debug!(
                    event = "public_block_snapshot_signed",
                    generation = authority.signing.generation.number,
                    version = blocks.version,
                    issued_at = now,
                );
                self.snapshot = Some(PublicBlockSnapshot {
                    source,
                    certificate: certificate.clone(),
                    issued_at: now,
                    body,
                    etag,
                });
            }
            Ok(self
                .snapshot
                .as_ref()
                .expect("signed public block snapshot"))
        }
    }
}

#[cfg(any(test, target_arch = "wasm32"))]
pub use feed::PublicBlockFeed;
#[cfg(test)]
pub use feed::RENEW_AFTER_SECONDS;
