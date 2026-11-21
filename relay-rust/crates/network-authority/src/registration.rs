use crate::{
    Account, Authority, Credential, Environment, Fault, HashedPassword, PasswordChallenge,
    PasswordChangePreparation, PasswordRegistrationPreparation, PasswordRegistrationPrepare,
    Profile, Rate, Registration, RegistrationPolicy, Session, Store,
    admission::{AdmissionProof, throttle_key},
    password::{invalid_credentials, valid_password_length},
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

const PASSWORD_REGISTER_SOURCE_RATE: Rate = Rate {
    burst: 10,
    refill_seconds: 60,
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

    /// Validates admission and password-hashing budgets before the edge Worker runs Argon2.
    ///
    /// # Errors
    /// Refuses malformed input, occupied usernames, rate-limited callers or unverified proofs.
    pub fn prepare_password_registration(
        &mut self,
        source: &str,
        request: &PasswordRegistrationPrepare,
    ) -> Result<PasswordRegistrationPreparation> {
        let attempt = RegistrationAttempt::from_secret(&request.client_secret)?;
        if load::<Credential>(&self.store, &key("credential", attempt.digest()))?.is_some() {
            return Ok(PasswordRegistrationPreparation::Existing {
                session: Box::new(self.refresh(&request.client_secret)?),
            });
        }
        let name = username(&request.username)?;
        self.ensure_username_available(&name, None)?;
        ensure!(valid_password_length(request.password_len), Fault::Invalid);
        let settings = self.settings()?;
        let account_count = self.account_count()?;
        // Dropping the returned admission handle preserves valid proofs and creation
        // quota for commit while still persisting failed proof attempts.
        let preview = self.creation_admission(
            source,
            attempt,
            AdmissionProof {
                challenge: request.challenge.as_deref(),
                solution: request.solution.as_deref(),
            },
            &settings,
            account_count,
        )?;
        if let Some((_, canonical)) = preview.verified_email() {
            self.ensure_email_available(canonical, None)?;
        }
        // Charge per-source and global password-hashing limits atomically so aborted
        // or racing clients cannot trigger edge Argon2 without spending capacity.
        self.throttle_all([
            (
                "account-password-register",
                throttle_key("account-password-register", source.as_bytes()),
                PASSWORD_REGISTER_SOURCE_RATE,
            ),
            (
                "account-password-global",
                throttle_key("account-password-global", b"global"),
                PASSWORD_CHANGE_GLOBAL_RATE,
            ),
        ])?;
        Ok(PasswordRegistrationPreparation::Ready)
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
        let account = Account {
            id,
            created_at: self.environment.now(),
            username: name.clone(),
            password: request.password,
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

    /// Enforces source, account and global login limits and loads the stored
    /// verifier so Argon2 runs outside the singleton Durable Object.
    ///
    /// # Errors
    /// Refuses rate-limited sources, malformed usernames, unknown accounts, and accounts without passwords.
    pub fn begin_account_login(&mut self, source: &str, name: &str) -> Result<PasswordChallenge> {
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
        // Check account restriction only after edge password verification so
        // unauthenticated callers cannot probe which usernames are blocked.
        let account = load::<Account>(&self.store, &key("account", &id))?.ok_or(Fault::Invalid)?;
        let Some(password) = account.password else {
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
        Ok(PasswordChallenge {
            account: id,
            password,
        })
    }

    /// Issues a new credential after edge password verification, re-checking
    /// that the account stayed unblocked and kept the same password verifier.
    ///
    /// # Errors
    /// Refuses blocked accounts or verifiers superseded during edge hashing.
    pub fn complete_account_login(
        &mut self,
        account_id: AccountId,
        password_digest: [u8; 32],
    ) -> Result<Session> {
        let account = self.verified_account(account_id, password_digest)?;
        let secret = URL_SAFE_NO_PAD.encode(self.environment.random::<32>()?);
        self.new_credential(account.id, secret)
    }

    pub(crate) fn verified_account(
        &self,
        account_id: AccountId,
        password_digest: [u8; 32],
    ) -> Result<Account> {
        let account = self.ensure_account(account_id)?;
        if account
            .password
            .as_ref()
            .is_none_or(|hash| hash.fingerprint() != password_digest)
        {
            tracing::warn!(event = "password_login_stale_verifier", account = ?account.id);
            return Err(invalid_credentials().into());
        }
        Ok(account)
    }

    /// Enforces password-change limits and validates authentication and username
    /// ownership before the edge Worker runs Argon2.
    ///
    /// # Errors
    /// Refuses rate-limited callers, revoked credentials, invalid inputs or taken usernames.
    pub fn prepare_password_change(
        &mut self,
        source: &str,
        secret: &str,
        name: &str,
        password_len: usize,
    ) -> Result<PasswordChangePreparation> {
        self.check_throttle(
            "account-password-source",
            source.as_bytes(),
            &PASSWORD_CHANGE_SOURCE_RATE,
        )?;
        let name = username(name)?;
        ensure!(valid_password_length(password_len), Fault::Invalid);
        let (credential, account) = match self.authenticate(secret) {
            Ok(verified) => verified,
            Err(error) => {
                // Failed credentials spend only the caller's source bucket without
                // touching account or global Argon2 capacity.
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
        tracing::debug!(
            event = "password_change_prepared",
            account = ?account.id,
            credential = ?credential.id
        );
        Ok(PasswordChangePreparation {
            previous_verifier_digest: account.password.as_ref().map(HashedPassword::fingerprint),
        })
    }

    /// Commits a pre-hashed password after re-checking credential liveness,
    /// optimistic verifier version and username uniqueness.
    ///
    /// # Errors
    /// Only a live credential can change its account login, concurrent credential
    /// updates fail closed, and names remain unique.
    pub fn commit_password_change(
        &mut self,
        secret: &str,
        name: &str,
        password: HashedPassword,
        expected_previous_verifier: Option<[u8; 32]>,
    ) -> Result<Session> {
        let (credential, mut account) = self.authenticate(secret)?;
        let current_digest = account.password.as_ref().map(HashedPassword::fingerprint);
        if current_digest != expected_previous_verifier {
            tracing::warn!(
                event = "password_change_stale_verifier",
                account = ?account.id,
                credential = ?credential.id
            );
            return Err(Fault::Denied {
                reason: "credentials_changed".into(),
            }
            .into());
        }
        let name = username(name)?;
        self.ensure_username_available(&name, Some(account.id))?;
        if let Some(previous) = &account.username
            && previous != &name
        {
            self.store.delete(&format!("username/{previous}"))?;
        }
        save(&mut self.store, &format!("username/{name}"), &account.id)?;
        account.username = Some(name);
        account.password = Some(password);
        save(&mut self.store, &key("account", &account.id), &account)?;
        tracing::info!(event="account_login_changed",account=?account.id,credential=?credential.id);
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
