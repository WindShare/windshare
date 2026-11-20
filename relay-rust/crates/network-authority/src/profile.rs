use crate::{
    Account, Authority, Credential, EmailDelivery, Environment, Fault, Nickname, Profile,
    RegistrationMode, Store,
    challenges::EmailOwner,
    registration::{RegistrationAttempt, digest},
    store::{key, load, save},
};
use anyhow::{Result, anyhow, ensure};
use windshare_relay_access::{AccountId, entitlement::DAY_SECONDS};

const NICKNAME_INTERVAL: u64 = 30 * DAY_SECONDS;
const MAX_NICKNAME_CHARS: usize = 32;
const MAX_MAILBOX_BYTES: usize = 254;
const MAX_LOCAL_PART_BYTES: usize = 64;
const MAX_DOMAIN_LABEL_BYTES: usize = 63;
const PROVIDER_DOMAINS: &[&str] = &[
    "qq.com",
    "vip.qq.com",
    "foxmail.com",
    "163.com",
    "vip.163.com",
    "126.com",
    "188.com",
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
    "live.cn",
    "msn.com",
    "icloud.com",
    "me.com",
    "mac.com",
    "yahoo.com",
    "yahoo.co.jp",
    "aol.com",
    "proton.me",
    "protonmail.com",
    "fastmail.com",
];

pub enum EmailVerification {
    Registration { client_secret: String },
    Profile { bearer: String },
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Mailbox {
    address: String,
    canonical: String,
}

impl Mailbox {
    /// # Errors
    /// Only consumer mailboxes on the supported provider domains can receive verification codes.
    pub fn parse(input: &str) -> Result<Self, Fault> {
        Self::parse_with_extra_domains(input, &[] as &[&str])
    }

    /// # Errors
    /// Rejects malformed addresses and domains outside the default provider allowlist
    /// and any operator-configured extra domains.
    pub fn parse_with_extra_domains<S: AsRef<str>>(
        input: &str,
        extra_domains: &[S],
    ) -> Result<Self, Fault> {
        let (local, domain) = parse_syntax(input)?;
        if !domain_allowed(&domain, extra_domains) {
            return Err(Fault::UnsupportedEmail);
        }
        let canonical = canonicalize_email(&local, &domain);
        Ok(Self {
            address: format!("{local}@{domain}"),
            canonical,
        })
    }

    /// Parses comma-separated operator-configured extra domains, ignoring blank or invalid entries.
    pub fn parse_extra_domains(raw: &str) -> Vec<String> {
        raw.split(',')
            .map(|entry| entry.trim().to_ascii_lowercase())
            .filter(|domain| valid_domain_name(domain))
            .collect()
    }

    pub fn as_str(&self) -> &str {
        &self.address
    }

    pub fn canonical(&self) -> &str {
        &self.canonical
    }

    pub(crate) fn into_address(self) -> String {
        self.address
    }
}

fn parse_syntax(input: &str) -> Result<(String, String), Fault> {
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
    let domain = domain.to_ascii_lowercase();
    if input.len() > MAX_MAILBOX_BYTES || !valid_local || !valid_domain_name(&domain) {
        return Err(Fault::InvalidEmail);
    }
    Ok((local.to_owned(), domain))
}

fn valid_domain_name(domain: &str) -> bool {
    !domain.is_empty()
        && domain.len() <= MAX_MAILBOX_BYTES
        && domain.contains('.')
        && domain
            .rsplit('.')
            .next()
            .is_some_and(|tld| tld.as_bytes().first().is_some_and(u8::is_ascii_alphabetic))
        && domain.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= MAX_DOMAIN_LABEL_BYTES
                && !label.starts_with('-')
                && !label.ends_with('-')
                && label
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
}

fn domain_allowed<S: AsRef<str>>(domain: &str, extra_domains: &[S]) -> bool {
    PROVIDER_DOMAINS.contains(&domain)
        || extra_domains
            .iter()
            .any(|extra| extra.as_ref().trim().eq_ignore_ascii_case(domain))
}

fn canonicalize_email(local: &str, domain: &str) -> String {
    let domain = match domain {
        "googlemail.com" => "gmail.com",
        other => other,
    };
    let lower_local = local.to_ascii_lowercase();
    // Strip +tag subaddressing when a non-empty base mailbox exists so aliases
    // collapse onto the same account identity and recipient rate-limit bucket.
    let base = match lower_local.split_once('+') {
        Some((prefix, _)) if !prefix.is_empty() => prefix,
        _ => lower_local.as_str(),
    };
    if domain == "gmail.com" {
        let compact: String = base.chars().filter(|&ch| ch != '.').collect();
        if !compact.is_empty() {
            return format!("{compact}@{domain}");
        }
    }
    format!("{base}@{domain}")
}

fn canonicalize_stored(address: &str) -> Option<String> {
    let (local, domain) = parse_syntax(address).ok()?;
    Some(canonicalize_email(&local, &domain))
}

pub fn email_owner_key(canonical: &str) -> String {
    key("email", &digest(canonical))
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

    pub(crate) fn email_owner(&self, canonical: &str) -> Result<Option<AccountId>> {
        load(&self.store, &email_owner_key(canonical))
    }

    pub(crate) fn ensure_email_available(
        &self,
        canonical: &str,
        account: Option<AccountId>,
    ) -> Result<()> {
        let owner = self.email_owner(canonical)?;
        ensure!(
            owner.is_none() || owner == account,
            Fault::Denied {
                reason: "email_taken".into()
            }
        );
        Ok(())
    }

    /// # Errors
    /// Refuses disabled email registration, exhausted delivery limits, unsupported addresses and invalid credentials.
    pub fn prepare_email(
        &mut self,
        source: &str,
        verification: EmailVerification,
        address: &Mailbox,
    ) -> Result<EmailDelivery> {
        let owner = match verification {
            EmailVerification::Registration { client_secret } => {
                let attempt = RegistrationAttempt::from_secret(&client_secret)?;
                let mode = self.settings()?.registration.mode(
                    self.account_count()?,
                    self.environment.registration_email_configured(),
                );
                ensure!(
                    mode == RegistrationMode::Email,
                    Fault::Denied {
                        reason: "email_registration_disabled".into()
                    }
                );
                ensure!(
                    load::<Credential>(&self.store, &key("credential", attempt.digest()))?
                        .is_none(),
                    Fault::Invalid
                );
                EmailOwner::Registration(attempt)
            }
            EmailVerification::Profile { bearer } => {
                EmailOwner::Account(self.authenticate(&bearer)?.1.id)
            }
        };
        self.issue_email_code(source, owner, address)
    }

    /// # Errors
    /// The delivered code must match the current challenge for this account and address,
    /// and the canonical mailbox cannot already belong to another account.
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
        // Verify ownership of the mailbox before checking uniqueness so unverified
        // callers cannot probe whether an address is bound to another account.
        self.verify_mailbox(account.id, address.as_str(), challenge, code)?;
        self.ensure_email_available(address.canonical(), Some(account.id))?;
        if let Some(previous) = account
            .profile
            .email
            .as_deref()
            .and_then(canonicalize_stored)
            && previous != address.canonical()
        {
            let previous_key = email_owner_key(&previous);
            if load::<AccountId>(&self.store, &previous_key)? == Some(account.id) {
                self.store.delete(&previous_key)?;
            }
        }
        save(
            &mut self.store,
            &email_owner_key(address.canonical()),
            &account.id,
        )?;
        account.profile.email = Some(address.into_address());
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
