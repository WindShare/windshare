use crate::{
    Account, Authority, Credential, Environment, Fault, Session, Store,
    registration::digest,
    store::{key, load, save},
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use windshare_relay_access::{
    AccountId, CredentialId,
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME, Entitlement},
    restrictions::Restriction,
    ticket::{TICKET_LIFETIME, Ticket},
};

pub const DAILY_BYTES_VERSION_KEY: &str = "daily-bytes-version";
const INITIAL_DAILY_BYTES_VERSION: u64 = 1;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct DailyUsageRecord {
    pub bytes: u64,
    pub version: u64,
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn ensure_account(&self, id: AccountId) -> Result<Account> {
        if self.restricted(Restriction::Account(id))? {
            tracing::debug!(
                event = "account_auth_refused",
                account = ?id,
                reason = "account_blocked",
            );
            return Err(Fault::Denied {
                reason: "account_blocked".into(),
            }
            .into());
        }
        load(&self.store, &key("account", &id))?.ok_or_else(|| {
            tracing::debug!(
                event = "account_auth_refused",
                account = ?id,
                reason = "unknown_account",
            );
            Fault::Denied {
                reason: "unknown_account".into(),
            }
            .into()
        })
    }
    pub(crate) fn authenticate(&self, secret: &str) -> Result<(Credential, Account)> {
        ensure!(secret.len() <= 128, Fault::Invalid);
        let Some(credential) =
            load::<Credential>(&self.store, &key("credential", &digest(secret)))?
        else {
            tracing::debug!(event = "account_auth_refused", reason = "credentials");
            return Err(Fault::Denied {
                reason: "credentials".into(),
            }
            .into());
        };
        if credential.revoked || self.restricted(Restriction::Credential(credential.id))? {
            tracing::debug!(
                event = "account_auth_refused",
                account = ?credential.account,
                credential = ?credential.id,
                reason = "credential_revoked",
            );
            return Err(Fault::Denied {
                reason: "credential_revoked".into(),
            }
            .into());
        }
        let account = self.ensure_account(credential.account)?;
        Ok((credential, account))
    }
    /// # Errors
    /// Revoked credentials and blocked accounts cannot obtain tickets; signing and storage failures propagate.
    pub fn refresh(&self, secret: &str) -> Result<Session> {
        let (credential, account) = self.authenticate(secret)?;
        let entitlement = self.entitlement(credential.account)?;
        let now = self.environment.now();
        let expires_at = now
            .saturating_add(TICKET_LIFETIME)
            .min(self.signing.generation.expires_at);
        let ticket = Ticket {
            issuer: self.signing.generation.issuer,
            key_id: self.signing.generation.number,
            account: credential.account,
            credential: credential.id,
            issued_at: now,
            expires_at,
            entitlement,
        };
        let signed = ticket
            .issue(self.signing.generation, &self.signing.key)
            .map_err(|error| anyhow!("ticket signing: {error:?}"))?;
        tracing::info!(event="account_ticket_issued",account=?credential.account,credential=?credential.id,expires_at,version=entitlement.version);
        Ok(Session {
            account: credential.account,
            credential: credential.id,
            credential_secret: None,
            username: account.username,
            profile: account.profile,
            ticket: URL_SAFE_NO_PAD.encode(signed),
            expires_at,
            entitlement,
            certificate: self.signing.certificate.clone(),
        })
    }
    pub(crate) fn daily_bytes_version(&self) -> Result<u64> {
        Ok(load(&self.store, DAILY_BYTES_VERSION_KEY)?.unwrap_or(INITIAL_DAILY_BYTES_VERSION))
    }

    pub(crate) fn daily_usage(&self, id: &AccountId, day: u64) -> Result<DailyUsageRecord> {
        Ok(load(&self.store, &format!("{}/{day}", key("usage", id)))?.unwrap_or_default())
    }

    pub(crate) fn daily_usage_bytes(&self, id: &AccountId, day: u64) -> Result<u64> {
        Ok(self.daily_usage(id, day)?.bytes)
    }

    pub(crate) fn entitlement_version(&self, usage: DailyUsageRecord) -> Result<u64> {
        // Combine global allowance-limit revisions with per-account daily charge counts so
        // version increases strictly on state mutations without writing on read-only refreshes.
        self.daily_bytes_version()?
            .checked_add(usage.version)
            .ok_or_else(|| anyhow!("entitlement version exhausted"))
    }

    pub(crate) fn entitlement(&self, id: AccountId) -> Result<Entitlement> {
        load::<Account>(&self.store, &key("account", &id))?.ok_or(Fault::Invalid)?;
        let now = self.environment.now();
        let day = now / DAY_SECONDS;
        let usage = self.daily_usage(&id, day)?;
        let version = self.entitlement_version(usage)?;
        Ok(Entitlement {
            version,
            day,
            valid_until: now
                .saturating_add(ENTITLEMENT_LIFETIME)
                .min((day + 1) * DAY_SECONDS),
            remaining_bytes: self.settings()?.daily_bytes.saturating_sub(usage.bytes),
        })
    }
    /// # Errors
    /// Only a live credential belonging to the same account can revoke a sibling.
    pub fn revoke_credential(&mut self, secret: &str, id: CredentialId) -> Result<()> {
        let (caller, _) = self.authenticate(secret)?;
        let digest =
            load::<[u8; 32]>(&self.store, &key("credential-id", &id))?.ok_or(Fault::Invalid)?;
        let mut credential =
            load::<Credential>(&self.store, &key("credential", &digest))?.ok_or(Fault::Invalid)?;
        ensure!(
            credential.account == caller.account,
            Fault::Denied {
                reason: "credential_owner".into()
            }
        );
        credential.revoked = true;
        save(&mut self.store, &key("credential", &digest), &credential)?;
        if self.set_restriction(Restriction::Credential(id), true)? {
            tracing::warn!(event="credential_revoked",account=?caller.account,credential=?id);
        }
        Ok(())
    }
}
