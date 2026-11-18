use crate::{
    Authority, Environment, Store,
    store::{key, load, save},
};
use anyhow::{Result, anyhow};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use windshare_relay_access::{trust, usage::ShareAttribution};

#[derive(Default, Serialize, Deserialize)]
struct Snapshot {
    version: u64,
    shares: BTreeSet<[u8; 12]>,
    accounts: BTreeSet<[u8; 16]>,
}
impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn remember_shares(&mut self, shares: &[ShareAttribution]) -> Result<()> {
        let restrictions = self.restrictions()?;
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
                changed |= restrictions.accounts.contains(&entry.account);
            }
        }
        if changed {
            self.update_public_blocks()?;
        }
        Ok(())
    }

    pub(crate) fn update_public_blocks(&mut self) -> Result<()> {
        let restrictions = self.restrictions()?;
        let current = load::<Snapshot>(&self.store, "public-blocks")?.unwrap_or_default();
        let mut shares = restrictions.shares;
        for account in &restrictions.accounts {
            let prefix = format!("{}/", key("attribution", account));
            for (_, value) in self.store.scan(&prefix, None)? {
                shares.insert(serde_json::from_str(&value)?);
            }
        }
        if shares != current.shares || restrictions.accounts != current.accounts {
            let next = Snapshot {
                version: current
                    .version
                    .checked_add(1)
                    .ok_or_else(|| anyhow!("public block version exhausted"))?,
                shares,
                accounts: restrictions.accounts,
            };
            save(&mut self.store, "public-blocks", &next)?;
        }
        Ok(())
    }

    /// # Errors
    /// Propagates failure to load or sign the public restriction snapshot.
    pub fn public_blocks(&self) -> Result<serde_json::Value> {
        let snapshot = load::<Snapshot>(&self.store, "public-blocks")?.unwrap_or_default();
        let payload = serde_json::to_vec(&serde_json::json!({
            "version": snapshot.version, "shares": snapshot.shares,
            "accounts": snapshot.accounts, "issued_at": self.environment.now(),
        }))?;
        Ok(serde_json::json!({"certificate":self.signing.certificate,
            "body":URL_SAFE_NO_PAD.encode(trust::sign(b"windshare/public-blocks/v1\0",&payload,&self.signing.key))}))
    }
}
