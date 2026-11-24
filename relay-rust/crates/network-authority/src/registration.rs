use crate::{
    Account, Authority, Credential, Environment, Fault, PASSWORD_SALT_BYTES, PasswordVerifier,
    Profile, Rate, Registration, RegistrationPolicy, Session, Store, StoredPassword,
    admission::{AdmissionProof, throttle_key},
    password::invalid_credentials,
    profile::email_owner_key,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use sha2::{Digest, Sha256};
use windshare_relay_access::AccountId;

const MAX_USERNAME: usize = 64;
const CLIENT_SECRET_BYTES: usize = 32;
const ACCOUNT_COUNT_KEY: &str = "account-count";

const ACCOUNT_LOGIN_SOURCE_RATE: Rate = Rate {
    burst: 10,
    refill_seconds: 60,
};
const ACCOUNT_LOGIN_ACCOUNT_RATE: Rate = Rate {
    burst: 10,
    refill_seconds: 60,
};
const ACCOUNT_LOGIN_GLOBAL_RATE: Rate = Rate {
    burst: 120,
    refill_seconds: 1,
};

const PASSWORD_CHANGE_SOURCE_RATE: Rate = Rate {
    burst: 5,
    refill_seconds: 60,
};
const PASSWORD_CHANGE_ACCOUNT_RATE: Rate = Rate {
    burst: 5,
    refill_seconds: 60,
};
const PASSWORD_CHANGE_GLOBAL_RATE: Rate = Rate {
    burst: 60,
    refill_seconds: 1,
};

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

    /// Domain-separated secondary digest for diagnostics so traces correlate
    /// retries without exposing the stored `credential/<digest>` lookup key.
    pub(crate) fn fingerprint(&self) -> String {
        let hash = Sha256::new()
            .chain_update(b"registration-attempt-log/")
            .chain_update(self.0)
            .finalize();
        URL_SAFE_NO_PAD.encode(&hash[..16])
    }
}

pub fn digest(secret: &str) -> [u8; 32] {
    Sha256::digest(secret.as_bytes()).into()
}

/// # Errors
/// Rejects empty, overly long or non-ASCII-identifier usernames.
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

    fn ensure_username_available(&self, name: &str, account: Option<AccountId>) -> Result<()> {
        let owner = load::<AccountId>(&self.store, &format!("username/{name}"))?;
        ensure!(
            owner.is_none() || owner == account,
            Fault::Denied {
                reason: "username_taken".into()
            }
        );
        Ok(())
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
        ensure!(
            name.is_some() == request.password_verifier.is_some(),
            Fault::Invalid
        );
        if let Some(name) = &name {
            self.ensure_username_available(name, None)?;
        }
        let settings = self.settings()?;
        let account_count = self.account_count()?;
        let admission = self.creation_admission(
            source,
            attempt,
            AdmissionProof {
                challenge: request.challenge.as_deref(),
                solution: request.solution.as_deref(),
            },
            &settings,
            account_count,
        )?;
        let mode = admission.mode();
        let existing_owner = admission
            .verified_email()
            .map(|(_, canonical)| self.email_owner(canonical))
            .transpose()?
            .flatten();
        if let Some(existing_id) = existing_owner {
            // Password registration explicitly creates a new named account and must
            // not silently overwrite or merge into another account's credentials.
            ensure!(
                name.is_none(),
                Fault::Denied {
                    reason: "email_taken".into()
                }
            );
            // Passwordless email verification recovers the existing account with a
            // new device credential so reinstalls work while keeping one allowance
            // and block state per canonical mailbox.
            let source_bucket = admission.source_bucket().to_owned();
            admission.consume(&mut self.store)?;
            let account = self.ensure_account(existing_id)?;
            tracing::info!(
                event = "account_recovered_by_email",
                attempt = %attempt.fingerprint(),
                account = ?account.id,
                source_bucket = %source_bucket,
                ?mode,
            );
            return self.new_credential(account.id, request.client_secret);
        }
        let source_bucket = admission.source_bucket().to_owned();
        let verified_email = admission.consume(&mut self.store)?;
        let id = self.environment.random()?;
        anyhow::ensure!(id != [0; 16], "account ID generator returned zero");
        let password = match request.password_verifier {
            Some(verifier) => {
                let salt = self.environment.random::<PASSWORD_SALT_BYTES>()?;
                Some(StoredPassword::seal(
                    &verifier,
                    salt,
                    self.environment.password_peppers(),
                ))
            }
            None => None,
        };
        let account = Account {
            id,
            created_at: self.environment.now(),
            username: name.clone(),
            password,
            profile: Profile {
                revision: u64::from(verified_email.is_some()),
                email: verified_email.as_ref().map(|email| email.address.clone()),
                ..Profile::default()
            },
        };
        if let Some(email) = &verified_email {
            save(&mut self.store, &email_owner_key(&email.canonical), &id)?;
        }
        if let Some(name) = name {
            save(&mut self.store, &format!("username/{name}"), &id)?;
        }
        save(&mut self.store, &key("account", &id), &account)?;
        // Registration order is immutable, while accounts are updated only on login/profile edits.
        self.store.put(
            &crate::account_admin::index_key(account.created_at, &id),
            "",
        )?;
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
        tracing::info!(
            event = "account_created",
            attempt = %attempt.fingerprint(),
            account = ?id,
            source_bucket = %source_bucket,
            ?mode,
            count,
        );
        self.new_credential(id, request.client_secret)
    }

    /// Enforces login rate limits, verifies the client-derived password verifier,
    /// and checks account restrictions only after the verifier matches.
    pub(crate) fn verify_account_login(
        &mut self,
        source: &str,
        name: &str,
        verifier: &PasswordVerifier,
    ) -> Result<Account> {
        self.check_throttle(
            "account-login",
            source.as_bytes(),
            &ACCOUNT_LOGIN_SOURCE_RATE,
        )?;
        let name = username(name)?;
        let Some(id) = load::<AccountId>(&self.store, &format!("username/{name}"))? else {
            // Unknown usernames spend only the source bucket so probing cannot create
            // unbounded per-account throttle rows or drain global login capacity.
            self.throttle(
                source,
                "account-login",
                ACCOUNT_LOGIN_SOURCE_RATE.burst,
                ACCOUNT_LOGIN_SOURCE_RATE.refill_seconds,
            )?;
            return Err(invalid_credentials().into());
        };
        let account = load::<Account>(&self.store, &key("account", &id))?.ok_or(Fault::Invalid)?;
        let Some(password) = account.password.as_ref() else {
            self.throttle(
                source,
                "account-login",
                ACCOUNT_LOGIN_SOURCE_RATE.burst,
                ACCOUNT_LOGIN_SOURCE_RATE.refill_seconds,
            )?;
            return Err(invalid_credentials().into());
        };
        self.throttle_all([
            (
                "account-login",
                throttle_key("account-login", source.as_bytes()),
                ACCOUNT_LOGIN_SOURCE_RATE,
            ),
            (
                "account-login-account",
                throttle_key("account-login-account", &id),
                ACCOUNT_LOGIN_ACCOUNT_RATE,
            ),
            (
                "account-login-global",
                throttle_key("account-login-global", b"global"),
                ACCOUNT_LOGIN_GLOBAL_RATE,
            ),
        ])?;
        if !password.verify(verifier, self.environment.password_peppers()) {
            tracing::debug!(event = "account_login_denied", account = ?id);
            return Err(invalid_credentials().into());
        }
        // Check account restriction only after password verification so
        // unauthenticated callers cannot probe which usernames are blocked.
        self.ensure_account(id)
    }

    /// Authenticates an account password verifier and mints a fresh device credential.
    ///
    /// # Errors
    /// Refuses rate-limited sources, malformed usernames, wrong credentials, or blocked accounts.
    pub fn login(
        &mut self,
        source: &str,
        name: &str,
        verifier: &PasswordVerifier,
    ) -> Result<Session> {
        let account = self.verify_account_login(source, name, verifier)?;
        let secret = URL_SAFE_NO_PAD.encode(self.environment.random::<32>()?);
        self.new_credential(account.id, secret)
    }

    /// Sets or rotates the account username and password verifier atomically.
    ///
    /// # Errors
    /// Refuses rate-limited callers, revoked credentials, invalid inputs, or taken usernames.
    pub fn set_password(
        &mut self,
        source: &str,
        secret: &str,
        name: &str,
        verifier: &PasswordVerifier,
    ) -> Result<Session> {
        self.check_throttle(
            "account-password-source",
            source.as_bytes(),
            &PASSWORD_CHANGE_SOURCE_RATE,
        )?;
        let name = username(name)?;
        let (credential, mut account) = match self.authenticate(secret) {
            Ok(authenticated) => authenticated,
            Err(error) => {
                // Failed credentials spend only the caller's source bucket without
                // touching account or global password-change capacity.
                self.throttle(
                    source,
                    "account-password-source",
                    PASSWORD_CHANGE_SOURCE_RATE.burst,
                    PASSWORD_CHANGE_SOURCE_RATE.refill_seconds,
                )?;
                return Err(error);
            }
        };
        self.ensure_username_available(&name, Some(account.id))?;
        self.throttle_all([
            (
                "account-password-source",
                throttle_key("account-password-source", source.as_bytes()),
                PASSWORD_CHANGE_SOURCE_RATE,
            ),
            (
                "account-password-account",
                throttle_key("account-password-account", &account.id),
                PASSWORD_CHANGE_ACCOUNT_RATE,
            ),
            (
                "account-password-global",
                throttle_key("account-password-global", b"global"),
                PASSWORD_CHANGE_GLOBAL_RATE,
            ),
        ])?;
        let salt = self.environment.random::<PASSWORD_SALT_BYTES>()?;
        let sealed = StoredPassword::seal(verifier, salt, self.environment.password_peppers());
        if let Some(previous) = &account.username
            && previous != &name
        {
            self.store.delete(&format!("username/{previous}"))?;
        }
        save(&mut self.store, &format!("username/{name}"), &account.id)?;
        let pepper_id = sealed.pepper_id().to_owned();
        account.username = Some(name);
        account.password = Some(sealed);
        save(&mut self.store, &key("account", &account.id), &account)?;
        tracing::info!(
            event = "account_login_changed",
            account = ?account.id,
            credential = ?credential.id,
            pepper_id,
        );
        self.refresh(secret)
    }

    fn new_credential(&mut self, account: AccountId, secret: String) -> Result<Session> {
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
