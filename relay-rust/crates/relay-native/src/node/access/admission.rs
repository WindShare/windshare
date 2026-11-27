use super::{Service, State, devices::CredentialEntry};
use crate::connection::AdmissionPriority;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use std::sync::Arc;
use windshare_relay_access::{
    Attribution, Digest,
    entitlement::{Allowance, Class},
    ticket::{MAX_TICKET_BYTES, Ticket, VerificationError, VerifiedTicket},
    trust::Error,
};
use windshare_relay_core::{access::RegistrationDecision, identifiers::ShareId};

const ENCODED_SIGNATURE_BYTES: usize = (ed25519_dalek::SIGNATURE_LENGTH * 4).div_ceil(3);
const MAX_ENCODED_TICKET_BYTES: usize = (MAX_TICKET_BYTES * 4).div_ceil(3);

#[derive(Clone)]
pub struct Bearer {
    digest: Digest,
    ticket: TicketEvidence,
}

#[derive(Clone)]
enum TicketEvidence {
    Absent,
    Unverified(Arc<str>),
    Verified(Arc<VerifiedTicket>),
}

impl Bearer {
    pub(crate) fn new(token: &str) -> Self {
        let ticket =
            if (ENCODED_SIGNATURE_BYTES + 1..=MAX_ENCODED_TICKET_BYTES).contains(&token.len()) {
                TicketEvidence::Unverified(token.into())
            } else {
                TicketEvidence::Absent
            };
        Self {
            digest: Digest::of(token.as_bytes()),
            ticket,
        }
    }

    fn ticket_proof(
        &self,
        access: &Service,
        now: u64,
    ) -> Result<Arc<VerifiedTicket>, VerificationError> {
        match &self.ticket {
            TicketEvidence::Absent => Err(Error::Encoding.into()),
            TicketEvidence::Verified(ticket) => Ok(Arc::clone(ticket)),
            TicketEvidence::Unverified(encoded) => {
                let signed = URL_SAFE_NO_PAD
                    .decode(encoded.as_bytes())
                    .map_err(|_| Error::Encoding)?;
                VerifiedTicket::verify_with(&signed, now, |issuer| {
                    let state = access.read();
                    if state.active_issuers.contains(&issuer) {
                        state.authorities.trusted.generation(issuer)
                    } else {
                        None
                    }
                })
                .map(Arc::new)
            }
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RefusalReason {
    ConnectionMissing,
    AnonymousDisabled,
    CredentialInactive,
    TicketEncoding,
    TicketLifetime,
    TicketSignature,
    TicketExpired,
    TicketUntrusted,
    TicketGenerationMismatch,
    NodePublicDisabled,
    NodeDraining,
    IssuerInactive,
    ShareRestricted,
    AccountRestricted,
    CredentialRestricted,
    PublicDailyBudgetExhausted,
    AttributionMismatch,
    PrincipalShareLimit,
    ResidentShareCapacity,
}

impl RefusalReason {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::ConnectionMissing => "connection_missing",
            Self::AnonymousDisabled => "anonymous_disabled",
            Self::CredentialInactive => "credential_inactive",
            Self::TicketEncoding => "ticket_encoding",
            Self::TicketLifetime => "ticket_lifetime",
            Self::TicketSignature => "ticket_signature",
            Self::TicketExpired => "ticket_expired",
            Self::TicketUntrusted => "ticket_untrusted",
            Self::TicketGenerationMismatch => "ticket_generation_mismatch",
            Self::NodeDraining => "node_draining",
            Self::NodePublicDisabled => "node_public_disabled",
            Self::IssuerInactive => "issuer_inactive",
            Self::ShareRestricted => "share_restricted",
            Self::AccountRestricted => "account_restricted",
            Self::CredentialRestricted => "credential_restricted",
            Self::PublicDailyBudgetExhausted => "public_daily_budget_exhausted",
            Self::AttributionMismatch => "attribution_mismatch",
            Self::PrincipalShareLimit => "principal_share_limit",
            Self::ResidentShareCapacity => "resident_share_capacity",
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub struct Refusal {
    pub decision: RegistrationDecision,
    pub reason: RefusalReason,
}

impl Refusal {
    pub const fn denied(reason: RefusalReason) -> Self {
        Self {
            decision: RegistrationDecision::Denied,
            reason,
        }
    }

    pub const fn unavailable(reason: RefusalReason) -> Self {
        Self {
            decision: RegistrationDecision::SERVICE_UNAVAILABLE,
            reason,
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub enum Authorization {
    Granted {
        attribution: Attribution,
        class: Class,
        allowance: Option<Allowance>,
    },
    Refused(Refusal),
}

enum AdmissionRefusal {
    Denied(RefusalReason),
    Ticket(VerificationError),
}

enum AuthorizedIdentity<'a> {
    Anonymous,
    Local {
        attribution: Attribution,
        entry: &'a CredentialEntry,
    },
    Ticket(Ticket),
}

impl AuthorizedIdentity<'_> {
    const fn attribution(&self) -> Attribution {
        match self {
            Self::Anonymous => Attribution::Anonymous,
            Self::Local { attribution, .. } => *attribution,
            Self::Ticket(ticket) => ticket.attribution(),
        }
    }
}

impl Service {
    pub(crate) fn admission_priority(
        &self,
        share: ShareId,
        bearer: Option<&mut Bearer>,
        now: u64,
    ) -> AdmissionPriority {
        let state = self.read();
        if let Some(priority) = state.known_priority(share, bearer.as_deref()) {
            return priority;
        }
        drop(state);
        let Some(bearer) = bearer else {
            return AdmissionPriority::Anonymous;
        };
        let Ok(proof) = bearer.ticket_proof(self, now) else {
            return AdmissionPriority::Anonymous;
        };

        let state = self.read();
        let priority = state
            .known_priority(share, Some(bearer))
            .unwrap_or_else(|| {
                state
                    .authorized_identity(share, Some(bearer), Some(Ok(proof.as_ref())), now)
                    .map_or(AdmissionPriority::Anonymous, |identity| {
                        state.priority_for(identity.attribution())
                    })
            });
        drop(state);
        // Keep proof of the signature on this connection. Policy is checked again at REGISTER.
        bearer.ticket = TicketEvidence::Verified(proof);
        priority
    }

    pub(crate) fn egress_owner(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        forwarded: Option<Attribution>,
    ) -> Option<bool> {
        let state = self.read();
        // Receivers normally present no owner key. Once a share is registered,
        // its sender's attribution governs both directions and their controls.
        // An explicit bearer that does not match an active local credential
        // (such as an official account ticket) cannot represent an owner share.
        let attribution = forwarded
            .or_else(|| {
                state
                    .residents
                    .get(&share)
                    .map(|(attribution, _)| *attribution)
            })
            .or_else(|| {
                let entry = state.credentials.get(bearer?.digest)?;
                state
                    .local_attribution(entry)
                    .filter(|value| !state.denied(share, *value))
            });
        let owner = attribution
            .map(|value| state.owner(value))
            .or_else(|| bearer.map(|_| false));
        drop(state);
        owner
    }

    pub(crate) fn authorize(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        now: u64,
    ) -> Authorization {
        // A previously untrusted signing generation may have become available since upgrade.
        let proof = bearer.map(|bearer| bearer.ticket_proof(self, now));
        let proof = proof
            .as_ref()
            .map(|result| result.as_deref().map_err(|error| *error));
        let mut state = self.write();
        if state.admission() == super::NodeAdmission::Draining {
            return Authorization::Refused(Refusal::unavailable(RefusalReason::NodeDraining));
        }
        let identity = match state.authorized_identity(share, bearer, proof, now) {
            Ok(identity) => identity,
            Err(AdmissionRefusal::Denied(reason)) => {
                return Authorization::Refused(Refusal::denied(reason));
            }
            Err(AdmissionRefusal::Ticket(error)) => {
                return Authorization::Refused(state.ticket_refusal(share, error));
            }
        };
        let attribution = identity.attribution();
        match identity {
            AuthorizedIdentity::Anonymous => {}
            AuthorizedIdentity::Local { entry, .. } => entry.record_use(now.saturating_mul(1000)),
            AuthorizedIdentity::Ticket(ticket) => {
                state.quotas.advance(
                    (ticket.issuer, ticket.account),
                    super::Quota::from_snapshot(ticket.entitlement, now),
                    now,
                );
            }
        }
        let (class, allowance) = state.class(attribution, now);
        drop(state);
        Authorization::Granted {
            attribution,
            class,
            allowance,
        }
    }
}

impl State {
    fn known_priority(&self, share: ShareId, bearer: Option<&Bearer>) -> Option<AdmissionPriority> {
        let resident = self
            .residents
            .get(&share)
            .map(|(attribution, _)| *attribution)
            .filter(|attribution| {
                *attribution != Attribution::Anonymous && !self.denied(share, *attribution)
            });
        let local = bearer.and_then(|bearer| self.credentials.get(bearer.digest));
        let local_attribution = local.and_then(|entry| self.local_attribution(entry));
        if resident.is_some_and(|attribution| self.owner(attribution))
            || local_attribution.is_some_and(|attribution| self.owner(attribution))
        {
            Some(AdmissionPriority::Owner)
        } else if resident.is_some()
            || local_attribution.is_some_and(|attribution| !self.denied(share, attribution))
        {
            Some(AdmissionPriority::Authenticated)
        } else {
            local.map(|_| AdmissionPriority::Anonymous)
        }
    }

    fn authorized_identity(
        &self,
        share: ShareId,
        bearer: Option<&Bearer>,
        proof: Option<Result<&VerifiedTicket, VerificationError>>,
        now: u64,
    ) -> Result<AuthorizedIdentity<'_>, AdmissionRefusal> {
        let identity = match bearer {
            None if self.anonymous() => AuthorizedIdentity::Anonymous,
            None => return Err(AdmissionRefusal::Denied(RefusalReason::AnonymousDisabled)),
            Some(bearer) => {
                if let Some(entry) = self.credentials.get(bearer.digest) {
                    AuthorizedIdentity::Local {
                        attribution: self
                            .local_attribution(entry)
                            .ok_or(AdmissionRefusal::Denied(RefusalReason::CredentialInactive))?,
                        entry,
                    }
                } else {
                    let ticket = proof
                        .ok_or(AdmissionRefusal::Denied(RefusalReason::TicketEncoding))?
                        .and_then(|proof| proof.current(&self.authorities.trusted, now))
                        .map_err(AdmissionRefusal::Ticket)?;
                    AuthorizedIdentity::Ticket(ticket)
                }
            }
        };
        if let Some(reason) = self.denial_reason(share, identity.attribution()) {
            return Err(AdmissionRefusal::Denied(reason));
        }
        Ok(identity)
    }

    fn ticket_refusal(&self, share: ShareId, error: VerificationError) -> Refusal {
        let VerificationError::GenerationMismatch {
            issuer,
            presented_generation,
            installed_generation,
        } = error
        else {
            let VerificationError::Invalid(error) = error else {
                unreachable!("non-mismatch verification error must be Invalid");
            };
            return Refusal::denied(match error {
                Error::Encoding => RefusalReason::TicketEncoding,
                Error::Lifetime => RefusalReason::TicketLifetime,
                Error::Signature => RefusalReason::TicketSignature,
                Error::Expired => RefusalReason::TicketExpired,
                Error::Untrusted | Error::Rollback => RefusalReason::TicketUntrusted,
            });
        };
        if !self.active_issuers.contains(&issuer) {
            return Refusal::denied(RefusalReason::IssuerInactive);
        }
        if matches!(
            self.admission(),
            super::NodeAdmission::AuthorityDisabled | super::NodeAdmission::ContributionDisabled
        ) {
            return Refusal::denied(RefusalReason::NodePublicDisabled);
        }
        if self.share_restricted(share) {
            return Refusal::denied(RefusalReason::ShareRestricted);
        }

        let direction = if presented_generation > installed_generation {
            "ticket_ahead"
        } else {
            "ticket_behind"
        };
        tracing::debug!(
            event = "ticket_generation_mismatch",
            ?share,
            ?issuer,
            presented_generation,
            installed_generation,
            direction,
        );
        Refusal::unavailable(RefusalReason::TicketGenerationMismatch)
    }

    fn local_attribution(&self, entry: &CredentialEntry) -> Option<Attribution> {
        let key = &entry.value;
        let attribution = key.active_attribution()?;
        Some(
            self.credentials
                .account(key.id)
                .map_or(attribution, |binding| Attribution::Account {
                    issuer: binding.issuer,
                    account: binding.account,
                    credential: key.id,
                }),
        )
    }
}
