use crate::{
    Account, Authority, Credential, Environment, Fault, Profile, Registration, RegistrationPolicy,
    Session, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use argon2::{
    Argon2,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString},
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use sha2::{Digest, Sha256};

const MIN_PASSWORD: usize = 12;
const MAX_PASSWORD: usize = 256;
const MAX_USERNAME: usize = 64;
const CLIENT_SECRET_BYTES: usize = 32;
const ACCOUNT_COUNT_KEY: &str = "account-count";

#[derive(Clone, Copy)]
pub struct RegistrationAttempt([u8; 32]);

impl RegistrationAttempt {
    pub(crate) fn from_secret(secret: &str) -> Result<Self> {
        ensure!(
            URL_SAFE_NO_PAD
                .decode(secret)
                .is_ok_and(|bytes| bytes.len() == CLIENT_SECRET_BYTES),
            Fault::Invalid
        );
        Ok(Self(digest(secret)))
    }

    pub(crate) const fn digest(&self) -> &[u8; 32] {
        &self.0
    }
}

pub fn digest(secret: &str) -> [u8; 32] {
    Sha256::digest(secret.as_bytes()).into()
}
pub fn password_hash(password: &str, salt: &[u8]) -> Result<String> {
    ensure!(
        (MIN_PASSWORD..=MAX_PASSWORD).contains(&password.len()),
        Fault::Invalid
    );
    let salt = SaltString::encode_b64(salt).map_err(|_| Fault::Invalid)?;
    Argon2::default()
        .hash_password(password.as_bytes(), &salt)
        .map(|hash| hash.to_string())
        .map_err(|_| Fault::Invalid.into())
}
pub fn password_matches(password: &str, hash: &str) -> bool {
    password.len() <= MAX_PASSWORD
        && PasswordHash::new(hash).is_ok_and(|hash| {
            Argon2::default()
                .verify_password(password.as_bytes(), &hash)
                .is_ok()
        })
}
pub fn username(name: &str) -> Result<String> {
    let name = name.trim().to_ascii_lowercase();
    ensure!(
        !name.is_empty()
            && name.len() <= MAX_USERNAME
            && name
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || b"._-".contains(&byte)),
        Fault::Invalid
    );
    Ok(name)
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn account_count(&self) -> Result<u64> {
        Ok(load(&self.store, ACCOUNT_COUNT_KEY)?.unwrap_or(0))
    }

    /// # Errors
    /// Refuses invalid or restricted creation requests; retries preserve existing credentials.
    pub fn register(&mut self, source: &str, request: Registration) -> Result<Session> {
        let attempt = RegistrationAttempt::from_secret(&request.client_secret)?;
        // Persisted client entropy makes a lost setup response safe to retry without
        // creating another principal or spending another creation-rate token.
        if load::<Credential>(&self.store, &key("credential", attempt.digest()))?.is_some() {
            return self.refresh(&request.client_secret);
        }
        let name = request.username.as_deref().map(username).transpose()?;
        ensure!(name.is_some() == request.password.is_some(), Fault::Invalid);
        if let Some(name) = &name {
            ensure!(
                self.store.get(&format!("username/{name}"))?.is_none(),
                Fault::Denied {
                    reason: "username_taken".into()
                }
            );
        }
        if let Some(password) = &request.password {
            ensure!(
                (MIN_PASSWORD..=MAX_PASSWORD).contains(&password.len()),
                Fault::Invalid
            );
        }
        let settings = self.settings()?;
        let account_count = self.account_count()?;
        let email = self.creation_admission(source, attempt, &request, &settings, account_count)?;
        // Creation admission precedes expensive password hashing, so refused
        // requests cannot spend unbounded CPU while bypassing the creation limiter.
        let id = self.environment.random()?;
        let password = request
            .password
            .as_deref()
            .map(|password| password_hash(password, &id))
            .transpose()?;
        let account = Account {
            id,
            created_at: self.environment.now(),
            username: name.clone(),
            password,
            profile: Profile {
                revision: u64::from(email.is_some()),
                email,
                ..Profile::default()
            },
            version: 0,
        };
        if let Some(name) = name {
            save(&mut self.store, &format!("username/{name}"), &id)?;
        }
        save(&mut self.store, &key("account", &id), &account)?;
        let count = account_count + 1;
        save(&mut self.store, ACCOUNT_COUNT_KEY, &count)?;
        if RegistrationPolicy::is_milestone(count) {
            save(
                &mut self.store,
                &format!("review/{count}"),
                &self.environment.now(),
            )?;
            let email_configured = self.environment.registration_email_configured();
            tracing::info!(event = "account_registration_milestone", count, policy = ?settings.registration,
                next_mode = ?settings.registration.mode(count, email_configured), email_configured);
        }
        tracing::info!(event="account_created",account=?id,source,count);
        self.new_credential(id, request.client_secret)
    }

    /// # Errors
    /// Refuses incorrect passwords or blocked accounts and propagates credential persistence failures.
    pub fn login(&mut self, name: &str, password: &str) -> Result<Session> {
        let account = self.password_account(name, password)?;
        let secret = URL_SAFE_NO_PAD.encode(self.environment.random::<32>()?);
        self.new_credential(account.id, secret)
    }

    pub(crate) fn password_account(&self, name: &str, password: &str) -> Result<Account> {
        let name = username(name)?;
        let id = load::<[u8; 16]>(&self.store, &format!("username/{name}"))?.ok_or_else(|| {
            Fault::Denied {
                reason: "credentials".into(),
            }
        })?;
        let account = load::<Account>(&self.store, &key("account", &id))?.ok_or(Fault::Invalid)?;
        ensure!(
            account
                .password
                .as_ref()
                .is_some_and(|hash| password_matches(password, hash)),
            Fault::Denied {
                reason: "credentials".into()
            }
        );
        self.ensure_account(id)
    }

    /// # Errors
    /// Only a live credential can change its account login, and names remain unique.
    pub fn set_password(&mut self, secret: &str, name: &str, password: &str) -> Result<Session> {
        let (credential, mut account) = self.authenticate(secret)?;
        let name = username(name)?;
        let owner = load::<[u8; 16]>(&self.store, &format!("username/{name}"))?;
        ensure!(
            owner.is_none_or(|id| id == account.id),
            Fault::Denied {
                reason: "username_taken".into()
            }
        );
        let hash = password_hash(password, &account.id)?;
        if let Some(previous) = &account.username
            && previous != &name
        {
            self.store.delete(&format!("username/{previous}"))?;
        }
        save(&mut self.store, &format!("username/{name}"), &account.id)?;
        account.username = Some(name);
        account.password = Some(hash);
        save(&mut self.store, &key("account", &account.id), &account)?;
        tracing::info!(event="account_login_changed",account=?account.id,credential=?credential.id);
        self.refresh(secret)
    }

    fn new_credential(&mut self, account: [u8; 16], secret: String) -> Result<Session> {
        let credential = Credential {
            id: self.environment.random()?,
            account,
            secret_digest: digest(&secret),
            created_at: self.environment.now(),
            revoked: false,
        };
        save(
            &mut self.store,
            &key("credential", &credential.secret_digest),
            &credential,
        )?;
        save(
            &mut self.store,
            &key("credential-id", &credential.id),
            &credential.secret_digest,
        )?;
        let mut session = self.refresh(&secret)?;
        session.credential_secret = Some(secret);
        Ok(session)
    }
}
