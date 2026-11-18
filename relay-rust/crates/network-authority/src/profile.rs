use crate::{
    Account, Authority, EmailDelivery, Environment, Fault, Nickname, Profile, Store,
    challenges::Owner,
    registration::digest,
    store::{key, save},
};
use anyhow::{Result, anyhow, ensure};
use windshare_relay_access::entitlement::DAY_SECONDS;

const NICKNAME_INTERVAL: u64 = 30 * DAY_SECONDS;
const MAX_NICKNAME_CHARS: usize = 32;
const MAX_MAILBOX_BYTES: usize = 254;
const MAX_LOCAL_PART_BYTES: usize = 64;
const PROVIDER_DOMAINS: &[&str] = &[
    "qq.com",
    "foxmail.com",
    "163.com",
    "126.com",
    "yeah.net",
    "sina.com",
    "sina.cn",
    "sohu.com",
    "139.com",
    "189.cn",
    "aliyun.com",
    "gmail.com",
    "googlemail.com",
    "outlook.com",
    "hotmail.com",
    "live.com",
    "msn.com",
    "icloud.com",
    "me.com",
    "mac.com",
    "yahoo.com",
    "yahoo.co.jp",
    "aol.com",
    "proton.me",
    "protonmail.com",
];

pub struct Mailbox(String);

impl Mailbox {
    /// # Errors
    /// Only consumer mailboxes on the supported provider domains can receive verification codes.
    pub fn parse(input: &str) -> Result<Self, Fault> {
        let input = input.trim();
        let (local, domain) = input.split_once('@').ok_or(Fault::InvalidEmail)?;
        let valid_local = !local.is_empty()
            && local.len() <= MAX_LOCAL_PART_BYTES
            && !local.starts_with('.')
            && !local.ends_with('.')
            && !local.contains("..")
            && local
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b".!#$%&'*+-/=?^_`{|}~".contains(&b));
        let valid_domain = !domain.is_empty()
            && domain
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b".-".contains(&b));
        if input.len() > MAX_MAILBOX_BYTES || !valid_local || !valid_domain {
            return Err(Fault::InvalidEmail);
        }
        let domain = domain.to_ascii_lowercase();
        if !PROVIDER_DOMAINS.contains(&domain.as_str()) {
            return Err(Fault::UnsupportedEmail);
        }
        Ok(Self(format!("{local}@{domain}")))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Requires a live credential and an eligible account nickname.
    pub fn set_nickname(&mut self, secret: &str, value: &str) -> Result<Profile> {
        let (credential, mut account) = self.authenticate(secret)?;
        let value = value.trim();
        ensure!(
            !value.is_empty()
                && value.chars().count() <= MAX_NICKNAME_CHARS
                && !value
                    .contains(|ch: char| ch.is_control() || matches!(ch, '\u{2028}' | '\u{2029}')),
            Fault::InvalidNickname
        );
        let now = self.environment.now();
        if let Some(current) = &account.profile.nickname {
            // A retry after a lost response must preserve the original deadline.
            if current.value == value {
                return Ok(account.profile);
            }
            if now < current.next_change_at {
                tracing::info!(event="nickname_change_deferred", account=?account.id, credential=?credential.id, next_change_at=current.next_change_at);
                return Err(Fault::NicknameCooldown {
                    profile: account.profile,
                }
                .into());
            }
        }
        account.profile.nickname = Some(Nickname {
            value: value.into(),
            next_change_at: now.saturating_add(NICKNAME_INTERVAL),
        });
        self.save_profile(account, "nickname", credential.id)
    }

    /// # Errors
    /// Refuses exhausted delivery limits, unsupported addresses and invalid account credentials.
    pub fn prepare_email(
        &mut self,
        source: &str,
        bearer: Option<&str>,
        address: &Mailbox,
    ) -> Result<EmailDelivery> {
        let owner = match bearer {
            Some(secret) => Owner::Account(self.authenticate(secret)?.1.id),
            None => Owner::Registration(digest(source)),
        };
        self.issue_email_code(source, owner, address.as_str())
    }

    /// # Errors
    /// The delivered code must match the current challenge for this account and address.
    pub fn confirm_email(
        &mut self,
        secret: &str,
        address: Mailbox,
        challenge: &str,
        code: &str,
    ) -> Result<Profile> {
        let (credential, mut account) = self.authenticate(secret)?;
        if account.profile.email.as_deref() == Some(address.as_str()) {
            return Ok(account.profile);
        }
        self.verify_mailbox(account.id, address.as_str(), challenge, code)?;
        account.profile.email = Some(address.0);
        self.save_profile(account, "email", credential.id)
    }

    fn save_profile(
        &mut self,
        mut account: Account,
        field: &str,
        credential: [u8; 16],
    ) -> Result<Profile> {
        account.profile.revision = account
            .profile
            .revision
            .checked_add(1)
            .ok_or_else(|| anyhow!("profile revision exhausted"))?;
        save(&mut self.store, &key("account", &account.id), &account)?;
        tracing::info!(event="account_profile_updated", account=?account.id, ?credential, field, revision=account.profile.revision);
        Ok(account.profile)
    }
}
