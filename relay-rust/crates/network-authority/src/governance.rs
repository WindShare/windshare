use crate::{
    Authority, Environment, Fault, RegistrationMode, Settings, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use windshare_relay_access::restrictions::Restriction;

const MAX_REPORT_REASON: usize = 2000;
const MAX_CONSOLE_ROWS: usize = 1000;
#[derive(Clone, Copy, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum Block {
    Share { id: [u8; 12], blocked: bool },
    Account { id: [u8; 16], blocked: bool },
}
#[derive(Serialize, Deserialize)]
pub struct Report {
    pub id: [u8; 16],
    pub share: [u8; 12],
    pub reason: String,
    pub created_at: u64,
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Requires a live administrator session and a valid registration policy.
    pub fn set_settings(
        &mut self,
        session: &str,
        settings: &Settings,
        email_configured: bool,
    ) -> Result<()> {
        self.require_admin(session)?;
        ensure!(
            (16..=30).contains(&settings.pow_bits)
                && settings.daily_bytes > 0
                && settings.source_rate.burst > 0
                && settings.source_rate.refill_seconds > 0
                && settings.global_rate.burst > 0
                && settings.global_rate.refill_seconds > 0
                && (settings.registration != RegistrationMode::Email || email_configured),
            Fault::Invalid
        );
        save(&mut self.store, "settings", settings)?;
        tracing::info!(event="authority_policy_changed",mode=?settings.registration,pow_bits=settings.pow_bits);
        Ok(())
    }
    /// # Errors
    /// Requires a live administrator session and a nonzero target identity.
    pub fn block(&mut self, session: &str, block: Block) -> Result<()> {
        self.require_admin(session)?;
        let (entry, blocked) = match block {
            Block::Share { id, blocked } => {
                ensure!(id != [0; 12], Fault::Invalid);
                (Restriction::Share(id), blocked)
            }
            Block::Account { id, blocked } => {
                ensure!(id != [0; 16], Fault::Invalid);
                (Restriction::Account(id), blocked)
            }
        };
        if self.set_restriction(entry, blocked)? {
            tracing::warn!(event = "access_block_changed", ?entry, blocked);
            self.update_public_blocks()?;
        }
        Ok(())
    }
    /// # Errors
    /// Refuses malformed or rate-limited reports and storage failures.
    pub fn report(&mut self, source: &str, share: [u8; 12], reason: String) -> Result<[u8; 16]> {
        ensure!(
            share != [0; 12] && !reason.trim().is_empty() && reason.len() <= MAX_REPORT_REASON,
            Fault::Invalid
        );
        self.throttle(source, "reports", 10, 60)?;
        let id = self.environment.random()?;
        save(
            &mut self.store,
            &key("report", &id),
            &Report {
                id,
                share,
                reason,
                created_at: self.environment.now(),
            },
        )?;
        Ok(id)
    }
    /// # Errors
    /// Requires a live administrator session and readable persistent records.
    pub fn console(&self, session: &str) -> Result<serde_json::Value> {
        self.require_admin(session)?;
        let read = |prefix| -> Result<Vec<serde_json::Value>> {
            self.store
                .scan(prefix, Some(MAX_CONSOLE_ROWS))?
                .into_iter()
                .map(|(_, value)| Ok(serde_json::from_str(&value)?))
                .collect()
        };
        Ok(
            serde_json::json!({"settings":self.settings()?,"account_count":load::<u64>(&self.store,"account-count")?.unwrap_or(0),
            "reviews":read("review/")?,"nodes":read("node/")?,"load":self.store.scan("node-load/", Some(MAX_CONSOLE_ROWS))?.into_iter().map(|(key,value)| {
                let (created_at, load): (u64, windshare_relay_access::usage::Load) = serde_json::from_str(&value)?;
                let node=key.trim_start_matches("node-load/");
                let reported_at=crate::store::load::<u64>(&self.store,&format!("node-report/{node}"))?;
                Ok(serde_json::json!({"node":node,"created_at":created_at,"reported_at":reported_at,"load":load}))
            }).collect::<Result<Vec<_>>>()?,"reports":self.store.recent_reports(MAX_CONSOLE_ROWS)?,
            "restrictions":self.blocks()?}),
        )
    }
}
