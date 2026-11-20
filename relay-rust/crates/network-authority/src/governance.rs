use crate::{
    Authority, Environment, Fault, RegistrationMode, RegistrationPolicy, Settings, SettingsChanges,
    Store,
    store::{key, save},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use windshare_relay_access::restrictions::Restriction;

const MAX_REPORT_REASON: usize = 2000;
const MAX_CONSOLE_REVIEWS: usize = 1000;
const REPORT_PAGE_SIZE: usize = 50;
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

#[derive(Clone, Copy)]
pub struct ReportPosition {
    pub created_at: u64,
    pub id: [u8; 16],
}

impl From<&Report> for ReportPosition {
    fn from(report: &Report) -> Self {
        Self {
            created_at: report.created_at,
            id: report.id,
        }
    }
}

pub struct ReportPage {
    pub reports: Vec<Report>,
    pub next: Option<ReportPosition>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct MilestoneReview {
    pub count: u64,
    pub reached_at: u64,
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Requires a live administrator session and a valid registration policy.
    pub fn update_settings(
        &mut self,
        session: &str,
        changes: &SettingsChanges,
    ) -> Result<(Settings, RegistrationMode)> {
        self.require_admin(session)?;
        let current = self.settings()?;
        let settings = changes.apply_to(&current);
        let email_configured = self.environment.registration_email_configured();
        ensure!(
            (16..=30).contains(&settings.pow_bits)
                && settings.daily_bytes > 0
                && settings.source_rate.burst > 0
                && settings.source_rate.refill_seconds > 0
                && settings.global_rate.burst > 0
                && settings.global_rate.refill_seconds > 0
                && (settings.registration != RegistrationPolicy::Email || email_configured),
            Fault::Invalid
        );
        let account_count = self.account_count()?;
        let mode = settings.registration.mode(account_count, email_configured);
        if settings != current {
            save(&mut self.store, "settings", &settings)?;
            tracing::info!(
                event = "authority_policy_changed",
                ?changes,
                policy = ?settings.registration,
                ?mode,
                account_count,
                pow_bits = settings.pow_bits
            );
        }
        Ok((settings, mode))
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
    /// Requires a live administrator session and readable report records.
    pub fn reports(&self, session: &str, after: Option<ReportPosition>) -> Result<ReportPage> {
        self.require_admin(session)?;
        let mut reports = self.store.reports_after(after, REPORT_PAGE_SIZE + 1)?;
        let has_more = reports.len() > REPORT_PAGE_SIZE;
        reports.truncate(REPORT_PAGE_SIZE);
        let next = reports
            .last()
            .filter(|_| has_more)
            .map(ReportPosition::from);
        tracing::debug!(
            event = "authority_reports_listed",
            continued = after.is_some(),
            count = reports.len(),
            has_more
        );
        Ok(ReportPage { reports, next })
    }

    /// # Errors
    /// Requires a live administrator session and an existing report.
    pub fn report_by_id(&self, session: &str, id: [u8; 16]) -> Result<Report> {
        self.require_admin(session)?;
        let record = key("report", &id);
        let report = crate::store::load(&self.store, &record)?;
        tracing::debug!(event = "authority_report_read", report_key = %record, found = report.is_some());
        report.ok_or_else(|| Fault::ReportNotFound.into())
    }

    fn reviews(&self) -> Result<Vec<MilestoneReview>> {
        let mut reviews = self
            .store
            .scan("review/", Some(MAX_CONSOLE_REVIEWS))?
            .into_iter()
            .filter_map(|(record, value)| {
                let count = record.strip_prefix("review/")?.parse::<u64>().ok()?;
                Some(
                    serde_json::from_str::<u64>(&value)
                        .map(|reached_at| MilestoneReview { count, reached_at }),
                )
            })
            .collect::<std::result::Result<Vec<_>, _>>()?;
        reviews.sort_unstable_by_key(|review| review.count);
        Ok(reviews)
    }

    /// Marks a registration milestone review as acknowledged and returns the remaining ones.
    ///
    /// # Errors
    /// Requires a live administrator session and a positive milestone count.
    pub fn dismiss_review(&mut self, session: &str, count: u64) -> Result<Vec<MilestoneReview>> {
        self.require_admin(session)?;
        ensure!(count > 0, Fault::Invalid);
        self.store.delete(&format!("review/{count}"))?;
        tracing::info!(event = "authority_milestone_review_dismissed", count);
        self.reviews()
    }

    /// # Errors
    /// Requires a live administrator session and readable persistent records.
    pub fn console(&self, session: &str) -> Result<serde_json::Value> {
        self.require_admin(session)?;
        let reviews = self.reviews()?;
        let settings = self.settings()?;
        let account_count = self.account_count()?;
        let email_configured = self.environment.registration_email_configured();
        let registration_mode = settings.registration.mode(account_count, email_configured);
        Ok(
            serde_json::json!({"settings":settings,"account_count":account_count,
            "registration_mode":registration_mode,"email_configured":email_configured,
            "reviews":reviews,"node_page":self.node_page(None)?,
            "restrictions":self.blocks()?}),
        )
    }
}
