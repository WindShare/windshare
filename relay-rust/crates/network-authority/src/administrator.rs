use crate::{
    Authority, Environment, Fault, PASSWORD_SALT_BYTES, PasswordVerifier, Rate, Store,
    StoredPassword,
    admission::throttle_key,
    password::invalid_credentials,
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
    password: StoredPassword,
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

    pub(crate) fn administrator_username(&self) -> Result<Option<String>> {
        Ok(load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.map(|admin| admin.username))
    }

    /// Initializes the sole administrator credentials using the one-time setup token.
    ///
    /// # Errors
    /// Refuses a consumed initialization link, invalid username, or storage failure.
    pub fn initialize_admin(
        &mut self,
        provided: &str,
        expected: &str,
        name: &str,
        verifier: &PasswordVerifier,
    ) -> Result<String> {
        self.verify_initialization_token(provided, expected)?;
        let normalized = username(name)?;
        let salt = self.environment.random::<PASSWORD_SALT_BYTES>()?;
        let password = StoredPassword::seal(verifier, salt, self.environment.password_peppers());
        let administrator = Administrator {
            username: normalized,
            password,
        };
        save(&mut self.store, ADMINISTRATOR_KEY, &administrator)?;
        tracing::info!(
            event = "authority_initialized",
            username = administrator.username,
            pepper_id = administrator.password.pepper_id(),
        );
        self.admin_session()
    }

    /// Verifies administrator credentials and issues a new console session atomically.
    ///
    /// # Errors
    /// Refuses invalid usernames, wrong credentials, exhausted limits, or storage failures.
    pub fn admin_login(
        &mut self,
        source: &str,
        name: &str,
        verifier: &PasswordVerifier,
    ) -> Result<String> {
        self.check_throttle("admin-login", source.as_bytes(), &ADMIN_LOGIN_SOURCE_RATE)?;
        let administrator =
            load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.ok_or(Fault::Invalid)?;
        if username(name)? != administrator.username {
            // Wrong admin usernames spend only the caller's source bucket so
            // blind probing cannot drain the global admin login budget.
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
        if !administrator
            .password
            .verify(verifier, self.environment.password_peppers())
        {
            tracing::debug!(event = "admin_login_denied");
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

    /// Verifies the current administrator password, stores a newly salted replacement
    /// verifier, revokes every existing session, and issues a fresh session.
    ///
    /// # Errors
    /// Refuses expired sessions, rate-limited callers, wrong current verifiers, or missing records.
    pub fn change_admin_password(
        &mut self,
        source: &str,
        session: &str,
        current: &PasswordVerifier,
        next: &PasswordVerifier,
    ) -> Result<String> {
        self.require_admin(session)?;
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
        let mut administrator =
            load::<Administrator>(&self.store, ADMINISTRATOR_KEY)?.ok_or(Fault::Invalid)?;
        if !administrator
            .password
            .verify(current, self.environment.password_peppers())
        {
            tracing::debug!(event = "admin_password_change_denied");
            return Err(invalid_credentials().into());
        }
        let salt = self.environment.random::<PASSWORD_SALT_BYTES>()?;
        administrator.password =
            StoredPassword::seal(next, salt, self.environment.password_peppers());
        save(&mut self.store, ADMINISTRATOR_KEY, &administrator)?;
        let sessions = self.store.scan("admin-session/", None)?;
        for (record, _) in &sessions {
            self.store.delete(record)?;
        }
        let next_session = self.admin_session()?;
        tracing::info!(
            event = "admin_password_changed",
            username = administrator.username,
            pepper_id = administrator.password.pepper_id(),
            invalidated_sessions = sessions.len()
        );
        Ok(next_session)
    }
}
