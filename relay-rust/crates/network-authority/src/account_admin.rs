use crate::{
    Account, Authority, Environment, Fault, Store,
    accounts::DailyUsageRecord,
    registration::username,
    store::{key, load},
};
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::Serialize;
use std::cmp::Reverse;
use windshare_relay_access::{AccountId, entitlement::DAY_SECONDS, restrictions::Restriction};

const ACCOUNT_INDEX: &str = "account-index/";
const ACCOUNT_PAGE_SIZE: usize = 50;
const USAGE_DAYS: usize = 30;
const SHARE_LIMIT: usize = 100;

#[derive(Debug, Serialize)]
pub struct AccountSummary {
    pub id: AccountId,
    pub created_at: u64,
    pub username: Option<String>,
    pub nickname: Option<String>,
    pub email: Option<String>,
    pub has_password: bool,
    pub today_bytes: u64,
}

#[derive(Debug, Serialize)]
pub struct AccountPage {
    pub today_day: u64,
    pub entries: Vec<AccountSummary>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct DailyUsage {
    pub day: u64,
    pub bytes: u64,
}

#[derive(Debug, Serialize)]
pub struct AccountDetails {
    #[serde(flatten)]
    pub summary: AccountSummary,
    pub today_day: u64,
    pub daily_limit_bytes: u64,
    pub remaining_bytes: u64,
    pub restricted: bool,
    pub restriction_version: u64,
    pub entitlement_version: u64,
    pub recent_usage: Vec<DailyUsage>,
    pub has_older_usage: bool,
    pub shares: Vec<[u8; 12]>,
    pub has_more_shares: bool,
}

pub fn index_key(created_at: u64, id: &AccountId) -> String {
    format!(
        "{ACCOUNT_INDEX}{:020}/{}",
        u64::MAX - created_at,
        URL_SAFE_NO_PAD.encode(id)
    )
}

fn account_id(text: &str) -> Option<AccountId> {
    let mut id = AccountId::default();
    (URL_SAFE_NO_PAD.decode_slice(text, &mut id).ok()? == id.len() && id != [0; 16]).then_some(id)
}

fn after_key(cursor: &str) -> Result<String> {
    let (time, id) = cursor.split_once('.').ok_or(Fault::Invalid)?;
    let created_at = time.parse::<u64>().map_err(|_| Fault::Invalid)?;
    ensure!(created_at.to_string() == time, Fault::Invalid);
    Ok(index_key(
        created_at,
        &account_id(id).ok_or(Fault::Invalid)?,
    ))
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Requires an administrator session, a canonical cursor and readable records.
    pub fn accounts(&self, session: &str, after: Option<&str>) -> Result<AccountPage> {
        self.require_admin(session)?;
        let after = after.map(after_key).transpose()?;
        let today_day = self.environment.now() / DAY_SECONDS;
        let rows =
            self.store
                .scan_after(ACCOUNT_INDEX, after.as_deref(), Some(ACCOUNT_PAGE_SIZE + 1))?;
        let has_more = rows.len() > ACCOUNT_PAGE_SIZE;
        let entries = rows
            .into_iter()
            .take(ACCOUNT_PAGE_SIZE)
            .map(|(record, _)| {
                let id = record
                    .rsplit('/')
                    .next()
                    .and_then(account_id)
                    .context("invalid account index")?;
                let account =
                    load(&self.store, &key("account", &id))?.context("missing indexed account")?;
                self.account_summary(account, today_day)
            })
            .collect::<Result<Vec<_>>>()?;
        let next_cursor = entries
            .last()
            .filter(|_| has_more)
            .map(|entry| format!("{}.{}", entry.created_at, URL_SAFE_NO_PAD.encode(entry.id)));
        tracing::debug!(
            event = "authority_accounts_listed",
            continued = after.is_some(),
            count = entries.len(),
            today_day,
            has_more
        );
        Ok(AccountPage {
            today_day,
            entries,
            next_cursor,
        })
    }

    /// # Errors
    /// Requires an administrator session and a nonempty ID or registered username.
    pub fn lookup_account(&self, session: &str, query: &str) -> Result<AccountSummary> {
        self.require_admin(session)?;
        let query = query.trim();
        ensure!(!query.is_empty(), Fault::Invalid);
        let today = self.environment.now() / DAY_SECONDS;
        let (lookup_kind, account) = if let Some(id) = account_id(query)
            && let Some(account) = load(&self.store, &key("account", &id))?
        {
            ("id", Some(account))
        } else if let Ok(name) = username(query)
            && let Some(id) = load::<AccountId>(&self.store, &format!("username/{name}"))?
        {
            // A username can encode a different, unused account ID.
            ("username", load(&self.store, &key("account", &id))?)
        } else {
            ("miss", None)
        };
        tracing::debug!(
            event = "authority_account_lookup",
            lookup_kind,
            found = account.is_some()
        );
        self.account_summary(account.ok_or(Fault::AccountNotFound)?, today)
    }

    /// # Errors
    /// Requires an administrator session, a canonical ID and an existing account.
    pub fn account_by_id(&self, session: &str, text: &str) -> Result<AccountDetails> {
        self.require_admin(session)?;
        let id = account_id(text).ok_or(Fault::Invalid)?;
        let account = load::<Account>(&self.store, &key("account", &id))?;
        tracing::debug!(event = "authority_account_read", account = %text, found = account.is_some());
        let account = account.ok_or(Fault::AccountNotFound)?;
        let today_day = self.environment.now() / DAY_SECONDS;
        let today_usage = self.daily_usage(&id, today_day)?;
        let entitlement_version = self.entitlement_version(today_usage)?;
        let summary = account_summary_with_bytes(account, today_usage.bytes);
        let daily_limit_bytes = self.settings()?.daily_bytes;
        let remaining_bytes = daily_limit_bytes.saturating_sub(summary.today_bytes);
        let restricted = self.restricted(Restriction::Account(id))?;
        let (recent_usage, has_older_usage) = self.recent_usage(&id)?;
        let mut shares = self.store.scan(
            &format!("{}/", key("attribution", &id)),
            Some(SHARE_LIMIT + 1),
        )?;
        let has_more_shares = shares.len() > SHARE_LIMIT;
        shares.truncate(SHARE_LIMIT);
        let shares = shares
            .into_iter()
            .map(|(_, value)| serde_json::from_str(&value))
            .collect::<serde_json::Result<_>>()?;
        Ok(AccountDetails {
            summary,
            today_day,
            daily_limit_bytes,
            remaining_bytes,
            restricted,
            restriction_version: self.restriction_version()?,
            entitlement_version,
            recent_usage,
            has_older_usage,
            shares,
            has_more_shares,
        })
    }

    fn account_summary(&self, account: Account, day: u64) -> Result<AccountSummary> {
        let today_bytes = self.daily_usage_bytes(&account.id, day)?;
        Ok(account_summary_with_bytes(account, today_bytes))
    }

    fn recent_usage(&self, id: &AccountId) -> Result<(Vec<DailyUsage>, bool)> {
        let prefix = format!("{}/", key("usage", id));
        let mut days = Vec::new();
        for (record, value) in self.store.scan(&prefix, None)? {
            let entry: DailyUsageRecord = serde_json::from_str(&value)?;
            if entry.bytes > 0 {
                let day = record
                    .strip_prefix(&prefix)
                    .context("invalid usage record")?
                    .parse()?;
                days.push(DailyUsage {
                    day,
                    bytes: entry.bytes,
                });
            }
        }
        // Usage keys contain unpadded decimal days, so storage order is not chronological.
        days.sort_unstable_by_key(|entry| Reverse(entry.day));
        let older = days.len() > USAGE_DAYS;
        days.truncate(USAGE_DAYS);
        Ok((days, older))
    }
}

fn account_summary_with_bytes(account: Account, today_bytes: u64) -> AccountSummary {
    AccountSummary {
        id: account.id,
        created_at: account.created_at,
        username: account.username,
        nickname: account.profile.nickname.map(|name| name.value),
        email: account.profile.email,
        has_password: account.password.is_some(),
        today_bytes,
    }
}
