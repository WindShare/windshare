use crate::{
    Authority, Environment, Fault, HashedPassword, Rate, Store,
    admission::throttle_key,
    password::{invalid_credentials, valid_password_length},
    registration::{digest, username},
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};

const ADMINISTRATOR_KEY: &str = "administrator";
const ADMIN_SESSION_SECONDS: u64 = 12 * 60 * 60;
const ADMIN_LOGIN_SOURCE_RATE: Rate = Rate {
    burst: 10,
    refill_seconds: 60,
};
const ADMIN_LOGIN_GLOBAL_RATE: Rate = Rate {
    burst: 30,
    refill_seconds: 2,
};
const ADMIN_PASSWORD_CHANGE_SOURCE_RATE: Rate = Rate {
    burst: 5,
    refill_seconds: 60,
};
const ADMIN_PASSWORD_CHANGE_GLOBAL_RATE: Rate = Rate {
    burst: 30,
    refill_seconds: 2,
};

#[derive(Serialize, Deserialize)]
struct Administrator {
    username: String,
    password: HashedPassword,
}

impl<R: Store, E: Environment> Authority<R, E> {
    fn verify_initialization_token(&self, provided: &str, expected: &str) -> Result<()> {
        ensure!(
            !expected.is_empty()
                && digest(provided) == digest(expected)
                && self.store.get(ADMINISTRATOR_KEY)?.is_none(),
            Fault::Denied {
                reason: "initialization_consumed".into()
            }
        );
        Ok(())
    }

    /// Validates initialization authorization and inputs before edge Argon2 runs.
    ///
    /// # Errors
    /// Refuses a consumed initialization link or invalid administrator credentials.
    pub fn prepare_initialization(
        &self,
        provided: &str,
        expected: &str,
        name: &str,
        password_len: usize,
    ) -> Result<String> {
        self.verify_initialization_token(provided, expected)?;
        let normalized = username(name)?;
        ensure!(valid_password_length(password_len), Fault::Invalid);
        Ok(normalized)
    }

    /// Commits administrator credentials hashed outside the Durable Object.
    ///
    /// # Errors
    /// Refuses a consumed initialization link or invalid administrator username.
    pub fn complete_initialization(
        &mut self,
        provided: &str,
        expected: &str,
        name: &str,
        password: HashedPassword,
    ) -> Result<String> {
        self.verify_initialization_token(provided, expected)?;
        let administrator = Administrator {
            username: username(name)?,
            password,
        };
        save(&mut self.store, ADMINISTRATOR_KEY, &administrator)?;
        tracing::info!(
            event = "authority_initialized",
            username = administrator.username
        );
        self.admin_session()
    }

    /// Enforces administrator login limits and returns the stored verifier for
    /// edge password verification.
    ///
    /// # Errors
    /// Refuses invalid usernames, exhausted attempt limits and storage failures.
    pub fn begin_admin_login(&mut self, source: &str, name: &str) -> Result<HashedPassword> {
        self.check_throttle("admin-login", source.as_bytes(), &ADMIN_LOGIN_SOURCE_RATE)?;
        let administrator =
            load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.ok_or(Fault::Invalid)?;
        if username(name)? != administrator.username {
            // Wrong admin usernames spend only the caller's source bucket so
            // blind probing cannot drain the global admin Argon2 budget.
            self.throttle(
                source,
                "admin-login",
                ADMIN_LOGIN_SOURCE_RATE.burst,
                ADMIN_LOGIN_SOURCE_RATE.refill_seconds,
            )?;
            return Err(invalid_credentials().into());
        }
        self.throttle_all([
            (
                "admin-login",
                throttle_key("admin-login", source.as_bytes()),
                ADMIN_LOGIN_SOURCE_RATE,
            ),
            (
                "admin-login-global",
                throttle_key("admin-login-global", b"global"),
                ADMIN_LOGIN_GLOBAL_RATE,
            ),
        ])?;
        Ok(administrator.password)
    }

    /// Issues an administrator session after edge verification, re-checking that
    /// the stored verifier did not change in between.
    ///
    /// # Errors
    /// Refuses stale verifiers or missing administrator records.
    pub fn complete_admin_login(&mut self, password_digest: [u8; 32]) -> Result<String> {
        let administrator =
            load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.ok_or(Fault::Invalid)?;
        if administrator.password.fingerprint() != password_digest {
            tracing::warn!(event = "admin_login_stale_verifier");
            return Err(invalid_credentials().into());
        }
        self.admin_session()
    }
    fn admin_session(&mut self) -> Result<String> {
        let session = URL_SAFE_NO_PAD.encode(self.environment.random::<32>()?);
        save(
            &mut self.store,
            &key("admin-session", &digest(&session)),
            &(self.environment.now() + ADMIN_SESSION_SECONDS),
        )?;
        Ok(session)
    }
    pub(crate) fn require_admin(&self, session: &str) -> Result<()> {
        ensure!(
            session.len() <= 128
                && load::<u64>(&self.store, &key("admin-session", &digest(session)))?
                    .is_some_and(|expires| expires > self.environment.now()),
            Fault::administrator_session_required()
        );
        Ok(())
    }
    /// # Errors
    /// Propagates failure to invalidate the persistent administrator session.
    pub fn logout(&mut self, session: &str) -> Result<()> {
        self.store.delete(&key("admin-session", &digest(session)))
    }

    /// Validates administrator session, password-change rate limits and new
    /// password length before the edge Worker runs Argon2 verification and hashing.
    ///
    /// # Errors
    /// Refuses expired sessions, invalid lengths, rate-limited callers or missing records.
    pub fn prepare_admin_password_change(
        &mut self,
        source: &str,
        session: &str,
        new_password_len: usize,
    ) -> Result<HashedPassword> {
        self.require_admin(session)?;
        ensure!(valid_password_length(new_password_len), Fault::Invalid);
        self.throttle_all([
            (
                "admin-password",
                throttle_key("admin-password", source.as_bytes()),
                ADMIN_PASSWORD_CHANGE_SOURCE_RATE,
            ),
            (
                "admin-password-global",
                throttle_key("admin-password-global", b"global"),
                ADMIN_PASSWORD_CHANGE_GLOBAL_RATE,
            ),
        ])?;
        let administrator =
            load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.ok_or(Fault::Invalid)?;
        Ok(administrator.password)
    }

    /// Commits a pre-hashed administrator password, revokes every existing
    /// administrator session and issues a replacement session for the caller.
    ///
    /// # Errors
    /// Refuses expired sessions or verifiers superseded by a concurrent password change.
    pub fn commit_admin_password_change(
        &mut self,
        session: &str,
        password: HashedPassword,
        previous_verifier_digest: [u8; 32],
    ) -> Result<String> {
        self.require_admin(session)?;
        let mut administrator =
            load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.ok_or(Fault::Invalid)?;
        if administrator.password.fingerprint() != previous_verifier_digest {
            tracing::warn!(event = "admin_password_change_stale_verifier");
            return Err(Fault::Denied {
                reason: "credentials_changed".into(),
            }
            .into());
        }
        administrator.password = password;
        save(&mut self.store, ADMINISTRATOR_KEY, &administrator)?;
        let sessions = self.store.scan("admin-session/", None)?;
        for (record, _) in &sessions {
            self.store.delete(record)?;
        }
        let next_session = self.admin_session()?;
        tracing::info!(
            event = "admin_password_changed",
            username = administrator.username,
            invalidated_sessions = sessions.len()
        );
        Ok(next_session)
    }
}
