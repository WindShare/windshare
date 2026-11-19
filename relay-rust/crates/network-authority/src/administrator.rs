use crate::{
    Authority, Environment, Fault, Store,
    registration::{digest, password_hash, password_matches, username},
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};

const ADMIN_SESSION_SECONDS: u64 = 12 * 60 * 60;
#[derive(Serialize, Deserialize)]
struct Administrator {
    username: String,
    password: String,
}
#[derive(Serialize, Deserialize)]
struct Throttle {
    credit: u64,
    updated: u64,
}
impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn throttle(
        &mut self,
        source: &str,
        scope: &str,
        burst: u64,
        refill: u64,
    ) -> Result<()> {
        let key = key(&format!("throttle/{scope}"), &digest(source));
        let now = self.environment.now();
        let mut state = load::<Throttle>(&self.store, &key)?.unwrap_or(Throttle {
            credit: burst,
            updated: now,
        });
        let elapsed = now.saturating_sub(state.updated);
        state.credit = state.credit.saturating_add(elapsed / refill).min(burst);
        state.updated = now - elapsed % refill;
        ensure!(
            state.credit > 0,
            Fault::RateLimited {
                retry_after: refill
            }
        );
        state.credit -= 1;
        save(&mut self.store, &key, &state)
    }
    /// # Errors
    /// Refuses a consumed initialization link or invalid administrator credentials.
    pub fn initialize(
        &mut self,
        provided: &str,
        expected: &str,
        name: &str,
        password: &str,
    ) -> Result<String> {
        ensure!(
            !expected.is_empty()
                && digest(provided) == digest(expected)
                && self.store.get("administrator")?.is_none(),
            Fault::Denied {
                reason: "initialization_consumed".into()
            }
        );
        let administrator = Administrator {
            username: username(name)?,
            password: password_hash(password, &self.environment.random::<16>()?)?,
        };
        save(&mut self.store, "administrator", &administrator)?;
        tracing::info!(
            event = "authority_initialized",
            username = administrator.username
        );
        self.admin_session()
    }
    /// # Errors
    /// Refuses invalid credentials, exhausted attempt limits and storage failures.
    pub fn admin_login(&mut self, source: &str, name: &str, password: &str) -> Result<String> {
        self.throttle(source, "admin-login", 10, 60)?;
        let administrator =
            load::<Administrator>(&self.store, "administrator")?.ok_or(Fault::Invalid)?;
        ensure!(
            username(name)? == administrator.username
                && password_matches(password, &administrator.password),
            Fault::Denied {
                reason: "credentials".into()
            }
        );
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
}
