use crate::{
    Account, Authority, Credential, Environment, Fault, Session, Store,
    registration::digest,
    store::{key, load, save},
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use windshare_relay_access::{
    AccountId, CredentialId,
    entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME, Entitlement},
    restrictions::Restriction,
    ticket::{TICKET_LIFETIME, Ticket},
};

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn ensure_account(&self, id: AccountId) -> Result<Account> {
        ensure!(
            !self.restricted(Restriction::Account(id))?,
            Fault::Denied {
                reason: "account_blocked".into()
            }
        );
        load(&self.store, &key("account", &id))?.ok_or_else(|| {
            Fault::Denied {
                reason: "unknown_account".into(),
            }
            .into()
        })
    }
    pub(crate) fn authenticate(&self, secret: &str) -> Result<(Credential, Account)> {
        ensure!(secret.len() <= 128, Fault::Invalid);
        let credential = load::<Credential>(&self.store, &key("credential", &digest(secret)))?
            .ok_or_else(|| Fault::Denied {
                reason: "credentials".into(),
            })?;
        ensure!(
            !credential.revoked && !self.restricted(Restriction::Credential(credential.id))?,
            Fault::Denied {
                reason: "credential_revoked".into()
            }
        );
        let account = self.ensure_account(credential.account)?;
        Ok((credential, account))
    }
    /// # Errors
    /// Revoked credentials and blocked accounts cannot obtain tickets; signing and storage failures propagate.
    pub fn refresh(&mut self, secret: &str) -> Result<Session> {
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
    pub(crate) fn entitlement(&mut self, id: AccountId) -> Result<Entitlement> {
        let mut account =
            load::<Account>(&self.store, &key("account", &id))?.ok_or(Fault::Invalid)?;
        let now = self.environment.now();
        let day = now / DAY_SECONDS;
        let used =
            load::<u64>(&self.store, &format!("{}/{}", key("usage", &id), day))?.unwrap_or(0);
        account.version = account
            .version
            .checked_add(1)
            .ok_or_else(|| anyhow!("entitlement version exhausted"))?;
        save(&mut self.store, &key("account", &id), &account)?;
        Ok(Entitlement {
            version: account.version,
            day,
            valid_until: now
                .saturating_add(ENTITLEMENT_LIFETIME)
                .min((day + 1) * DAY_SECONDS),
            remaining_bytes: self.settings()?.daily_bytes.saturating_sub(used),
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
