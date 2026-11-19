use crate::{
    Authority, Environment, Store,
    store::{key, load, save},
};
use anyhow::{Result, anyhow};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use windshare_relay_access::restrictions::{PAGE_ENTRIES, PageResult, Restriction, Restrictions};

const VERSION: &str = "restriction-version";
const ENTRIES: &str = "restriction/";
const ACCOUNTS: &str = "restriction/account";
const SHARES: &str = "restriction/share";
const CREDENTIALS: &str = "restriction/credential";

#[derive(Default, Serialize, Deserialize)]
pub struct BlockList {
    pub version: u64,
    pub shares: BTreeSet<[u8; 12]>,
    pub accounts: BTreeSet<[u8; 16]>,
}

fn entry_key(entry: Restriction) -> String {
    match entry {
        Restriction::Account(id) => key(ACCOUNTS, &id),
        Restriction::Share(id) => key(SHARES, &id),
        Restriction::Credential(id) => key(CREDENTIALS, &id),
    }
}

pub fn migrate(store: &mut impl Store) -> Result<()> {
    let Some(legacy) = load::<Restrictions>(store, "restrictions")? else {
        return Ok(());
    };
    for entry in legacy
        .accounts
        .into_iter()
        .map(Restriction::Account)
        .chain(legacy.shares.into_iter().map(Restriction::Share))
        .chain(legacy.credentials.into_iter().map(Restriction::Credential))
    {
        save(store, &entry_key(entry), &entry)?;
    }
    save(store, VERSION, &legacy.version)?;
    store.delete("restrictions")
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn restriction_version(&self) -> Result<u64> {
        Ok(load(&self.store, VERSION)?.unwrap_or(0))
    }

    pub(crate) fn restricted(&self, entry: Restriction) -> Result<bool> {
        Ok(self.store.get(&entry_key(entry))?.is_some())
    }

    pub(crate) fn set_restriction(&mut self, entry: Restriction, blocked: bool) -> Result<bool> {
        if self.restricted(entry)? == blocked {
            return Ok(false);
        }
        let version = self
            .restriction_version()?
            .checked_add(1)
            .ok_or_else(|| anyhow!("restriction version exhausted"))?;
        let key = entry_key(entry);
        if blocked {
            save(&mut self.store, &key, &entry)?;
        } else {
            self.store.delete(&key)?;
        }
        save(&mut self.store, VERSION, &version)?;
        tracing::debug!(
            event = "restriction_version_advanced",
            ?entry,
            blocked,
            version
        );
        Ok(true)
    }

    pub(crate) fn restriction_page_at(
        &self,
        version: u64,
        after: Option<Restriction>,
    ) -> Result<PageResult> {
        let current = self.restriction_version()?;
        if current != version {
            return Ok(PageResult::Superseded { version: current });
        }
        let after = after.map(entry_key);
        let mut rows = self
            .store
            .scan_after(ENTRIES, after.as_deref(), Some(PAGE_ENTRIES + 1))?;
        let more = rows.len() > PAGE_ENTRIES;
        rows.truncate(PAGE_ENTRIES);
        let entries = rows
            .into_iter()
            .map(|(_, value)| Ok(serde_json::from_str(&value)?))
            .collect::<Result<_>>()?;
        Ok(PageResult::Page { entries, more })
    }

    pub(crate) fn blocks(&self) -> Result<BlockList> {
        let mut blocks = BlockList {
            version: self.restriction_version()?,
            ..BlockList::default()
        };
        for prefix in [ACCOUNTS, SHARES] {
            for (_, value) in self.store.scan(&format!("{prefix}/"), None)? {
                match serde_json::from_str(&value)? {
                    Restriction::Account(id) => {
                        blocks.accounts.insert(id);
                    }
                    Restriction::Share(id) => {
                        blocks.shares.insert(id);
                    }
                    Restriction::Credential(_) => return Err(anyhow!("invalid block entry")),
                }
            }
        }
        Ok(blocks)
    }
}
